package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"scoutclaw/internal/redact"
)

func TestLogRedactsAndFilters(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "audit.jsonl")
	l, err := Open(p, redact.New("exact-secret-value"))
	if err != nil {
		t.Fatal(err)
	}
	l.Log(Event{SessionID: "s1", Kind: "tool_request", Tool: "shell_exec", Data: map[string]any{"input": "curl -H 'Authorization: Bearer abcdefghijklmnopqrstuvwx' exact-secret-value"}})
	l.Log(Event{SessionID: "s2", Kind: "task_start"})
	l.Close()

	raw, _ := os.ReadFile(p)
	if strings.Contains(string(raw), "abcdefghijklmnop") || strings.Contains(string(raw), "exact-secret-value") {
		t.Fatalf("secret on disk: %s", raw)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("audit log must be private: %v", st.Mode())
	}
	ev, err := Read(p, "s1")
	if err != nil || len(ev) != 1 || ev[0].Tool != "shell_exec" {
		t.Fatalf("%v %+v", err, ev)
	}
	if all, _ := Read(p, ""); len(all) != 2 {
		t.Fatalf("%d", len(all))
	}
}

func TestNilLoggerIsSafe(t *testing.T) {
	var l *Logger
	l.Log(Event{Kind: "x"})
	if l.Close() != nil {
		t.Fatal()
	}
}
