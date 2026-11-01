package filesystem

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"scoutclaw/internal/tools"
)

func mustWrite(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readString(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func TestMkdir(t *testing.T) {
	f, ws, m := setup(t)
	ctx := context.Background()
	mustWrite(t, filepath.Join(ws, "file"), "x")

	tests := []struct {
		name    string
		path    string
		mutated bool
		wantErr tools.Kind
	}{
		{name: "creates parents", path: "a/b/c", mutated: true},
		{name: "existing directory is a no-op", path: "a/b"},
		{name: "existing file", path: "file", wantErr: tools.KindInvalidInput},
		{name: "outside workspace", path: "../escape", wantErr: tools.KindDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := run(t, m["fs_mkdir"], ctx, map[string]any{"path": tt.path})
			switch {
			case tt.wantErr == tools.KindNone && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != tools.KindNone && tools.KindOf(err) != tt.wantErr:
				t.Fatalf("err = %v, want %s", err, tt.wantErr)
			}
			if r.Mutated != tt.mutated {
				t.Fatalf("mutated = %v", r.Mutated)
			}
		})
	}
	c := f.State.Changes()
	if len(c) != 1 || c[0].CreatedDirs != filepath.Join(ws, "a") {
		t.Fatalf("mkdir should journal the top-most created dir: %+v", c)
	}
}

func TestPrepareTransferErrors(t *testing.T) {
	f, ws, _ := setup(t)
	mustWrite(t, filepath.Join(ws, "a.txt"), "a")
	mustWrite(t, filepath.Join(ws, "b.txt"), "b")
	mustWrite(t, filepath.Join(ws, "dir", "x"), "x")
	tests := []struct {
		name    string
		op      string
		args    transferArgs
		wantErr string
	}{
		{"missing source", "copy", transferArgs{Src: "nope", Dest: "z"}, "nope"},
		{"same path", "copy", transferArgs{Src: "a.txt", Dest: "a.txt"}, "same path"},
		{"directory into itself", "move", transferArgs{Src: "dir", Dest: "dir/sub"}, "into itself"},
		{"dest exists", "copy", transferArgs{Src: "a.txt", Dest: "b.txt"}, "already exists"},
		{"move workspace root", "move", transferArgs{Src: ".", Dest: "../x"}, "workspace root"},
		{"credential source", "copy", transferArgs{Src: ".ssh/id_rsa", Dest: "k"}, "credential"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, _, err := f.prepareTransfer(tt.op, tt.args)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
	if _, _, _, exists, err := f.prepareTransfer("copy", transferArgs{Src: "a.txt", Dest: "b.txt", Overwrite: true}); err != nil || !exists {
		t.Fatalf("overwrite should be accepted and report the existing dest: %v %v", exists, err)
	}
}

func TestCopyAndMoveOverwriteSendOldDestToTrash(t *testing.T) {
	for _, op := range []string{"fs_copy", "fs_move"} {
		t.Run(op, func(t *testing.T) {
			f, ws, m := setup(t)
			mustWrite(t, filepath.Join(ws, "src.txt"), "new")
			mustWrite(t, filepath.Join(ws, "dst.txt"), "old")
			r, err := run(t, m[op], context.Background(), map[string]any{"src": "src.txt", "dest": "dst.txt", "overwrite": true})
			if err != nil {
				t.Fatal(err)
			}
			if got := readString(t, filepath.Join(ws, "dst.txt")); got != "new" {
				t.Fatalf("dest = %q", got)
			}
			c := f.State.Changes()[0]
			if !strings.HasPrefix(c.Trash, f.TrashDir) || readString(t, c.Trash) != "old" || !strings.Contains(r.Output, c.Trash) {
				t.Fatalf("old dest not in trash: %+v %q", c, r.Output)
			}
			_, srcErr := os.Stat(filepath.Join(ws, "src.txt"))
			if (op == "fs_move") != errors.Is(srcErr, os.ErrNotExist) {
				t.Fatalf("source presence wrong after %s: %v", op, srcErr)
			}
		})
	}
}

func TestMoveIntoNewDirectoryAndOutsideNeedsGrant(t *testing.T) {
	_, ws, m := setup(t)
	ctx := context.Background()
	mustWrite(t, filepath.Join(ws, "a.txt"), "a")
	if _, err := run(t, m["fs_move"], ctx, map[string]any{"src": "a.txt", "dest": "new/dir/a.txt"}); err != nil {
		t.Fatal(err)
	}
	if readString(t, filepath.Join(ws, "new/dir/a.txt")) != "a" {
		t.Fatal("not moved")
	}
	outside := filepath.Join(t.TempDir(), "a.txt")
	if _, err := run(t, m["fs_move"], ctx, map[string]any{"src": "new/dir/a.txt", "dest": outside}); tools.KindOf(err) != tools.KindDenied {
		t.Fatalf("move outside without grant must be denied: %v", err)
	}
	granted := tools.WithGrant(ctx, tools.Grant{OutsideWorkspace: true})
	if _, err := run(t, m["fs_move"], granted, map[string]any{"src": "new/dir/a.txt", "dest": outside}); err != nil {
		t.Fatalf("granted move: %v", err)
	}
}

func TestCopyTreeSkipsGitAndCredentialsAndKeepsSymlinks(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "keep.go"), "package x")
	mustWrite(t, filepath.Join(src, ".git", "HEAD"), "ref")
	mustWrite(t, filepath.Join(src, "server.key"), "secret")
	if runtime.GOOS != "windows" {
		os.Chmod(filepath.Join(src, "keep.go"), 0o755)
		os.Symlink("keep.go", filepath.Join(src, "link"))
	}
	dest := filepath.Join(t.TempDir(), "out")
	n, skipped, err := copyTree(context.Background(), src, dest, true)
	if err != nil {
		t.Fatal(err)
	}
	if readString(t, filepath.Join(dest, "keep.go")) != "package x" {
		t.Fatal("regular file not copied")
	}
	for _, p := range []string{".git", "server.key"} {
		if _, err := os.Lstat(filepath.Join(dest, p)); err == nil {
			t.Errorf("%s should have been skipped", p)
		}
	}
	if len(skipped) != 1 || !strings.HasSuffix(skipped[0], "server.key") {
		t.Fatalf("skipped = %v", skipped)
	}
	if runtime.GOOS != "windows" {
		if target, err := os.Readlink(filepath.Join(dest, "link")); err != nil || target != "keep.go" {
			t.Fatalf("symlink not recreated: %q %v", target, err)
		}
		if st, _ := os.Stat(filepath.Join(dest, "keep.go")); st.Mode().Perm() != 0o755 {
			t.Fatalf("mode not kept: %v", st.Mode())
		}
	}
	if n < 2 {
		t.Fatalf("entry count = %d", n)
	}
	// unfiltered copies (used by moves) keep everything
	all := filepath.Join(t.TempDir(), "all")
	if _, _, err := copyTree(context.Background(), src, all, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(all, ".git", "HEAD")); err != nil {
		t.Fatal("unfiltered copy must include .git")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := copyTree(ctx, src, filepath.Join(t.TempDir(), "c"), true); err == nil {
		t.Fatal("cancelled copy must fail")
	}
}

func TestMoveAny(t *testing.T) {
	t.Run("refuses existing dest", func(t *testing.T) {
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, "a"), "a")
		mustWrite(t, filepath.Join(dir, "b"), "b")
		if err := moveAny(filepath.Join(dir, "a"), filepath.Join(dir, "b")); err == nil {
			t.Fatal("must refuse to replace dest")
		}
		if readString(t, filepath.Join(dir, "b")) != "b" {
			t.Fatal("dest was touched")
		}
	})
	t.Run("missing source", func(t *testing.T) {
		dir := t.TempDir()
		if err := moveAny(filepath.Join(dir, "nope"), filepath.Join(dir, "x")); tools.KindOf(err) != tools.KindNotFound {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("falls back to copy when rename fails across devices", func(t *testing.T) {
		old := rename
		rename = func(a, b string) error { return &os.LinkError{Op: "rename", Old: a, New: b, Err: syscall.EXDEV} }
		t.Cleanup(func() { rename = old })
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, "src", "f.txt"), "data")
		if err := moveAny(filepath.Join(dir, "src"), filepath.Join(dir, "dst")); err != nil {
			t.Fatal(err)
		}
		if readString(t, filepath.Join(dir, "dst", "f.txt")) != "data" {
			t.Fatal("not copied")
		}
		if _, err := os.Stat(filepath.Join(dir, "src")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("source not removed after fallback")
		}
	})
}

func TestToTrashRequiresTrashDir(t *testing.T) {
	f, ws, _ := setup(t)
	mustWrite(t, filepath.Join(ws, "a"), "a")
	f.TrashDir = ""
	if _, err := f.toTrash(filepath.Join(ws, "a")); tools.KindOf(err) != tools.KindUnavailable {
		t.Fatalf("err = %v", err)
	}
	if readString(t, filepath.Join(ws, "a")) != "a" {
		t.Fatal("file removed without a trash")
	}
}

func TestToTrashUsesUniqueFolders(t *testing.T) {
	f, ws, _ := setup(t)
	var got []string
	for i := 0; i < 3; i++ {
		mustWrite(t, filepath.Join(ws, "same.txt"), "x")
		p, err := f.toTrash(filepath.Join(ws, "same.txt"))
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, p)
	}
	if got[0] == got[1] || got[1] == got[2] {
		t.Fatalf("trash paths collide: %v", got)
	}
}

func TestDeleteCases(t *testing.T) {
	_, ws, m := setup(t)
	ctx := context.Background()
	mustWrite(t, filepath.Join(ws, "f.txt"), "x")
	os.Mkdir(filepath.Join(ws, "empty"), 0o755)
	mustWrite(t, filepath.Join(ws, "full", "x"), "x")
	os.Mkdir(filepath.Join(ws, ".git"), 0o755)
	tests := []struct {
		name    string
		in      map[string]any
		wantErr tools.Kind
		gone    string
	}{
		{name: "file", in: map[string]any{"path": "f.txt"}, gone: "f.txt"},
		{name: "empty dir without recursive", in: map[string]any{"path": "empty"}, gone: "empty"},
		{name: "full dir without recursive", in: map[string]any{"path": "full"}, wantErr: tools.KindInvalidInput},
		{name: "full dir recursive", in: map[string]any{"path": "full", "recursive": true}, gone: "full"},
		{name: "missing", in: map[string]any{"path": "nope"}, wantErr: tools.KindNotFound},
		{name: "git dir", in: map[string]any{"path": ".git", "recursive": true}, wantErr: tools.KindDenied},
		{name: "workspace root", in: map[string]any{"path": ".", "recursive": true}, wantErr: tools.KindDenied},
		{name: "parent of workspace", in: map[string]any{"path": "..", "recursive": true}, wantErr: tools.KindDenied},
		{name: "outside without grant", in: map[string]any{"path": filepath.Join(t.TempDir(), "x")}, wantErr: tools.KindDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := run(t, m["fs_delete"], ctx, tt.in)
			if tt.wantErr != tools.KindNone {
				if tools.KindOf(err) != tt.wantErr {
					t.Fatalf("err = %v, want %s", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(filepath.Join(ws, tt.gone)); err == nil {
				t.Fatalf("%s still exists", tt.gone)
			}
		})
	}
}

func TestPreviews(t *testing.T) {
	_, ws, m := setup(t)
	ctx := context.Background()
	mustWrite(t, filepath.Join(ws, "a.txt"), "one\ntwo\n")
	mustWrite(t, filepath.Join(ws, "b.txt"), "b")
	mustWrite(t, filepath.Join(ws, "d", "x"), "x")
	readFile(t, m, "a.txt")
	preview := func(tool string, in map[string]any) (string, error) {
		b, _ := json.Marshal(in)
		return m[tool].(tools.Previewer).Preview(ctx, b)
	}
	tests := []struct {
		tool    string
		in      map[string]any
		want    []string
		wantErr string
	}{
		{"fs_write", map[string]any{"path": "new.txt", "content": "hi\n"}, []string{"new file", "+hi"}, ""},
		{"fs_write", map[string]any{"path": "a.txt", "content": "one\n"}, []string{"overwrite", "-two"}, ""},
		{"fs_write", map[string]any{"path": "b.txt", "content": "x"}, nil, "has not been read"},
		{"fs_patch", map[string]any{"path": "a.txt", "old_string": "two", "new_string": "2"}, []string{"edit", "-two", "+2"}, ""},
		{"fs_lines", map[string]any{"path": "a.txt", "ops": []map[string]any{{"op": "delete", "start": 1, "expect": "one"}}}, []string{"edit", "-one"}, ""},
		{"fs_lines", map[string]any{"path": "a.txt", "ops": []map[string]any{{"op": "delete", "start": 1, "expect": "bad"}}}, nil, "expect does not match"},
		{"fs_mkdir", map[string]any{"path": "x/y"}, []string{"create directory"}, ""},
		{"fs_copy", map[string]any{"src": "d", "dest": "d2"}, []string{"copy directory (2 entries)", "-> "}, ""},
		{"fs_move", map[string]any{"src": "a.txt", "dest": "b.txt", "overwrite": true}, []string{"move file", "replaces the existing"}, ""},
		{"fs_move", map[string]any{"src": "a.txt", "dest": "b.txt"}, nil, "already exists"},
		{"fs_delete", map[string]any{"path": "d", "recursive": true}, []string{"delete directory", "session trash"}, ""},
		{"fs_delete", map[string]any{"path": "a.txt"}, []string{"delete file"}, ""},
		{"fs_undo", map[string]any{}, nil, "no changes to undo"},
	}
	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			got, err := preview(tt.tool, tt.in)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("preview missing %q:\n%s", w, got)
				}
			}
		})
	}
	// previews never change anything
	if readString(t, filepath.Join(ws, "a.txt")) != "one\ntwo\n" {
		t.Fatal("a preview modified a file")
	}
	if _, err := os.Stat(filepath.Join(ws, "x")); err == nil {
		t.Fatal("mkdir preview created a directory")
	}
}

func TestTransferValidation(t *testing.T) {
	_, _, m := setup(t)
	for _, bad := range []string{`{"src":"","dest":"x"}`, `{"src":"x","dest":""}`, `{"src":"x"}`, `{"src":"a","dest":"b","extra":1}`} {
		if m["fs_copy"].Validate(json.RawMessage(bad)) == nil {
			t.Errorf("must be rejected: %s", bad)
		}
	}
}

func TestOutputUsesWorkspaceRelativePaths(t *testing.T) {
	f, ws, m := setup(t)
	outside := filepath.Join(t.TempDir(), "o.txt")
	tests := []struct {
		abs, want string
	}{
		{filepath.Join(ws, "a", "b.go"), filepath.Join("a", "b.go")},
		{ws, "."},
		{outside, outside},
	}
	for _, tt := range tests {
		if got := f.rel(tt.abs); got != tt.want {
			t.Errorf("rel(%q) = %q, want %q", tt.abs, got, tt.want)
		}
	}
	mustWrite(t, filepath.Join(ws, "src", "a.txt"), "x\n")
	if out := readFile(t, m, "src/a.txt"); !strings.HasPrefix(out, filepath.Join("src", "a.txt")+": lines 1-1") {
		t.Fatalf("read header should be relative: %q", out)
	}
	r, _ := run(t, m["fs_patch"], context.Background(), map[string]any{"path": "src/a.txt", "old_string": "x", "new_string": "y"})
	if strings.Contains(r.Output, ws) || !strings.Contains(r.Output, "edited "+filepath.Join("src", "a.txt")) {
		t.Fatalf("edit output should not repeat the workspace path: %q", r.Output)
	}
	if rep := f.Report(""); strings.Contains(rep, ws) {
		t.Fatalf("report should use relative paths:\n%s", rep)
	}
}
