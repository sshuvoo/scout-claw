package llm

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveOpenAICompaction talks to the real API. It runs only when
// SCOUT_LIVE_OPENAI=1 and OPENAI_API_KEY are set, and costs a few cents.
// It checks that the endpoint accepts context_management, that compaction
// triggers past the threshold, and that a fact from before the compaction
// survives it when the conversation is chained with previous_response_id.
func TestLiveOpenAICompaction(t *testing.T) {
	if os.Getenv("SCOUT_LIVE_OPENAI") != "1" || os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("set SCOUT_LIVE_OPENAI=1 and OPENAI_API_KEY to run")
	}
	model := os.Getenv("SCOUTCLAW_MODEL")
	if model == "" {
		model = "gpt-5.5"
	}
	o, err := NewOpenAI(os.Getenv("OPENAI_API_KEY"), os.Getenv("OPENAI_BASE_URL"), model)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	const instructions = "You are a test assistant. Reply in at most five words."
	filler := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 1200) // roughly 12k tokens

	prev := ""
	say := func(text string) *Response {
		t.Helper()
		resp, err := o.Generate(ctx, Request{Instructions: instructions, UserText: text, PreviousID: prev, CompactAt: 20000})
		if err != nil {
			t.Fatalf("%v", err)
		}
		prev = resp.ID
		t.Logf("input_tokens=%d compacted=%v reply=%q", resp.Usage.InputTokens, resp.Compacted, resp.Text)
		return resp
	}
	say("Remember this code word for later: PELICAN-42. Reply OK.")
	compacted := false
	for turn := 1; turn <= 4 && !compacted; turn++ {
		compacted = say(fmt.Sprintf("Filler %d, just reply OK:\n%s", turn, filler)).Compacted
	}
	if !o.CompactionSupported() {
		t.Fatal("the endpoint rejected context_management; compaction falls back to truncation only")
	}
	if !compacted {
		t.Fatal("compaction did not trigger past the threshold")
	}
	if reply := say("What was the code word I asked you to remember?"); !strings.Contains(strings.ToUpper(reply.Text), "PELICAN-42") {
		t.Fatalf("the code word did not survive compaction: %q", reply.Text)
	}
}
