package shell

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"scoutclaw/internal/policy"
	"scoutclaw/internal/tools"
)

// TestHelperProcess is not a real test: background tests start the test
// binary itself in one of these modes, so the child behaves the same on every OS.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("SCOUT_HELPER") != "1" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	switch mode {
	case "serve":
		fmt.Println("booting")
		time.Sleep(200 * time.Millisecond)
		fmt.Println("ready on :1234")
		for i := 1; ; i++ {
			time.Sleep(100 * time.Millisecond)
			fmt.Printf("tick %d\n", i)
		}
	case "exit3":
		fmt.Println("bad config")
		os.Exit(3)
	case "spam":
		fmt.Print(strings.Repeat("x", 64*1024) + "\nend-of-spam\n")
		time.Sleep(time.Hour)
	case "env":
		fmt.Printf("secret=%q\n", os.Getenv("SCOUT_TEST_SECRET"))
		time.Sleep(time.Hour)
	case "spawn":
		child := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "--", "serve")
		if err := child.Start(); err != nil {
			fmt.Println("spawn failed:", err)
			os.Exit(1)
		}
		fmt.Printf("child %d\n", child.Process.Pid)
		time.Sleep(time.Hour)
	}
	os.Exit(0)
}

func helperArgs(mode string) map[string]any {
	return map[string]any{"command": os.Args[0], "args": []string{"-test.run=^TestHelperProcess$", "--", mode}}
}

func newBackground(t *testing.T) (*Background, map[string]tools.Tool) {
	t.Helper()
	t.Setenv("SCOUT_HELPER", "1")
	b := &Background{Runner: Runner{Scope: policy.NewScope(t.TempDir()), MaxOutput: 4096, DefaultTimeout: 10 * time.Second}}
	t.Cleanup(func() { b.Shutdown() })
	m := map[string]tools.Tool{}
	for _, tl := range b.Tools() {
		m[tl.Name()] = tl
	}
	return b, m
}

func bgCall(t *testing.T, tl tools.Tool, in map[string]any) (tools.Result, error) {
	t.Helper()
	raw, _ := json.Marshal(in)
	return tl.Execute(context.Background(), raw)
}

func with(base map[string]any, kv ...any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

func TestRing(t *testing.T) {
	tests := []struct {
		name        string
		cap         int
		writes      []string
		since       int64
		want        string
		wantDropped int64
	}{
		{"everything fits", 10, []string{"abc", "def"}, 0, "abcdef", 0},
		{"read from a position", 10, []string{"abc", "def"}, 3, "def", 0},
		{"old bytes are dropped", 4, []string{"abc", "def"}, 0, "cdef", 2},
		{"position inside the kept window", 4, []string{"abc", "def"}, 4, "ef", 0},
		{"nothing new", 10, []string{"abc"}, 3, "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := ring{cap: tt.cap}
			for _, w := range tt.writes {
				r.write([]byte(w))
			}
			got, dropped := r.since(tt.since)
			if got != tt.want || dropped != tt.wantDropped {
				t.Fatalf("since(%d) = %q, %d; want %q, %d", tt.since, got, dropped, tt.want, tt.wantDropped)
			}
		})
	}
}

func TestProcLifecycle(t *testing.T) {
	b, m := newBackground(t)
	r, err := bgCall(t, m["proc_start"], with(helperArgs("serve"), "name", "web", "wait_for", "ready on", "wait_sec", 20))
	if err != nil {
		t.Fatal(err)
	}
	id := r.Metadata["id"]
	if id != "p1" || r.Mutated || !strings.Contains(r.Output, "ready on :1234") || !strings.Contains(r.Output, "status: running") || strings.Contains(r.Output, "did not appear") {
		t.Fatalf("start: %+v", r)
	}
	if b.Running() != 1 || !strings.Contains(b.List(), "p1 [web]  running") {
		t.Fatalf("list: %q", b.List())
	}

	r, _ = bgCall(t, m["proc_output"], map[string]any{"id": id, "wait_for": "tick 2", "wait_sec": 20})
	if !strings.Contains(r.Output, "tick 2") || strings.Contains(r.Output, "booting") {
		t.Fatalf("output should only show what is new: %q", r.Output)
	}
	r, _ = bgCall(t, m["proc_output"], map[string]any{"id": id, "all": true})
	if !strings.Contains(r.Output, "booting") {
		t.Fatalf("all=true should show everything buffered: %q", r.Output)
	}

	r, err = bgCall(t, m["proc_stop"], map[string]any{"id": id})
	if err != nil || !strings.Contains(r.Output, "stopped p1") || b.Running() != 0 {
		t.Fatalf("stop: %v %q", err, r.Output)
	}
	r, _ = bgCall(t, m["proc_stop"], map[string]any{"id": id})
	if !strings.Contains(r.Output, "already exited") {
		t.Fatalf("second stop: %q", r.Output)
	}
	if !strings.Contains(b.List(), "p1 [web]  exited") {
		t.Fatalf("list after stop: %q", b.List())
	}
}

func TestProcStartReportsEarlyExit(t *testing.T) {
	_, m := newBackground(t)
	r, err := bgCall(t, m["proc_start"], with(helperArgs("exit3"), "wait_sec", 20))
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != tools.KindFailed || r.ExitCode != 3 || !strings.Contains(r.Output, "bad config") || !strings.Contains(r.Output, "exited with code 3") {
		t.Fatalf("early exit should be reported as a failure: %+v", r)
	}
}

func TestProcWaitForTimesOutWithANote(t *testing.T) {
	_, m := newBackground(t)
	start := time.Now()
	r, err := bgCall(t, m["proc_start"], with(helperArgs("serve"), "wait_for", "never-printed", "wait_sec", 1))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Output, `"never-printed" did not appear within 1s`) || time.Since(start) > 8*time.Second {
		t.Fatalf("output: %q after %s", r.Output, time.Since(start))
	}
}

func TestProcOutputReportsDroppedBytes(t *testing.T) {
	b, m := newBackground(t)
	b.BufferSize = 1024
	r, err := bgCall(t, m["proc_start"], with(helperArgs("spam"), "wait_for", "end-of-spam", "wait_sec", 20))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Output, "earlier bytes no longer buffered") || !strings.Contains(r.Output, "end-of-spam") || len(r.Output) > 2048 {
		t.Fatalf("buffer should keep only the tail: %d bytes", len(r.Output))
	}
}

func TestProcChildEnvIsStripped(t *testing.T) {
	b, m := newBackground(t)
	t.Setenv("SCOUT_TEST_SECRET", "hunter2")
	b.Runner.StripEnv = []string{"SCOUT_TEST_SECRET"}
	r, err := bgCall(t, m["proc_start"], with(helperArgs("env"), "wait_for", "secret=", "wait_sec", 20))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Output, `secret=""`) {
		t.Fatalf("stripped variable leaked into the background process: %q", r.Output)
	}
}

func TestProcLimitAndShutdown(t *testing.T) {
	b, m := newBackground(t)
	b.MaxRunning = 2
	for i := 0; i < 2; i++ {
		if _, err := bgCall(t, m["proc_start"], with(helperArgs("serve"), "wait_sec", 1)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := bgCall(t, m["proc_start"], with(helperArgs("serve"), "wait_sec", 1)); tools.KindOf(err) != tools.KindDenied {
		t.Fatalf("third process must be refused: %v", err)
	}
	if n := b.Shutdown(); n != 2 || b.Running() != 0 {
		t.Fatalf("shutdown stopped %d, %d still running", n, b.Running())
	}
	if n := b.Shutdown(); n != 0 {
		t.Fatalf("second shutdown stopped %d", n)
	}
}

func TestProcErrorsAndValidation(t *testing.T) {
	_, m := newBackground(t)
	for _, bad := range []struct {
		tool, in string
	}{
		{"proc_start", `{"command":""}`},
		{"proc_start", `{"command":"x","wait_for":"("}`},
		{"proc_start", `{"command":"x","wait_sec":500}`},
		{"proc_start", `{"command":"x","wait_sec":-1}`},
		{"proc_output", `{"id":""}`},
		{"proc_stop", `{}`},
		{"proc_list", `{"extra":1}`},
	} {
		if m[bad.tool].Validate(json.RawMessage(bad.in)) == nil {
			t.Errorf("%s %s must be rejected", bad.tool, bad.in)
		}
	}
	if _, err := bgCall(t, m["proc_output"], map[string]any{"id": "p99"}); tools.KindOf(err) != tools.KindNotFound {
		t.Fatalf("unknown id: %v", err)
	}
	if _, err := bgCall(t, m["proc_start"], map[string]any{"command": "definitely-not-a-real-program-xyz"}); tools.KindOf(err) != tools.KindUnavailable {
		t.Fatalf("missing executable: %v", err)
	}
	if _, err := bgCall(t, m["proc_start"], with(helperArgs("serve"), "workdir", "no/such/dir")); tools.KindOf(err) != tools.KindInvalidInput {
		t.Fatalf("bad workdir: %v", err)
	}
	if r, _ := bgCall(t, m["proc_list"], map[string]any{}); r.Output != "no background processes\n" {
		t.Fatalf("empty list: %q", r.Output)
	}
}
