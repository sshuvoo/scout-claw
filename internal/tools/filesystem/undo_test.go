package filesystem

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"scoutclaw/internal/tools"
)

// TestUndoEachOperation does one operation, undoes it, and checks the
// workspace is back to where it started.
func TestUndoEachOperation(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, ws string, m map[string]tools.Tool)
		op    func(t *testing.T, m map[string]tools.Tool) (tools.Result, error)
		check func(t *testing.T, ws string)
	}{
		{
			name: "write new file",
			op: func(t *testing.T, m map[string]tools.Tool) (tools.Result, error) {
				return run(t, m["fs_write"], ctx0, map[string]any{"path": "n.txt", "content": "x"})
			},
			check: func(t *testing.T, ws string) { assertGone(t, ws, "n.txt") },
		},
		{
			name: "overwrite existing file",
			setup: func(t *testing.T, ws string, m map[string]tools.Tool) {
				mustWrite(t, filepath.Join(ws, "o.txt"), "orig\r\n")
				readFile(t, m, "o.txt")
			},
			op: func(t *testing.T, m map[string]tools.Tool) (tools.Result, error) {
				return run(t, m["fs_write"], ctx0, map[string]any{"path": "o.txt", "content": "new\n"})
			},
			check: func(t *testing.T, ws string) { assertContent(t, ws, "o.txt", "orig\r\n") },
		},
		{
			name: "lines edit",
			setup: func(t *testing.T, ws string, m map[string]tools.Tool) {
				mustWrite(t, filepath.Join(ws, "l.txt"), "a\nb\n")
				readFile(t, m, "l.txt")
			},
			op: func(t *testing.T, m map[string]tools.Tool) (tools.Result, error) {
				return run(t, m["fs_lines"], ctx0, map[string]any{"path": "l.txt", "ops": []map[string]any{{"op": "delete", "start": 1, "expect": "a"}}})
			},
			check: func(t *testing.T, ws string) { assertContent(t, ws, "l.txt", "a\nb\n") },
		},
		{
			name:  "mkdir removes only what it created",
			setup: func(t *testing.T, ws string, m map[string]tools.Tool) { os.Mkdir(filepath.Join(ws, "keep"), 0o755) },
			op: func(t *testing.T, m map[string]tools.Tool) (tools.Result, error) {
				return run(t, m["fs_mkdir"], ctx0, map[string]any{"path": "keep/a/b"})
			},
			check: func(t *testing.T, ws string) {
				assertGone(t, ws, "keep/a")
				if _, err := os.Stat(filepath.Join(ws, "keep")); err != nil {
					t.Fatal("pre-existing directory was removed")
				}
			},
		},
		{
			name: "copy with overwrite restores the old dest",
			setup: func(t *testing.T, ws string, m map[string]tools.Tool) {
				mustWrite(t, filepath.Join(ws, "s.txt"), "src")
				mustWrite(t, filepath.Join(ws, "d.txt"), "dest")
			},
			op: func(t *testing.T, m map[string]tools.Tool) (tools.Result, error) {
				return run(t, m["fs_copy"], ctx0, map[string]any{"src": "s.txt", "dest": "d.txt", "overwrite": true})
			},
			check: func(t *testing.T, ws string) {
				assertContent(t, ws, "d.txt", "dest")
				assertContent(t, ws, "s.txt", "src")
			},
		},
		{
			name: "move with overwrite restores both",
			setup: func(t *testing.T, ws string, m map[string]tools.Tool) {
				mustWrite(t, filepath.Join(ws, "s.txt"), "src")
				mustWrite(t, filepath.Join(ws, "d.txt"), "dest")
			},
			op: func(t *testing.T, m map[string]tools.Tool) (tools.Result, error) {
				return run(t, m["fs_move"], ctx0, map[string]any{"src": "s.txt", "dest": "d.txt", "overwrite": true})
			},
			check: func(t *testing.T, ws string) {
				assertContent(t, ws, "d.txt", "dest")
				assertContent(t, ws, "s.txt", "src")
			},
		},
		{
			name: "delete directory",
			setup: func(t *testing.T, ws string, m map[string]tools.Tool) {
				mustWrite(t, filepath.Join(ws, "dir", "f"), "f")
			},
			op: func(t *testing.T, m map[string]tools.Tool) (tools.Result, error) {
				return run(t, m["fs_delete"], ctx0, map[string]any{"path": "dir", "recursive": true})
			},
			check: func(t *testing.T, ws string) { assertContent(t, ws, "dir/f", "f") },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, ws, m := setup(t)
			if tt.setup != nil {
				tt.setup(t, ws, m)
			}
			if _, err := tt.op(t, m); err != nil {
				t.Fatalf("operation: %v", err)
			}
			r, err := run(t, m["fs_undo"], ctx0, map[string]any{})
			if err != nil || !r.Mutated || !strings.Contains(r.Output, "undid #1") {
				t.Fatalf("undo: %v %+v", err, r)
			}
			tt.check(t, ws)
			if n := len(f.State.Changes()); n != 0 {
				t.Fatalf("%d change(s) still live after undo", n)
			}
		})
	}
}

func TestUndoRefusals(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, ws string, m map[string]tools.Tool) // performs a change and then interferes
		wantErr string
	}{
		{
			name: "mkdir that now has content",
			prepare: func(t *testing.T, ws string, m map[string]tools.Tool) {
				run(t, m["fs_mkdir"], ctx0, map[string]any{"path": "d"})
				mustWrite(t, filepath.Join(ws, "d", "new"), "x")
			},
			wantErr: "not empty",
		},
		{
			name: "move whose source path was reused",
			prepare: func(t *testing.T, ws string, m map[string]tools.Tool) {
				mustWrite(t, filepath.Join(ws, "a"), "a")
				run(t, m["fs_move"], ctx0, map[string]any{"src": "a", "dest": "b"})
				mustWrite(t, filepath.Join(ws, "a"), "again")
			},
			wantErr: "exists again",
		},
		{
			name: "delete whose path was recreated",
			prepare: func(t *testing.T, ws string, m map[string]tools.Tool) {
				mustWrite(t, filepath.Join(ws, "a"), "a")
				run(t, m["fs_delete"], ctx0, map[string]any{"path": "a"})
				mustWrite(t, filepath.Join(ws, "a"), "again")
			},
			wantErr: "exists again",
		},
		{
			name: "edited file that was deleted by someone else",
			prepare: func(t *testing.T, ws string, m map[string]tools.Tool) {
				run(t, m["fs_write"], ctx0, map[string]any{"path": "n.txt", "content": "x"})
				os.Remove(filepath.Join(ws, "n.txt"))
			},
			wantErr: "n.txt",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, ws, m := setup(t)
			tt.prepare(t, ws, m)
			if len(f.State.Changes()) != 1 {
				t.Fatalf("setup should record exactly one change: %+v", f.State.Changes())
			}
			if _, err := run(t, m["fs_undo"], ctx0, map[string]any{}); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if len(f.State.Changes()) != 1 {
				t.Fatal("a refused undo must not be journaled")
			}
		})
	}
}

func TestUndoStopsAtFirstFailureAndKeepsEarlierWork(t *testing.T) {
	f, ws, m := setup(t)
	run(t, m["fs_mkdir"], ctx0, map[string]any{"path": "d"})                     // #1
	run(t, m["fs_write"], ctx0, map[string]any{"path": "a.txt", "content": "a"}) // #2
	mustWrite(t, filepath.Join(ws, "d", "x"), "blocks undo of #1")
	r, err := run(t, m["fs_undo"], ctx0, map[string]any{"steps": 2})
	if err == nil || !strings.Contains(err.Error(), "stopped at #1") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(r.Output, "undid #2") {
		t.Fatalf("output = %q", r.Output)
	}
	assertGone(t, ws, "a.txt")
	if c := f.State.Changes(); len(c) != 1 || c[0].Seq != 1 {
		t.Fatalf("live changes = %+v", c)
	}
}

func TestUndoPathFilter(t *testing.T) {
	_, ws, m := setup(t)
	run(t, m["fs_write"], ctx0, map[string]any{"path": "a/1.txt", "content": "1"})
	run(t, m["fs_write"], ctx0, map[string]any{"path": "b/2.txt", "content": "2"})
	if _, err := run(t, m["fs_undo"], ctx0, map[string]any{"path": "a"}); err != nil {
		t.Fatal(err)
	}
	assertGone(t, ws, "a/1.txt")
	assertContent(t, ws, "b/2.txt", "2")
	rep, _ := run(t, m["fs_changes"], ctx0, map[string]any{"path": "a"})
	if !strings.Contains(rep.Output, "no file changes") {
		t.Fatalf("filtered report = %q", rep.Output)
	}
}

func TestUndoValidation(t *testing.T) {
	_, _, m := setup(t)
	for _, bad := range []string{`{"steps":0.5}`, `{"steps":-1}`, `{"steps":21}`, `{"bogus":true}`} {
		if m["fs_undo"].Validate([]byte(bad)) == nil {
			t.Errorf("must be rejected: %s", bad)
		}
	}
	if m["fs_undo"].Validate([]byte(`{}`)) != nil {
		t.Error("empty input should default to one step")
	}
}

func TestReportShowsDiffsSinceSessionStart(t *testing.T) {
	_, ws, m := setup(t)
	mustWrite(t, filepath.Join(ws, "a.txt"), "v1\n")
	readFile(t, m, "a.txt")
	run(t, m["fs_patch"], ctx0, map[string]any{"path": "a.txt", "old_string": "v1", "new_string": "v2"})
	run(t, m["fs_patch"], ctx0, map[string]any{"path": "a.txt", "old_string": "v2", "new_string": "v3"})
	rep, _ := run(t, m["fs_changes"], ctx0, map[string]any{})
	// one combined diff from the original v1 to the current v3
	if !strings.Contains(rep.Output, "2 change(s)") || !strings.Contains(rep.Output, "-v1") || !strings.Contains(rep.Output, "+v3") || strings.Contains(rep.Output, "+v2") {
		t.Fatalf("report:\n%s", rep.Output)
	}
	os.Remove(filepath.Join(ws, "a.txt"))
	rep, _ = run(t, m["fs_changes"], ctx0, map[string]any{})
	if !strings.Contains(rep.Output, "no longer exists") {
		t.Fatalf("report should note the missing file:\n%s", rep.Output)
	}
}

var ctx0 = context.Background()

func assertGone(t *testing.T, ws, rel string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(ws, rel)); err == nil {
		t.Fatalf("%s should not exist", rel)
	}
}

func assertContent(t *testing.T, ws, rel, want string) {
	t.Helper()
	if got := readString(t, filepath.Join(ws, rel)); got != want {
		t.Fatalf("%s = %q, want %q", rel, got, want)
	}
}
