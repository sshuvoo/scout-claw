package filesystem

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestCheckFresh(t *testing.T) {
	s := NewState("")
	if err := s.checkFresh("/a", "x"); err == nil || !strings.Contains(err.Error(), "has not been read") {
		t.Fatalf("unread: %v", err)
	}
	s.sawContent("/a", "x")
	if err := s.checkFresh("/a", "x"); err != nil {
		t.Fatalf("fresh: %v", err)
	}
	if err := s.checkFresh("/a", "y"); err == nil || !strings.Contains(err.Error(), "changed on disk") {
		t.Fatalf("stale: %v", err)
	}
}

func TestReadRecordsFollowMovesAndCopies(t *testing.T) {
	sep := string(filepath.Separator)
	dir, file, other := sep+"d", sep+"d"+sep+"f", sep+"dx"+sep+"g"
	tests := []struct {
		name     string
		act      func(s *State)
		fresh    []string // paths that must still count as read
		notFresh []string
	}{
		{"move file", func(s *State) { s.moved(file, sep+"n") }, []string{sep + "n", other}, []string{file}},
		{"move directory", func(s *State) { s.moved(dir, sep+"e") }, []string{sep + "e" + sep + "f", other}, []string{file}},
		{"copy keeps source", func(s *State) { s.copied(dir, sep+"c") }, []string{file, sep + "c" + sep + "f"}, nil},
		{"forget directory", func(s *State) { s.forget(dir) }, []string{other}, []string{file}},
		{"sibling prefix is not a child", func(s *State) { s.forget(sep + "d") }, []string{other}, []string{file}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewState("")
			s.sawContent(file, "x")
			s.sawContent(other, "x")
			tt.act(s)
			for _, p := range tt.fresh {
				if err := s.checkFresh(p, "x"); err != nil {
					t.Errorf("%s should be read: %v", p, err)
				}
			}
			for _, p := range tt.notFresh {
				if s.checkFresh(p, "x") == nil {
					t.Errorf("%s should not be read", p)
				}
			}
		})
	}
}

func TestChangeTouches(t *testing.T) {
	sep := string(filepath.Separator)
	c := Change{Path: sep + "a" + sep + "b.go", Dest: sep + "z" + sep + "y"}
	tests := []struct {
		path string
		want bool
	}{
		{sep + "a" + sep + "b.go", true},
		{sep + "a", true},
		{sep + "z", true},
		{sep + "z" + sep + "y", true},
		{sep + "a" + sep + "b", false},
		{sep + "ab", false},
		{sep + "q", false},
	}
	for _, tt := range tests {
		if got := c.touches(tt.path); got != tt.want {
			t.Errorf("touches(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestJournalPersistsAndAppliesUndoMarks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j", "s.jsonl")
	s := NewState(path)
	a := s.record(Change{Op: "write", Path: "/a"})
	b := s.record(Change{Op: "patch", Path: "/b"})
	if a.Seq != 1 || b.Seq != 2 || a.Time.IsZero() {
		t.Fatalf("sequence/time not assigned: %+v %+v", a, b)
	}
	s.record(Change{Op: "undo", Ref: b.Seq, Path: "/b"})
	if got := s.Changes(); len(got) != 1 || got[0].Path != "/a" {
		t.Fatalf("live changes = %+v", got)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("journal not written: %v", err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("journal mode = %v, want 0600", st.Mode().Perm())
	}

	reloaded := NewState(path)
	if got := reloaded.Changes(); len(got) != 1 || got[0].Seq != 1 {
		t.Fatalf("reloaded live changes = %+v", got)
	}
	if c := reloaded.record(Change{Op: "mkdir", Path: "/m"}); c.Seq != 4 {
		t.Fatalf("sequence should continue after reload, got %d", c.Seq)
	}
	if NewState(filepath.Join(t.TempDir(), "absent.jsonl")).Changes() != nil {
		t.Fatal("a missing journal starts empty")
	}
}

func TestPendingNewestFirstAndFiltered(t *testing.T) {
	s := NewState("")
	for _, p := range []string{"/a", "/b", "/a", "/c"} {
		s.record(Change{Op: "write", Path: p})
	}
	tests := []struct {
		steps int
		path  string
		want  []int
	}{
		{1, "", []int{4}},
		{3, "", []int{4, 3, 2}},
		{10, "/a", []int{3, 1}},
		{1, "/a", []int{3}},
		{5, "/none", nil},
	}
	for _, tt := range tests {
		var got []int
		for _, c := range s.pending(tt.steps, tt.path) {
			got = append(got, c.Seq)
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("pending(%d, %q) = %v, want %v", tt.steps, tt.path, got, tt.want)
		}
	}
}
