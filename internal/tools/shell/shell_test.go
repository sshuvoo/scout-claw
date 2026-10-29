package shell

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"scoutclaw/internal/policy"
	"scoutclaw/internal/tools"
)

func runner(t *testing.T) (Runner, tools.Tool) {
	r := Runner{Scope: policy.NewScope(t.TempDir()), MaxOutput: 2048, DefaultTimeout: 5 * time.Second}
	for _, tl := range r.Tools() {
		if tl.Name() == "shell_exec" {
			return r, tl
		}
	}
	t.Fatal("no shell_exec")
	return r, nil
}

func call(t *testing.T, tl tools.Tool, ctx context.Context, in map[string]any) (tools.Result, error) {
	b, _ := json.Marshal(in)
	return tl.Execute(ctx, b)
}

func TestExecCapturesOutputAndExitCodes(t *testing.T) {
	_, tl := runner(t)
	res, err := call(t, tl, context.Background(), map[string]any{"command": "sh", "args": []string{"-c", "echo out; echo err >&2"}})
	if err != nil || !res.OK() || !strings.Contains(res.Output, "out") || !strings.Contains(res.Output, "err") {
		t.Fatalf("%v %+v", err, res)
	}
	res, err = call(t, tl, context.Background(), map[string]any{"command": "sh", "args": []string{"-c", "echo boom; exit 3"}})
	if err != nil || res.ExitCode != 3 || res.Kind != tools.KindFailed || !strings.Contains(res.Output, "boom") {
		t.Fatalf("nonzero exit not reported: %v %+v", err, res)
	}
}

func TestExecMissingExecutable(t *testing.T) {
	_, tl := runner(t)
	_, err := call(t, tl, context.Background(), map[string]any{"command": "definitely-not-a-real-binary-xyz"})
	if tools.KindOf(err) != tools.KindUnavailable {
		t.Fatalf("want unavailable, got %v", err)
	}
}

func TestExecDoesNotInterpretShellSyntax(t *testing.T) {
	_, tl := runner(t)
	res, _ := call(t, tl, context.Background(), map[string]any{"command": "echo", "args": []string{"a; echo injected", "$HOME"}})
	if !strings.Contains(res.Output, "a; echo injected $HOME") {
		t.Fatalf("arguments must be passed literally: %q", res.Output)
	}
}

func TestExecTimeoutKillsProcessGroup(t *testing.T) {
	_, tl := runner(t)
	marker := filepath.Join(t.TempDir(), "child-survived")
	start := time.Now()
	// The grandchild would create the marker after 3s if it were not killed.
	res, err := call(t, tl, context.Background(), map[string]any{
		"command": "sh", "args": []string{"-c", "(sleep 3; touch " + marker + ") & sleep 30"}, "timeout_sec": 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != tools.KindTimeout {
		t.Fatalf("want timeout, got %+v", res)
	}
	if time.Since(start) > 8*time.Second {
		t.Fatalf("took too long: %s", time.Since(start))
	}
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("grandchild survived: process group was not terminated")
	}
}

func TestExecCancellation(t *testing.T) {
	_, tl := runner(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	start := time.Now()
	res, err := call(t, tl, ctx, map[string]any{"command": "sleep", "args": []string{"30"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != tools.KindCancelled || time.Since(start) > 5*time.Second {
		t.Fatalf("cancel not honoured: %+v after %s", res, time.Since(start))
	}
}

func TestOutputTruncationKeepsHeadAndTail(t *testing.T) {
	_, tl := runner(t)
	res, err := call(t, tl, context.Background(), map[string]any{"command": "sh", "args": []string{"-c", "echo HEAD; seq 1 20000; echo TAIL"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || len(res.Output) > 2048+100 {
		t.Fatalf("not truncated: %d bytes", len(res.Output))
	}
	if !strings.Contains(res.Output, "HEAD") || !strings.Contains(res.Output, "TAIL") || !strings.Contains(res.Output, "omitted") {
		t.Fatalf("head/tail lost: %q", res.Output[:min(80, len(res.Output))])
	}
}

func TestChildEnvStripsAPIKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-should-not-leak-1234567890")
	r, tl := runner(t)
	r.StripEnv = []string{"OPENAI_API_KEY"}
	for _, x := range r.Tools() {
		if x.Name() == "shell_exec" {
			tl = x
		}
	}
	res, _ := call(t, tl, context.Background(), map[string]any{"command": "env"})
	if strings.Contains(res.Output, "OPENAI_API_KEY") {
		t.Fatal("API key leaked into child environment")
	}
}

func TestValidation(t *testing.T) {
	_, tl := runner(t)
	for _, in := range []string{`{}`, `{"command":""}`, `{"command":"ls","purpose":"hack"}`, `{"command":"ls","timeout_sec":-1}`, `{"command":"ls","bogus":1}`} {
		if tl.Validate(json.RawMessage(in)) == nil {
			t.Errorf("%s should be invalid", in)
		}
	}
}
