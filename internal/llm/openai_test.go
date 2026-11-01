package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeResponses is a minimal Responses API server: it records each request
// body and streams a completed response, optionally containing a compaction item.
type fakeResponses struct {
	mu        sync.Mutex
	bodies    []map[string]any
	compacted bool
	reject    bool // answer 400 to requests that carry context_management
}

func (f *fakeResponses) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.bodies = append(f.bodies, body)
	f.mu.Unlock()
	if _, has := body["context_management"]; has && f.reject {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"Unknown parameter: 'context_management'.","type":"invalid_request_error","param":"context_management","code":"unknown_parameter"}}`)
		return
	}
	output := `{"type":"message","id":"m1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello","annotations":[]}]}`
	if f.compacted {
		output = `{"type":"compaction","id":"c1","encrypted_content":"opaque"},` + output
	}
	resp := `{"id":"resp_1","object":"response","created_at":0,"status":"completed","model":"m","output":[` + output + `],` +
		`"usage":{"input_tokens":1234,"output_tokens":5,"total_tokens":1239,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}},` +
		`"parallel_tool_calls":true,"tool_choice":"auto","tools":[]}`
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\",\"sequence_number\":1}\n\n")
	fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":%s}\n\n", resp)
}

func (f *fakeResponses) body(i int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[i]
}

func newFakeOpenAI(t *testing.T, f *fakeResponses) *OpenAI {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	o, err := NewOpenAI("sk-test", srv.URL, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestOpenAISendsCompactionAndDetectsIt(t *testing.T) {
	f := &fakeResponses{compacted: true}
	o := newFakeOpenAI(t, f)
	var streamed strings.Builder
	resp, err := o.Generate(context.Background(), Request{UserText: "hi", CompactAt: 150000, OnDelta: func(s string) { streamed.WriteString(s) }})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Compacted || resp.Text != "hello" || resp.Usage.InputTokens != 1234 || streamed.String() != "hello" {
		t.Fatalf("response: %+v streamed=%q", resp, streamed.String())
	}
	cm, ok := f.body(0)["context_management"].([]any)
	if !ok || len(cm) != 1 {
		t.Fatalf("context_management not sent: %v", f.body(0))
	}
	entry := cm[0].(map[string]any)
	if entry["type"] != "compaction" || entry["compact_threshold"] != float64(150000) {
		t.Fatalf("context_management = %v", entry)
	}
	if f.body(0)["truncation"] != "auto" {
		t.Fatalf("truncation should stay as a last resort: %v", f.body(0)["truncation"])
	}
}

func TestOpenAIOmitsCompactionWhenOff(t *testing.T) {
	f := &fakeResponses{}
	o := newFakeOpenAI(t, f)
	resp, err := o.Generate(context.Background(), Request{UserText: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if _, has := f.body(0)["context_management"]; has || resp.Compacted {
		t.Fatalf("compaction must not be requested or reported: %v %+v", f.body(0), resp)
	}
}

func TestOpenAIFallsBackWhenServerRejectsCompaction(t *testing.T) {
	f := &fakeResponses{reject: true}
	o := newFakeOpenAI(t, f)
	for i := 0; i < 2; i++ {
		if _, err := o.Generate(context.Background(), Request{UserText: "hi", CompactAt: 150000}); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	// first request: rejected with the field, retried without it; second: sent without it
	if len(f.bodies) != 3 {
		t.Fatalf("want 3 HTTP requests, got %d", len(f.bodies))
	}
	if _, has := f.body(0)["context_management"]; !has {
		t.Fatal("first attempt should try compaction")
	}
	for _, i := range []int{1, 2} {
		if _, has := f.body(i)["context_management"]; has {
			t.Fatalf("request %d should no longer send context_management", i)
		}
	}
}

func TestRejectsCompaction(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{&Error{Kind: ErrBadRequest, Err: fmt.Errorf("Unknown parameter: 'context_management'.")}, true},
		{&Error{Kind: ErrBadRequest, Err: fmt.Errorf("compact_threshold must be positive")}, true},
		{&Error{Kind: ErrBadRequest, Err: fmt.Errorf("invalid model")}, false},
		{&Error{Kind: ErrTransient, Err: fmt.Errorf("context_management timeout")}, false},
		{fmt.Errorf("context_management"), false},
		{nil, false},
	}
	for _, tt := range tests {
		if got := rejectsCompaction(tt.err); got != tt.want {
			t.Errorf("rejectsCompaction(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}
