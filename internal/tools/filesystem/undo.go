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

const maxUndoSteps = 20

type changesArgs struct {
	Path string `json:"path"`
}
type undoArgs struct {
	Steps int    `json:"steps"`
	Path  string `json:"path"`
}

func (f FS) undoTools() []tools.Tool {
	return []tools.Tool{
		&tools.Typed[changesArgs]{
			ToolName: "fs_changes",
			Desc:     "List the file changes ScoutClaw made in this session (edits, creations, copies, moves, deletions) with a combined diff per edited file. Optionally only those under path.",
			Schema:   `{"type":"object","properties":{"path":{"type":"string"}},"additionalProperties":false}`,
			Run: func(ctx context.Context, a changesArgs) (tools.Result, error) {
				abs := ""
				if a.Path != "" {
					abs = f.resolve(a.Path)
				}
				return tools.Result{Output: f.Report(abs)}, nil
			},
		},
		&tools.Typed[undoArgs]{
			ToolName: "fs_undo",
			Desc:     "Undo the most recent file changes ScoutClaw made in this session, newest first (steps, default 1), optionally only changes under path. Refuses if a file was changed again afterwards.",
			Schema:   `{"type":"object","properties":{"steps":{"type":"integer","minimum":1,"maximum":20},"path":{"type":"string"}},"additionalProperties":false}`,
			ValidateFn: func(a *undoArgs) error {
				if a.Steps == 0 {
					a.Steps = 1
				}
				if a.Steps < 1 || a.Steps > maxUndoSteps {
					return fmt.Errorf("steps must be 1-%d", maxUndoSteps)
				}
				return nil
			},
			Run: func(ctx context.Context, a undoArgs) (tools.Result, error) { return f.Undo(ctx, a.Steps, a.Path) },
			PreviewFn: func(ctx context.Context, a undoArgs) (string, error) {
				return f.previewUndo(a.Steps, a.Path)
			},
		},
	}
}

func (f FS) describe(c Change) string {
	s := fmt.Sprintf("#%d %s %s", c.Seq, c.Op, f.rel(c.Path))
	if c.Dest != "" {
		s += " -> " + f.rel(c.Dest)
	}
	if c.Created && (c.Op == "write" || c.Op == "copy") {
		s += " (new)"
	}
	return s
}

// displayText strips a BOM and CRLF so diffs compare content, not encoding.
func displayText(raw string) string {
	return strings.ReplaceAll(strings.TrimPrefix(raw, utf8BOM), "\r\n", "\n")
}

// Report lists the changes still in effect (under abs, if set) and, for each
// edited file, a diff from its contents before the session to now.
func (f FS) Report(abs string) string {
	var list []Change
	for _, c := range f.State.Changes() {
		if abs == "" || c.touches(abs) {
			list = append(list, c)
		}
	}
	if len(list) == 0 {
		return "no file changes recorded in this session\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d change(s) in this session:\n", len(list))
	first := map[string]Change{}
	var order []string
	for _, c := range list {
		fmt.Fprintf(&b, "  %s  (%s)\n", f.describe(c), c.Time.Local().Format("15:04:05"))
		switch c.Op {
		case "write", "patch", "lines":
			if _, ok := first[c.Path]; !ok {
				first[c.Path] = c
				order = append(order, c.Path)
			}
		}
	}
	for _, p := range order {
		c := first[p]
		before := ""
		if !c.Created {
			data, err := os.ReadFile(c.Backup)
			if err != nil {
				fmt.Fprintf(&b, "\n%s: original contents unavailable (%v)\n", f.rel(p), err)
				continue
			}
			before = string(data)
		}
		now, err := os.ReadFile(p)
		if err != nil {
			fmt.Fprintf(&b, "\n%s: no longer exists\n", f.rel(p))
			continue
		}
		diff := UnifiedDiff(f.rel(p), displayText(before), displayText(string(now)))
		added, removed := diffStat(diff)
		fmt.Fprintf(&b, "\n%s (+%d -%d)\n%s", f.rel(p), added, removed, diff)
	}
	return b.String()
}

// pending returns the changes fs_undo would revert, newest first.
func (s *State) pending(steps int, abs string) []Change {
	all := s.Changes()
	var out []Change
	for i := len(all) - 1; i >= 0 && len(out) < steps; i-- {
		if abs == "" || all[i].touches(abs) {
			out = append(out, all[i])
		}
	}
	return out
}

func (f FS) previewUndo(steps int, path string) (string, error) {
	abs := ""
	if path != "" {
		abs = f.resolve(path)
	}
	list := f.State.pending(steps, abs)
	if len(list) == 0 {
		return "", tools.Errorf(tools.KindInvalidInput, "no changes to undo")
	}
	var b strings.Builder
	for _, c := range list {
		fmt.Fprintf(&b, "undo %s\n", f.describe(c))
		if (c.Op == "write" || c.Op == "patch" || c.Op == "lines") && !c.Created {
			cur, err1 := os.ReadFile(c.Path)
			old, err2 := os.ReadFile(c.Backup)
			if err1 == nil && err2 == nil {
				b.WriteString(UnifiedDiff(f.rel(c.Path), displayText(string(cur)), displayText(string(old))))
			}
		}
	}
	return b.String(), nil
}

// Undo reverts the most recent changes (under path, if set), newest first. It
// stops at the first change that cannot be reverted safely.
func (f FS) Undo(ctx context.Context, steps int, path string) (tools.Result, error) {
	abs := ""
	if path != "" {
		abs = f.resolve(path)
	}
	list := f.State.pending(steps, abs)
	if len(list) == 0 {
		return tools.Result{}, tools.Errorf(tools.KindInvalidInput, "no changes to undo")
	}
	var b strings.Builder
	done := 0
	for _, c := range list {
		if err := f.revert(ctx, c); err != nil {
			if done > 0 {
				return tools.Result{Output: b.String(), Mutated: true}, fmt.Errorf("stopped at %s: %w", f.describe(c), err)
			}
			return tools.Result{}, fmt.Errorf("cannot undo %s: %w", f.describe(c), err)
		}
		f.State.record(Change{Op: "undo", Ref: c.Seq, Path: c.Path, Dest: c.Dest})
		fmt.Fprintf(&b, "undid %s\n", f.describe(c))
		done++
	}
	return tools.Result{Output: b.String(), Mutated: true}, nil
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func (f FS) revert(ctx context.Context, c Change) error {
	for _, p := range []string{c.Path, c.Dest} {
		if p != "" {
			if err := f.checkWritable(ctx, p); err != nil {
				return err
			}
		}
	}
	switch c.Op {
	case "write", "patch", "lines":
		cur, err := os.ReadFile(c.Path)
		if err != nil {
			return mapErr(err)
		}
		if hashOf(string(cur)) != c.After {
			return tools.Errorf(tools.KindFailed, "%s was changed again after this edit; fix it with a new edit instead", c.Path)
		}
		f.State.forget(c.Path)
		if c.Created {
			_, err := f.toTrash(c.Path)
			return err
		}
		old, err := os.ReadFile(c.Backup)
		if err != nil {
			return fmt.Errorf("backup %s unreadable: %w", c.Backup, err)
		}
		st, err := os.Stat(c.Path)
		if err != nil {
			return mapErr(err)
		}
		if _, err := f.backup(c.Path, cur); err != nil {
			return err
		}
		return writeAtomic(c.Path, old, st.Mode().Perm())
	case "mkdir":
		for cur := c.Path; ; cur = filepath.Dir(cur) {
			if err := os.Remove(cur); err != nil && !errors.Is(err, os.ErrNotExist) {
				return tools.Errorf(tools.KindFailed, "%s is not empty any more; not removed", cur)
			}
			if cur == c.CreatedDirs || filepath.Dir(cur) == cur {
				return nil
			}
		}
	case "copy":
		if exists(c.Dest) {
			if _, err := f.toTrash(c.Dest); err != nil {
				return err
			}
			f.State.forget(c.Dest)
		}
		if c.Trash != "" {
			return moveAny(c.Trash, c.Dest)
		}
		return nil
	case "move":
		if exists(c.Path) {
			return tools.Errorf(tools.KindFailed, "%s exists again; not moving %s back over it", c.Path, c.Dest)
		}
		if err := os.MkdirAll(filepath.Dir(c.Path), 0o755); err != nil {
			return mapErr(err)
		}
		if err := moveAny(c.Dest, c.Path); err != nil {
			return err
		}
		f.State.moved(c.Dest, c.Path)
		if c.Trash != "" {
			return moveAny(c.Trash, c.Dest)
		}
		return nil
	case "delete":
		if exists(c.Path) {
			return tools.Errorf(tools.KindFailed, "%s exists again; not restoring over it", c.Path)
		}
		if err := os.MkdirAll(filepath.Dir(c.Path), 0o755); err != nil {
			return mapErr(err)
		}
		return moveAny(c.Trash, c.Path)
	}
	return tools.Errorf(tools.KindInternal, "unknown change type %q", c.Op)
}
