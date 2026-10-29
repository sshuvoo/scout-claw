package filesystem

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"scoutclaw/internal/policy"
	"scoutclaw/internal/tools"
)

func setup(t *testing.T) (FS, string, map[string]tools.Tool) {
	ws := t.TempDir()
	ws, _ = filepath.EvalSymlinks(ws)
	f := FS{Scope: policy.NewScope(ws), BackupDir: filepath.Join(t.TempDir(), "bk"), SessionID: "s1"}
	m := map[string]tools.Tool{}
	for _, tl := range f.Tools() {
		m[tl.Name()] = tl
	}
	return f, ws, m
}

func run(t *testing.T, tl tools.Tool, ctx context.Context, in map[string]any) (tools.Result, error) {
	b, _ := json.Marshal(in)
	return tl.Execute(ctx, b)
}

func TestReadListStatSearch(t *testing.T) {
	_, ws, m := setup(t)
	os.WriteFile(filepath.Join(ws, "a.log"), []byte("one\nERROR two\nthree\n"), 0o644)
	os.Mkdir(filepath.Join(ws, "sub"), 0o755)
	os.WriteFile(filepath.Join(ws, "sub", "b.txt"), []byte("hello ERROR\n"), 0o644)
	os.WriteFile(filepath.Join(ws, "bin.dat"), []byte{1, 2, 0, 3}, 0o644)
	ctx := context.Background()

	r, err := run(t, m["fs_read"], ctx, map[string]any{"path": "a.log"})
	if err != nil || !strings.Contains(r.Output, "     2\tERROR two") {
		t.Fatalf("read: %v %q", err, r.Output)
	}
	r, _ = run(t, m["fs_read"], ctx, map[string]any{"path": "a.log", "offset_line": -1})
	if !strings.Contains(r.Output, "three") || strings.Contains(r.Output, "one") {
		t.Fatalf("tail read wrong: %q", r.Output)
	}
	if _, err := run(t, m["fs_read"], ctx, map[string]any{"path": "bin.dat"}); err == nil {
		t.Fatal("binary files must be refused")
	}
	if _, err := run(t, m["fs_read"], ctx, map[string]any{"path": "missing"}); tools.KindOf(err) != tools.KindNotFound {
		t.Fatalf("want not_found, got %v", err)
	}
	r, _ = run(t, m["fs_list"], ctx, map[string]any{"depth": 2})
	if !strings.Contains(r.Output, "a.log") || !strings.Contains(r.Output, "b.txt") {
		t.Fatalf("list: %q", r.Output)
	}
	r, _ = run(t, m["fs_stat"], ctx, map[string]any{"path": "a.log"})
	if !strings.Contains(r.Output, "type: file") || !strings.Contains(r.Output, "in_workspace: true") {
		t.Fatalf("stat: %q", r.Output)
	}
	r, _ = run(t, m["fs_search"], ctx, map[string]any{"pattern": "ERROR"})
	if !strings.Contains(r.Output, "a.log:2") || !strings.Contains(r.Output, "b.txt:1") || strings.Contains(r.Output, "bin.dat") {
		t.Fatalf("search: %q", r.Output)
	}
	r, _ = run(t, m["fs_search"], ctx, map[string]any{"name_glob": "*.txt"})
	if !strings.Contains(r.Output, "b.txt") || strings.Contains(r.Output, "a.log") {
		t.Fatalf("glob search: %q", r.Output)
	}
	if m["fs_search"].Validate(json.RawMessage(`{"pattern":"("}`)) == nil {
		t.Fatal("bad regex must be rejected")
	}
}

func TestPatchWritesDiffAndBackup(t *testing.T) {
	f, ws, m := setup(t)
	p := filepath.Join(ws, "app.conf")
	os.WriteFile(p, []byte("port=80\nmode=dev\n"), 0o640)
	ctx := context.Background()

	prev, err := m["fs_patch"].(tools.Previewer).Preview(ctx, json.RawMessage(`{"path":"app.conf","old_string":"port=80","new_string":"port=8080"}`))
	if err != nil || !strings.Contains(prev, "-port=80") || !strings.Contains(prev, "+port=8080") {
		t.Fatalf("preview: %v %q", err, prev)
	}
	r, err := run(t, m["fs_patch"], ctx, map[string]any{"path": "app.conf", "old_string": "port=80", "new_string": "port=8080"})
	if err != nil || !r.Mutated {
		t.Fatalf("%v %+v", err, r)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "port=8080\nmode=dev\n" {
		t.Fatalf("content: %q", got)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o640 {
		t.Fatalf("mode not preserved: %v", st.Mode())
	}
	bk := r.Metadata["backup"]
	old, err := os.ReadFile(bk)
	if err != nil || string(old) != "port=80\nmode=dev\n" || !strings.HasPrefix(bk, f.BackupDir) {
		t.Fatalf("backup missing or wrong: %v %q", err, bk)
	}
	// ambiguity and absence
	os.WriteFile(p, []byte("x\nx\n"), 0o644)
	if _, err := run(t, m["fs_patch"], ctx, map[string]any{"path": "app.conf", "old_string": "x", "new_string": "y"}); tools.KindOf(err) != tools.KindInvalidInput {
		t.Fatalf("ambiguous match must fail: %v", err)
	}
	if _, err := run(t, m["fs_patch"], ctx, map[string]any{"path": "app.conf", "old_string": "nope", "new_string": "y"}); err == nil {
		t.Fatal("missing text must fail")
	}
	if _, err := run(t, m["fs_patch"], ctx, map[string]any{"path": "app.conf", "old_string": "x", "new_string": "y", "replace_all": true}); err != nil {
		t.Fatal(err)
	}
}

func TestWriteCreatesAndOverwrites(t *testing.T) {
	_, ws, m := setup(t)
	ctx := context.Background()
	r, err := run(t, m["fs_write"], ctx, map[string]any{"path": "new/dir/f.txt", "content": "hi\n"})
	if err != nil || !r.Mutated {
		t.Fatalf("%v %+v", err, r)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "new/dir/f.txt")); string(b) != "hi\n" {
		t.Fatalf("%q", b)
	}
	r, _ = run(t, m["fs_write"], ctx, map[string]any{"path": "new/dir/f.txt", "content": "hi\n"})
	if r.Mutated || !strings.Contains(r.Output, "no change") {
		t.Fatalf("identical write should be a no-op: %+v", r)
	}
	if m["fs_write"].Validate(json.RawMessage(`{"path":"x","content":"a\u0000b"}`)) == nil {
		t.Fatal("NUL content must be rejected")
	}
}

func TestWritesOutsideWorkspaceNeedGrant(t *testing.T) {
	_, ws, m := setup(t)
	outside := filepath.Join(t.TempDir(), "o.txt")
	in := map[string]any{"path": outside, "content": "x"}
	if _, err := run(t, m["fs_write"], context.Background(), in); tools.KindOf(err) != tools.KindDenied {
		t.Fatalf("absolute outside path must be denied without grant: %v", err)
	}
	if _, err := run(t, m["fs_write"], context.Background(), map[string]any{"path": "../traversal.txt", "content": "x"}); tools.KindOf(err) != tools.KindDenied {
		t.Fatalf("traversal must be denied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(ws), "traversal.txt")); err == nil {
		t.Fatal("file escaped workspace")
	}
	// symlink pointing outside
	target := t.TempDir()
	os.Symlink(target, filepath.Join(ws, "lnk"))
	if _, err := run(t, m["fs_write"], context.Background(), map[string]any{"path": "lnk/x.txt", "content": "x"}); tools.KindOf(err) != tools.KindDenied {
		t.Fatalf("symlink escape must be denied: %v", err)
	}
	// with an approval grant the same write succeeds
	granted := tools.WithGrant(context.Background(), tools.Grant{OutsideWorkspace: true})
	if _, err := run(t, m["fs_write"], granted, in); err != nil {
		t.Fatalf("granted write failed: %v", err)
	}
	// credential files are never writable, grant or not
	ssh := filepath.Join(t.TempDir(), ".ssh", "id_rsa")
	if _, err := run(t, m["fs_write"], granted, map[string]any{"path": ssh, "content": "x"}); tools.KindOf(err) != tools.KindDenied {
		t.Fatalf("key file write must be denied: %v", err)
	}
}

func TestUnifiedDiff(t *testing.T) {
	d := UnifiedDiff("f", "a\nb\nc\n", "a\nB\nc\n")
	if !strings.Contains(d, "-b") || !strings.Contains(d, "+B") || !strings.Contains(d, " a") {
		t.Fatalf("%s", d)
	}
	if UnifiedDiff("f", "same\n", "same\n") != "(no differences)\n" {
		t.Fatal("identical inputs should report no differences")
	}
}
