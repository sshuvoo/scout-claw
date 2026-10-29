package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveLoadListLatest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	a := New(dir, "/ws", "m")
	a.Tasks = append(a.Tasks, TaskRecord{Prompt: "p1", Status: "completed"})
	a.ResponseID = "resp_1"
	a.Pending = []Pending{{CallID: "c1", Output: "o"}}
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	b := New(dir, "/ws", "m")
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}

	got, err := Load(dir, a.ID)
	if err != nil || got.ResponseID != "resp_1" || len(got.Tasks) != 1 || got.Pending[0].CallID != "c1" {
		t.Fatalf("%v %+v", err, got)
	}
	l, _ := List(dir)
	if len(l) != 2 || l[0].ID != b.ID {
		t.Fatalf("newest first expected: %v", l)
	}
	if last, _ := Latest(dir); last.ID != b.ID {
		t.Fatal("latest wrong")
	}
	if st, _ := os.Stat(filepath.Join(dir, a.ID+".json")); st.Mode().Perm() != 0o600 {
		t.Fatalf("session files must be private, got %v", st.Mode())
	}
}

func TestLoadRejectsTraversalAndCorruption(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(dir, "../x"); err == nil {
		t.Fatal("path traversal in id must be rejected")
	}
	os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{"), 0o600)
	if _, err := Load(dir, "bad"); err == nil {
		t.Fatal("corrupt file must error")
	}
	if l, err := List(filepath.Join(dir, "nope")); err != nil || l != nil {
		t.Fatalf("missing dir is an empty list: %v %v", l, err)
	}
}
