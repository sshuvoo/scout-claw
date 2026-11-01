package filesystem

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"scoutclaw/internal/tools"
)

func applyPatchRun(t *testing.T, m map[string]tools.Tool, patch string) (tools.Result, error) {
	t.Helper()
	return run(t, m["fs_apply_patch"], context.Background(), map[string]any{"patch": patch})
}

func TestApplyPatchMultiFile(t *testing.T) {
	f, ws, m := setup(t)
	mustWrite(t, filepath.Join(ws, "a.go"), "package x\n\nfunc A() int {\n\treturn 1\n}\n")
	mustWrite(t, filepath.Join(ws, "old.go"), "package x\n\nvar Old = 1\n")
	mustWrite(t, filepath.Join(ws, "gone.txt"), "bye\n")
	readFile(t, m, "a.go")
	readFile(t, m, "old.go")

	patch := `*** Begin Patch
*** Add File: pkg/new.go
+package pkg
+
+func New() {}
*** Update File: a.go
@@ func A() int {
-	return 1
+	return 2
*** Update File: old.go
*** Move to: renamed.go
@@
-var Old = 1
+var Renamed = 1
*** Delete File: gone.txt
*** End Patch`
	r, err := applyPatchRun(t, m, patch)
	if err != nil {
		t.Fatal(err)
	}
	assertContent(t, ws, "pkg/new.go", "package pkg\n\nfunc New() {}\n")
	assertContent(t, ws, "a.go", "package x\n\nfunc A() int {\n\treturn 2\n}\n")
	assertContent(t, ws, "renamed.go", "package x\n\nvar Renamed = 1\n")
	assertGone(t, ws, "old.go")
	assertGone(t, ws, "gone.txt")
	if !r.Mutated || !strings.Contains(r.Output, "fs_undo with steps=5") {
		t.Fatalf("output: %q", r.Output)
	}

	// the renamed file stays editable, and the whole patch can be undone
	if _, err := run(t, m["fs_patch"], context.Background(), map[string]any{"path": "renamed.go", "old_string": "Renamed", "new_string": "R"}); err != nil {
		t.Fatalf("edit after move: %v", err)
	}
	if _, err := run(t, m["fs_undo"], context.Background(), map[string]any{"steps": 6}); err != nil {
		t.Fatal(err)
	}
	assertGone(t, ws, "pkg/new.go")
	assertContent(t, ws, "a.go", "package x\n\nfunc A() int {\n\treturn 1\n}\n")
	assertContent(t, ws, "old.go", "package x\n\nvar Old = 1\n")
	assertContent(t, ws, "gone.txt", "bye\n")
	if n := len(f.State.Changes()); n != 0 {
		t.Fatalf("%d live changes after undo", n)
	}
}

func TestApplyPatchIsAllOrNothing(t *testing.T) {
	f, ws, m := setup(t)
	mustWrite(t, filepath.Join(ws, "a.txt"), "one\n")
	mustWrite(t, filepath.Join(ws, "b.txt"), "two\n")
	readFile(t, m, "a.txt")
	readFile(t, m, "b.txt")

	t.Run("bad hunk in the last file changes nothing", func(t *testing.T) {
		_, err := applyPatchRun(t, m, "*** Begin Patch\n*** Update File: a.txt\n-one\n+ONE\n*** Update File: b.txt\n-missing\n+x\n*** End Patch")
		if err == nil || !strings.Contains(err.Error(), "b.txt") {
			t.Fatalf("err = %v", err)
		}
		assertContent(t, ws, "a.txt", "one\n")
		if len(f.State.Changes()) != 0 {
			t.Fatal("nothing should be journaled")
		}
	})

	t.Run("failure while writing rolls back earlier files", func(t *testing.T) {
		mustWrite(t, filepath.Join(ws, "blocker"), "a file, so blocker/x cannot be created")
		_, err := applyPatchRun(t, m, "*** Begin Patch\n*** Update File: a.txt\n-one\n+ONE\n*** Update File: b.txt\n*** Move to: blocker/b.txt\n-two\n+TWO\n*** End Patch")
		if err == nil || !strings.Contains(err.Error(), "restored") {
			t.Fatalf("err = %v", err)
		}
		assertContent(t, ws, "a.txt", "one\n")
		assertContent(t, ws, "b.txt", "two\n")
		if n := len(f.State.Changes()); n != 0 {
			t.Fatalf("rolled-back changes still live: %d", n)
		}
	})
}

func TestApplyPatchChecks(t *testing.T) {
	_, ws, m := setup(t)
	mustWrite(t, filepath.Join(ws, "a.txt"), "one\n")
	mustWrite(t, filepath.Join(ws, "exists.txt"), "x\n")
	os.MkdirAll(filepath.Join(ws, "dir"), 0o755)
	tests := []struct {
		name    string
		patch   string
		wantErr string
	}{
		{"update needs a prior read", "*** Begin Patch\n*** Update File: a.txt\n-one\n+1\n*** End Patch", "has not been read"},
		{"add over an existing file", "*** Begin Patch\n*** Add File: exists.txt\n+y\n*** End Patch", "already exists"},
		{"update a missing file", "*** Begin Patch\n*** Update File: nope.txt\n-a\n+b\n*** End Patch", "does not exist"},
		{"delete a directory", "*** Begin Patch\n*** Delete File: dir\n*** End Patch", "use fs_delete"},
		{"delete a missing file", "*** Begin Patch\n*** Delete File: nope.txt\n*** End Patch", "no such file"},
		{"write outside the workspace", "*** Begin Patch\n*** Add File: ../escape.txt\n+x\n*** End Patch", "outside the workspace"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := applyPatchRun(t, m, tt.patch); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
	readFile(t, m, "a.txt")
	readFile(t, m, "exists.txt")
	if _, err := applyPatchRun(t, m, "*** Begin Patch\n*** Update File: a.txt\n*** Move to: exists.txt\n-one\n+1\n*** End Patch"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("move onto an existing file: %v", err)
	}
	if m["fs_apply_patch"].Validate(json.RawMessage(`{"patch":"not a patch"}`)) == nil {
		t.Fatal("invalid patch must fail validation")
	}
}

func TestApplyPatchUnifiedDiffAndCRLF(t *testing.T) {
	_, ws, m := setup(t)
	mustWrite(t, filepath.Join(ws, "win.txt"), "alpha\r\nbeta\r\ngamma\r\n")
	readFile(t, m, "win.txt")
	diff := "--- a/win.txt\n+++ b/win.txt\n@@ -1,3 +1,3 @@\n alpha\n-beta\n+BETA\n gamma\n"
	if _, err := applyPatchRun(t, m, diff); err != nil {
		t.Fatal(err)
	}
	assertContent(t, ws, "win.txt", "alpha\r\nBETA\r\ngamma\r\n")
}

func TestApplyPatchPreview(t *testing.T) {
	_, ws, m := setup(t)
	mustWrite(t, filepath.Join(ws, "a.txt"), "one\n")
	mustWrite(t, filepath.Join(ws, "d.txt"), "x\n")
	readFile(t, m, "a.txt")
	b, _ := json.Marshal(map[string]any{"patch": "*** Begin Patch\n*** Update File: a.txt\n*** Move to: b.txt\n-one\n+1\n*** Add File: c.txt\n+c\n*** Delete File: d.txt\n*** End Patch"})
	got, err := m["fs_apply_patch"].(tools.Previewer).Preview(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"patch touching 3 file(s)", "update a.txt -> b.txt", "-one", "+1", "add c.txt", "+c", "delete d.txt"} {
		if !strings.Contains(got, w) {
			t.Errorf("preview missing %q:\n%s", w, got)
		}
	}
	assertContent(t, ws, "a.txt", "one\n")
	assertGone(t, ws, "c.txt")
}
