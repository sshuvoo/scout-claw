package filesystem

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"scoutclaw/internal/tools"
)

type applyPatchArgs struct {
	Patch string `json:"patch"`
}

const applyPatchDesc = `Apply one patch that adds, updates, deletes and renames files, atomically: either every file changes or none does. Updated files must have been read first. Preferred for changes that touch several files or many places. Format:
*** Begin Patch
*** Add File: path/new.go
+line of the new file
*** Update File: path/old.go
*** Move to: path/renamed.go   (optional)
@@ func Example
 context line (leading space)
-removed line
+added line
*** Delete File: path/obsolete.go
*** End Patch
Each hunk lists the old lines (context and '-') exactly as they are in the file, with about 3 lines of context; '@@ text' (optional) searches after the line containing text. A standard unified diff (--- a/x, +++ b/x, @@ -l,n +l,n @@) is also accepted.`

func (f FS) applyPatchTool() tools.Tool {
	return &tools.Typed[applyPatchArgs]{
		ToolName: "fs_apply_patch",
		Desc:     applyPatchDesc,
		Schema:   `{"type":"object","properties":{"patch":{"type":"string"}},"required":["patch"],"additionalProperties":false}`,
		ValidateFn: func(a *applyPatchArgs) error {
			_, err := parsePatch(a.Patch)
			return err
		},
		Run:       f.applyPatch,
		PreviewFn: f.previewApplyPatch,
	}
}

// plannedFile is one file of a patch, checked and with its new contents computed.
type plannedFile struct {
	pf      patchFile
	abs     string
	dest    string // resolved move target
	t       textFile
	updated string // new text for add and update
}

// planPatch parses the patch and computes every change without touching the
// filesystem, so a bad hunk in the last file stops the whole patch.
func (f FS) planPatch(patch string) ([]plannedFile, error) {
	files, err := parsePatch(patch)
	if err != nil {
		return nil, tools.Errorf(tools.KindInvalidInput, "%v", err)
	}
	plan := make([]plannedFile, 0, len(files))
	for _, pf := range files {
		p := plannedFile{pf: pf}
		fail := func(err error) error {
			if errors.As(err, new(*tools.Error)) {
				return err
			}
			return tools.Errorf(tools.KindInvalidInput, "%s: %v", pf.path, err)
		}
		switch pf.kind {
		case patchAdd:
			p.abs = f.resolve(pf.path)
			if p.t, err = loadText(p.abs); err != nil {
				return nil, fail(err)
			}
			if p.t.exists {
				return nil, tools.Errorf(tools.KindInvalidInput, "%s already exists; use *** Update File to change it", f.rel(p.abs))
			}
			if len(pf.lines) > 0 {
				p.updated = strings.Join(pf.lines, "\n") + "\n"
			}
		case patchUpdate:
			p.abs = f.resolve(pf.path)
			if p.t, err = f.loadExisting(p.abs); err != nil {
				return nil, fail(err)
			}
			p.updated = p.t.text
			if len(pf.hunks) > 0 {
				if p.updated, err = applyHunks(p.t.text, normalizeHunks(p.t, pf.hunks)); err != nil {
					return nil, fail(err)
				}
			}
			if pf.moveTo != "" {
				p.dest = f.resolveEntry(pf.moveTo)
				if _, err := os.Lstat(p.dest); err == nil {
					return nil, tools.Errorf(tools.KindInvalidInput, "%s: cannot move to %s, which already exists", pf.path, f.rel(p.dest))
				}
			}
		case patchDelete:
			p.abs = f.resolveEntry(pf.path)
			info, err := f.checkDeletable(p.abs, false)
			if err != nil {
				return nil, fail(err)
			}
			if info.IsDir() {
				return nil, tools.Errorf(tools.KindInvalidInput, "%s is a directory; use fs_delete for directories", f.rel(p.abs))
			}
		}
		plan = append(plan, p)
	}
	return plan, nil
}

// normalizeHunks makes patch lines comparable with the file's text.
func normalizeHunks(t textFile, hs []hunk) []hunk {
	out := make([]hunk, len(hs))
	for i, h := range hs {
		out[i] = h
		out[i].old = normalizeLines(t, h.old)
		out[i].new = normalizeLines(t, h.new)
	}
	return out
}

func normalizeLines(t textFile, ls []string) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = t.normalize(l)
	}
	return out
}

func (f FS) previewApplyPatch(ctx context.Context, a applyPatchArgs) (string, error) {
	plan, err := f.planPatch(a.Patch)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "patch touching %d file(s)\n", len(plan))
	for _, p := range plan {
		name := f.rel(p.abs)
		switch p.pf.kind {
		case patchAdd:
			fmt.Fprintf(&b, "add %s\n%s", name, UnifiedDiff(name, "", p.updated))
		case patchUpdate:
			if p.dest != "" {
				fmt.Fprintf(&b, "update %s -> %s\n", name, f.rel(p.dest))
			} else {
				fmt.Fprintf(&b, "update %s\n", name)
			}
			if p.updated != p.t.text {
				b.WriteString(UnifiedDiff(name, p.t.text, p.updated))
			}
		case patchDelete:
			fmt.Fprintf(&b, "delete %s (moved to the session trash)\n", name)
		}
	}
	return b.String(), nil
}

func (f FS) applyPatch(ctx context.Context, a applyPatchArgs) (tools.Result, error) {
	plan, err := f.planPatch(a.Patch)
	if err != nil {
		return tools.Result{}, err
	}
	for _, p := range plan {
		for _, path := range []string{p.abs, p.dest} {
			if path == "" {
				continue
			}
			if err := f.checkWritable(ctx, path); err != nil {
				return tools.Result{}, err
			}
		}
	}

	var done []Change
	var out strings.Builder
	rollback := func(cause error) (tools.Result, error) {
		for i := len(done) - 1; i >= 0; i-- {
			if err := f.revert(ctx, done[i]); err == nil {
				f.State.record(Change{Op: "undo", Ref: done[i].Seq, Path: done[i].Path, Dest: done[i].Dest})
			} else {
				cause = fmt.Errorf("%w; rollback of %s also failed: %v", cause, f.describe(done[i]), err)
			}
		}
		return tools.Result{}, fmt.Errorf("patch not applied, earlier files were restored: %w", cause)
	}
	for _, p := range plan {
		switch p.pf.kind {
		case patchAdd:
			res, err := f.commit(p.t, p.updated, "write")
			if err != nil {
				return rollback(err)
			}
			done = append(done, f.State.last())
			out.WriteString(res.Output)
		case patchUpdate:
			if p.updated != p.t.text {
				res, err := f.commit(p.t, p.t.encode(p.updated), "patch")
				if err != nil {
					return rollback(err)
				}
				done = append(done, f.State.last())
				out.WriteString(res.Output)
			}
			if p.dest != "" {
				c, err := f.moveFile(p.abs, p.dest)
				if err != nil {
					return rollback(err)
				}
				done = append(done, c)
				fmt.Fprintf(&out, "moved %s -> %s\n", f.rel(p.abs), f.rel(p.dest))
			}
		case patchDelete:
			trash, err := f.toTrash(p.abs)
			if err != nil {
				return rollback(err)
			}
			f.State.forget(p.abs)
			done = append(done, f.State.record(Change{Op: "delete", Path: p.abs, Trash: trash}))
			fmt.Fprintf(&out, "deleted %s (moved to the session trash)\n", f.rel(p.abs))
		}
	}
	fmt.Fprintf(&out, "applied patch: %d change(s); fs_undo with steps=%d reverts it\n", len(done), len(done))
	return tools.Result{Output: out.String(), Mutated: true}, nil
}

// moveFile renames a file the patch updated, keeping its read record.
func (f FS) moveFile(src, dest string) (Change, error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return Change{}, mapErr(err)
	}
	if err := moveAny(src, dest); err != nil {
		return Change{}, err
	}
	f.State.moved(src, dest)
	return f.State.record(Change{Op: "move", Path: src, Dest: dest}), nil
}
