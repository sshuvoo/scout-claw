package filesystem

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"scoutclaw/internal/tools"
)

func TestEditRequiresFreshRead(t *testing.T) {
	_, ws, m := setup(t)
	ctx := context.Background()
	p := filepath.Join(ws, "a.txt")
	os.WriteFile(p, []byte("one\ntwo\n"), 0o644)
	patch := map[string]any{"path": "a.txt", "old_string": "one", "new_string": "ONE"}

	if _, err := run(t, m["fs_patch"], ctx, patch); err == nil || !strings.Contains(err.Error(), "has not been read") {
		t.Fatalf("edit before read must fail: %v", err)
	}
	if _, err := run(t, m["fs_write"], ctx, map[string]any{"path": "a.txt", "content": "x\n"}); err == nil {
		t.Fatal("overwrite before read must fail")
	}
	readFile(t, m, "a.txt")
	os.WriteFile(p, []byte("one\ntwo\nthree\n"), 0o644) // changed behind the agent's back
	if _, err := run(t, m["fs_patch"], ctx, patch); err == nil || !strings.Contains(err.Error(), "changed on disk") {
		t.Fatalf("stale edit must fail: %v", err)
	}
	readFile(t, m, "a.txt")
	if _, err := run(t, m["fs_patch"], ctx, patch); err != nil {
		t.Fatal(err)
	}
	// the agent's own edit counts as seen, so a follow-up edit works without a re-read
	if _, err := run(t, m["fs_patch"], ctx, map[string]any{"path": "a.txt", "old_string": "two", "new_string": "TWO"}); err != nil {
		t.Fatalf("follow-up edit: %v", err)
	}
	// new files need no read
	if _, err := run(t, m["fs_write"], ctx, map[string]any{"path": "b.txt", "content": "b\n"}); err != nil {
		t.Fatal(err)
	}
}

func TestPatchMultipleEditsAndHints(t *testing.T) {
	_, ws, m := setup(t)
	ctx := context.Background()
	p := filepath.Join(ws, "main.go")
	os.WriteFile(p, []byte("func a() {\n\treturn 1\n}\n\nfunc b() {\n\treturn 2\n}\n"), 0o644)
	readFile(t, m, "main.go")

	edits := []map[string]any{
		{"old_string": "return 1", "new_string": "return 10"},
		{"old_string": "return 2", "new_string": "return 20"},
	}
	r, err := run(t, m["fs_patch"], ctx, map[string]any{"path": "main.go", "edits": edits})
	if err != nil || !strings.Contains(r.Output, "(+2 -2)") {
		t.Fatalf("%v %q", err, r.Output)
	}
	// all or nothing: the second edit fails, so the first is not applied either
	bad := []map[string]any{
		{"old_string": "return 10", "new_string": "return 100"},
		{"old_string": "missing", "new_string": "x"},
	}
	if _, err := run(t, m["fs_patch"], ctx, map[string]any{"path": "main.go", "edits": bad}); err == nil || !strings.Contains(err.Error(), "edit 2") {
		t.Fatalf("want edit 2 failure: %v", err)
	}
	if b, _ := os.ReadFile(p); !strings.Contains(string(b), "return 10\n") {
		t.Fatalf("partial edit applied: %q", b)
	}
	// whitespace near miss is explained
	_, err = run(t, m["fs_patch"], ctx, map[string]any{"path": "main.go", "old_string": "func a() {\n  return 10\n}", "new_string": "x"})
	if err == nil || !strings.Contains(err.Error(), "matches lines 1-3 if whitespace is ignored") {
		t.Fatalf("want whitespace hint: %v", err)
	}
	if m["fs_patch"].Validate(json.RawMessage(`{"path":"x","old_string":"a","new_string":"b","edits":[{"old_string":"c","new_string":"d"}]}`)) == nil {
		t.Fatal("old_string together with edits must be rejected")
	}
}

func TestPreservesCRLFAndBOM(t *testing.T) {
	_, ws, m := setup(t)
	ctx := context.Background()
	p := filepath.Join(ws, "win.txt")
	os.WriteFile(p, []byte("\ufeffalpha\r\nbeta\r\ngamma\r\n"), 0o644)
	out := readFile(t, m, "win.txt")
	if !strings.Contains(out, "CRLF") || strings.Contains(out, "\ufeff") {
		t.Fatalf("read header/BOM: %q", out)
	}
	if _, err := run(t, m["fs_patch"], ctx, map[string]any{"path": "win.txt", "old_string": "alpha\nbeta", "new_string": "alpha\nBETA\nextra"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "\ufeffalpha\r\nBETA\r\nextra\r\ngamma\r\n" {
		t.Fatalf("format not preserved: %q", b)
	}
	readFile(t, m, "win.txt")
	if _, err := run(t, m["fs_write"], ctx, map[string]any{"path": "win.txt", "content": "new\nfile\n"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "\ufeffnew\r\nfile\r\n" {
		t.Fatalf("overwrite format not preserved: %q", b)
	}
}

func TestLinesOps(t *testing.T) {
	_, ws, m := setup(t)
	ctx := context.Background()
	p := filepath.Join(ws, "f.txt")
	os.WriteFile(p, []byte("l1\nl2\nl3\nl4\nl5\n"), 0o644)
	readFile(t, m, "f.txt")

	ops := []map[string]any{
		{"op": "insert", "start": 0, "text": "top"},
		{"op": "replace", "start": 2, "end": 3, "expect": "l2\nl3", "text": "two-three"},
		{"op": "delete", "start": 5, "expect": "l5"},
		{"op": "insert", "start": 4, "expect": "l4", "text": "after4a\nafter4b\n"},
		{"op": "insert", "start": -1, "text": "end"},
	}
	r, err := run(t, m["fs_lines"], ctx, map[string]any{"path": "f.txt", "ops": ops})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "top\nl1\ntwo-three\nl4\nafter4a\nafter4b\nend\n" {
		t.Fatalf("got %q (output %q)", b, r.Output)
	}
	// a stale guard refuses and shows the real lines
	_, err = run(t, m["fs_lines"], ctx, map[string]any{"path": "f.txt", "ops": []map[string]any{{"op": "delete", "start": 2, "expect": "l1-wrong"}}})
	if err == nil || !strings.Contains(err.Error(), "expect does not match line 2") || !strings.Contains(err.Error(), "     2\tl1") {
		t.Fatalf("want guard failure with context: %v", err)
	}
	// overlapping ops are rejected
	_, err = run(t, m["fs_lines"], ctx, map[string]any{"path": "f.txt", "ops": []map[string]any{
		{"op": "delete", "start": 1, "end": 3, "expect": "top"},
		{"op": "insert", "start": 2, "expect": "l1", "text": "x"},
	}})
	if err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("want overlap error: %v", err)
	}
	if _, err := run(t, m["fs_lines"], ctx, map[string]any{"path": "f.txt", "ops": []map[string]any{{"op": "delete", "start": 99, "expect": "x"}}}); err == nil {
		t.Fatal("out of range must fail")
	}
	for _, bad := range []string{
		`{"path":"f.txt","ops":[{"op":"replace","start":1,"text":"x"}]}`,             // no expect
		`{"path":"f.txt","ops":[{"op":"insert","start":1,"text":"x"}]}`,              // no expect for insert > 0
		`{"path":"f.txt","ops":[{"op":"delete","start":1,"expect":"a","text":"x"}]}`, // text on delete
		`{"path":"f.txt","ops":[{"op":"move","start":1}]}`,
		`{"path":"f.txt","ops":[]}`,
	} {
		if m["fs_lines"].Validate(json.RawMessage(bad)) == nil {
			t.Fatalf("must be rejected: %s", bad)
		}
	}
	// a file without a final newline keeps it that way
	os.WriteFile(filepath.Join(ws, "n.txt"), []byte("a\nb"), 0o644)
	readFile(t, m, "n.txt")
	if _, err := run(t, m["fs_lines"], ctx, map[string]any{"path": "n.txt", "ops": []map[string]any{{"op": "replace", "start": 2, "expect": "b", "text": "B"}}}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "n.txt")); string(b) != "a\nB" {
		t.Fatalf("got %q", b)
	}
}

func TestManageAndUndo(t *testing.T) {
	f, ws, m := setup(t)
	ctx := context.Background()
	must := func(name string, in map[string]any) tools.Result {
		t.Helper()
		r, err := run(t, m[name], ctx, in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return r
	}
	must("fs_mkdir", map[string]any{"path": "src/pkg"})
	if st, err := os.Stat(filepath.Join(ws, "src/pkg")); err != nil || !st.IsDir() {
		t.Fatal("mkdir failed")
	}
	must("fs_write", map[string]any{"path": "src/pkg/a.go", "content": "package pkg\n"})
	must("fs_copy", map[string]any{"src": "src", "dest": "src2"})
	if b, _ := os.ReadFile(filepath.Join(ws, "src2/pkg/a.go")); string(b) != "package pkg\n" {
		t.Fatalf("copy: %q", b)
	}
	if _, err := run(t, m["fs_copy"], ctx, map[string]any{"src": "src", "dest": "src2"}); err == nil {
		t.Fatal("copy onto an existing dest without overwrite must fail")
	}
	if _, err := run(t, m["fs_copy"], ctx, map[string]any{"src": "src", "dest": "src/inner"}); err == nil {
		t.Fatal("copying a directory into itself must fail")
	}
	must("fs_move", map[string]any{"src": "src2/pkg/a.go", "dest": "src2/pkg/b.go"})
	if _, err := os.Stat(filepath.Join(ws, "src2/pkg/b.go")); err != nil {
		t.Fatal("move failed")
	}
	// the read record follows the move, so the moved file can be edited
	must("fs_patch", map[string]any{"path": "src2/pkg/b.go", "old_string": "pkg", "new_string": "pkg2"})
	if _, err := run(t, m["fs_delete"], ctx, map[string]any{"path": "src2"}); err == nil {
		t.Fatal("non-empty directory needs recursive")
	}
	r := must("fs_delete", map[string]any{"path": "src2", "recursive": true})
	if _, err := os.Stat(filepath.Join(ws, "src2")); err == nil || !strings.HasPrefix(r.Metadata["trash"], f.TrashDir) {
		t.Fatalf("delete should move to trash: %+v", r)
	}
	for _, p := range []string{".", ws} {
		if _, err := run(t, m["fs_delete"], ctx, map[string]any{"path": p, "recursive": true}); tools.KindOf(err) != tools.KindDenied {
			t.Fatalf("deleting the workspace must be denied: %v", err)
		}
	}
	os.Mkdir(filepath.Join(ws, ".git"), 0o755)
	if _, err := run(t, m["fs_delete"], ctx, map[string]any{"path": ".git", "recursive": true}); tools.KindOf(err) != tools.KindDenied {
		t.Fatalf("deleting .git must be denied: %v", err)
	}

	rep := must("fs_changes", map[string]any{}).Output
	for _, want := range []string{"mkdir", "copy", "move", "patch", "delete", "src/pkg/a.go (+1 -0)"} {
		if !strings.Contains(rep, want) {
			t.Fatalf("changes report missing %q:\n%s", want, rep)
		}
	}

	// undo delete, patch and move, newest first
	must("fs_undo", map[string]any{"steps": 3})
	if b, _ := os.ReadFile(filepath.Join(ws, "src2/pkg/a.go")); string(b) != "package pkg\n" {
		t.Fatalf("undo did not restore: %q", b)
	}
	if _, err := os.Stat(filepath.Join(ws, "src2/pkg/b.go")); err == nil {
		t.Fatal("undo of move left the new name")
	}
	// undo copy, write and mkdir
	must("fs_undo", map[string]any{"steps": 3})
	for _, p := range []string{"src2", "src/pkg/a.go", "src"} {
		if _, err := os.Stat(filepath.Join(ws, p)); err == nil {
			t.Fatalf("%s should be gone after undo", p)
		}
	}
	if _, err := run(t, m["fs_undo"], ctx, map[string]any{}); err == nil {
		t.Fatal("nothing left to undo")
	}
	// the journal survives a new State (e.g. a rebuilt runtime)
	if n := len(NewState(f.State.journal).Changes()); n != 0 {
		t.Fatalf("reloaded journal should show no live changes, got %d", n)
	}
}

func TestUndoRefusesWhenFileChangedAgain(t *testing.T) {
	_, ws, m := setup(t)
	ctx := context.Background()
	p := filepath.Join(ws, "c.txt")
	os.WriteFile(p, []byte("v1\n"), 0o644)
	readFile(t, m, "c.txt")
	if _, err := run(t, m["fs_patch"], ctx, map[string]any{"path": "c.txt", "old_string": "v1", "new_string": "v2"}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(p, []byte("v3 by someone else\n"), 0o644)
	if _, err := run(t, m["fs_undo"], ctx, map[string]any{}); err == nil || !strings.Contains(err.Error(), "changed again") {
		t.Fatalf("want refusal: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "v3 by someone else\n" {
		t.Fatal("undo overwrote a later change")
	}
}

func TestDeleteSymlinkRemovesLinkNotTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	_, ws, m := setup(t)
	os.WriteFile(filepath.Join(ws, "target.txt"), []byte("keep\n"), 0o644)
	os.Symlink(filepath.Join(ws, "target.txt"), filepath.Join(ws, "link.txt"))
	if _, err := run(t, m["fs_delete"], context.Background(), map[string]any{"path": "link.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(ws, "link.txt")); err == nil {
		t.Fatal("link still there")
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "target.txt")); string(b) != "keep\n" {
		t.Fatal("target was deleted")
	}
}
