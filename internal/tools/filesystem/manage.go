package filesystem

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"scoutclaw/internal/policy"
	"scoutclaw/internal/tools"
)

const maxCopyEntries = 5000

type pathArgs struct {
	Path string `json:"path"`
}
type transferArgs struct {
	Src       string `json:"src"`
	Dest      string `json:"dest"`
	Overwrite bool   `json:"overwrite"`
}
type deleteArgs struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive"`
}

func validateTransfer(a *transferArgs) error {
	if err := needPath(a.Src); err != nil {
		return fmt.Errorf("src: %v", err)
	}
	if err := needPath(a.Dest); err != nil {
		return fmt.Errorf("dest: %v", err)
	}
	return nil
}

const transferSchema = `{"type":"object","properties":{"src":{"type":"string"},"dest":{"type":"string","description":"Full target path (not a directory to put src into)."},"overwrite":{"type":"boolean","description":"Replace an existing dest; the old one is moved to the session trash."}},"required":["src","dest"],"additionalProperties":false}`

func (f FS) manageTools() []tools.Tool {
	return append([]tools.Tool{
		&tools.Typed[pathArgs]{
			ToolName:   "fs_mkdir",
			Desc:       "Create a directory, including missing parent directories. Does nothing if it already exists.",
			Schema:     `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`,
			ValidateFn: func(a *pathArgs) error { return needPath(a.Path) },
			Run:        f.mkdir,
			PreviewFn: func(ctx context.Context, a pathArgs) (string, error) {
				return "create directory " + f.rel(f.resolve(a.Path)), nil
			},
		},
		&tools.Typed[transferArgs]{
			ToolName:   "fs_copy",
			Desc:       "Copy a file or a directory tree (skipping .git and credential files) to dest. Fails if dest exists unless overwrite is set.",
			Schema:     transferSchema,
			ValidateFn: validateTransfer,
			Run:        f.copy,
			PreviewFn:  func(ctx context.Context, a transferArgs) (string, error) { return f.previewTransfer("copy", a) },
		},
		&tools.Typed[transferArgs]{
			ToolName:   "fs_move",
			Desc:       "Move or rename a file or directory to dest. Fails if dest exists unless overwrite is set. Prefer this over editing a file into a new path.",
			Schema:     transferSchema,
			ValidateFn: validateTransfer,
			Run:        f.move,
			PreviewFn:  func(ctx context.Context, a transferArgs) (string, error) { return f.previewTransfer("move", a) },
		},
		&tools.Typed[deleteArgs]{
			ToolName:   "fs_delete",
			Desc:       "Delete a file or directory by moving it to the session trash, so fs_undo can restore it. A non-empty directory needs recursive=true.",
			Schema:     `{"type":"object","properties":{"path":{"type":"string"},"recursive":{"type":"boolean"}},"required":["path"],"additionalProperties":false}`,
			ValidateFn: func(a *deleteArgs) error { return needPath(a.Path) },
			Run:        f.delete,
			PreviewFn:  f.previewDelete,
		},
	}, f.undoTools()...)
}

// resolveEntry resolves a path whose last element is acted on itself (a
// symlink is moved or deleted, not its target).
func (f FS) resolveEntry(p string) string { return f.Scope.ResolveNoFollow(p, "") }

// firstMissing returns the top-most directory of abs that does not exist yet.
func firstMissing(abs string) string {
	cur := abs
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return cur
		}
		if _, err := os.Lstat(parent); err == nil {
			return cur
		}
		cur = parent
	}
}

func (f FS) mkdir(ctx context.Context, a pathArgs) (tools.Result, error) {
	abs := f.resolve(a.Path)
	if err := f.checkWritable(ctx, abs); err != nil {
		return tools.Result{}, err
	}
	if st, err := os.Stat(abs); err == nil {
		if st.IsDir() {
			return tools.Result{Output: "no change: directory already exists: " + f.rel(abs) + "\n"}, nil
		}
		return tools.Result{}, tools.Errorf(tools.KindInvalidInput, "%s exists and is not a directory", abs)
	}
	top := firstMissing(abs)
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return tools.Result{}, mapErr(err)
	}
	f.State.record(Change{Op: "mkdir", Path: abs, Created: true, CreatedDirs: top})
	return tools.Result{Output: "created directory " + f.rel(abs) + "\n", Mutated: true, Metadata: map[string]string{"path": abs}}, nil
}

func within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// prepareTransfer checks src and dest for copy and move.
func (f FS) prepareTransfer(op string, a transferArgs) (src, dest string, srcInfo fs.FileInfo, destExists bool, err error) {
	src, dest = f.resolveEntry(a.Src), f.resolveEntry(a.Dest)
	if policy.IsSensitive(src) {
		return "", "", nil, false, tools.Errorf(tools.KindDenied, "%s is a credential or key file", src)
	}
	if srcInfo, err = os.Lstat(src); err != nil {
		return "", "", nil, false, mapErr(err)
	}
	if src == dest {
		return "", "", nil, false, tools.Errorf(tools.KindInvalidInput, "src and dest are the same path")
	}
	if srcInfo.IsDir() && within(src, dest) {
		return "", "", nil, false, tools.Errorf(tools.KindInvalidInput, "cannot %s %s into itself (%s)", op, src, dest)
	}
	if op == "move" && (src == f.Scope.Root || within(src, f.Scope.Root)) {
		return "", "", nil, false, tools.Errorf(tools.KindDenied, "refusing to move the workspace root or a directory that contains it")
	}
	if _, err := os.Lstat(dest); err == nil {
		if !a.Overwrite {
			return "", "", nil, false, tools.Errorf(tools.KindInvalidInput, "%s already exists; set overwrite to replace it (the old one goes to the session trash)", dest)
		}
		destExists = true
	}
	return src, dest, srcInfo, destExists, nil
}

func (f FS) previewTransfer(op string, a transferArgs) (string, error) {
	src, dest, info, destExists, err := f.prepareTransfer(op, a)
	if err != nil {
		return "", err
	}
	what := "file"
	if info.IsDir() {
		n, err := countEntries(src, op == "copy")
		if err != nil {
			return "", err
		}
		what = fmt.Sprintf("directory (%d entries)", n)
	}
	out := fmt.Sprintf("%s %s %s\n  -> %s\n", op, what, f.rel(src), f.rel(dest))
	if destExists {
		out += "  replaces the existing " + f.rel(dest) + " (moved to the session trash)\n"
	}
	return out, nil
}

func (f FS) copy(ctx context.Context, a transferArgs) (tools.Result, error) {
	src, dest, _, destExists, err := f.prepareTransfer("copy", a)
	if err != nil {
		return tools.Result{}, err
	}
	if err := f.checkWritable(ctx, dest); err != nil {
		return tools.Result{}, err
	}
	if n, err := countEntries(src, true); err != nil {
		return tools.Result{}, err
	} else if n > maxCopyEntries {
		return tools.Result{}, tools.Errorf(tools.KindInvalidInput, "%s has %d entries (limit %d); use a shell command for copies this large", src, n, maxCopyEntries)
	}
	c := Change{Op: "copy", Path: src, Dest: dest, Created: true}
	if destExists {
		if c.Trash, err = f.toTrash(dest); err != nil {
			return tools.Result{}, err
		}
		f.State.forget(dest)
	}
	n, skipped, err := copyTree(ctx, src, dest, true)
	if err != nil {
		os.RemoveAll(dest)
		if c.Trash != "" {
			moveAny(c.Trash, dest)
		}
		return tools.Result{}, err
	}
	f.State.copied(src, dest)
	f.State.record(c)
	out := fmt.Sprintf("copied %s -> %s (%d entries)\n", f.rel(src), f.rel(dest), n)
	if len(skipped) > 0 {
		out += "skipped credential files: " + strings.Join(skipped, ", ") + "\n"
	}
	if c.Trash != "" {
		out += "previous " + f.rel(dest) + " moved to " + c.Trash + "\n"
	}
	return tools.Result{Output: out, Mutated: true, Metadata: map[string]string{"path": dest, "src": src}}, nil
}

func (f FS) move(ctx context.Context, a transferArgs) (tools.Result, error) {
	src, dest, _, destExists, err := f.prepareTransfer("move", a)
	if err != nil {
		return tools.Result{}, err
	}
	for _, p := range []string{src, dest} {
		if err := f.checkWritable(ctx, p); err != nil {
			return tools.Result{}, err
		}
	}
	c := Change{Op: "move", Path: src, Dest: dest}
	if destExists {
		if c.Trash, err = f.toTrash(dest); err != nil {
			return tools.Result{}, err
		}
		f.State.forget(dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return tools.Result{}, mapErr(err)
	}
	if err := moveAny(src, dest); err != nil {
		if c.Trash != "" {
			moveAny(c.Trash, dest)
		}
		return tools.Result{}, err
	}
	f.State.moved(src, dest)
	f.State.record(c)
	out := fmt.Sprintf("moved %s -> %s\n", f.rel(src), f.rel(dest))
	if c.Trash != "" {
		out += "previous " + f.rel(dest) + " moved to " + c.Trash + "\n"
	}
	return tools.Result{Output: out, Mutated: true, Metadata: map[string]string{"path": dest, "src": src}}, nil
}

func (f FS) checkDeletable(abs string, recursive bool) (fs.FileInfo, error) {
	switch {
	case abs == f.Scope.Root || within(abs, f.Scope.Root):
		return nil, tools.Errorf(tools.KindDenied, "refusing to delete the workspace root or a directory that contains it")
	case filepath.Base(abs) == ".git":
		return nil, tools.Errorf(tools.KindDenied, "refusing to delete a .git directory")
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, mapErr(err)
	}
	if info.IsDir() && !recursive {
		entries, err := os.ReadDir(abs)
		if err != nil {
			return nil, mapErr(err)
		}
		if len(entries) > 0 {
			return nil, tools.Errorf(tools.KindInvalidInput, "%s is a directory with %d entries; set recursive to delete it", abs, len(entries))
		}
	}
	return info, nil
}

func (f FS) previewDelete(ctx context.Context, a deleteArgs) (string, error) {
	abs := f.resolveEntry(a.Path)
	info, err := f.checkDeletable(abs, a.Recursive)
	if err != nil {
		return "", err
	}
	what := "file"
	if info.IsDir() {
		n, _ := countEntries(abs, false)
		what = fmt.Sprintf("directory (%d entries)", n)
	}
	return fmt.Sprintf("delete %s %s\n  (moved to the session trash; fs_undo restores it)\n", what, f.rel(abs)), nil
}

func (f FS) delete(ctx context.Context, a deleteArgs) (tools.Result, error) {
	abs := f.resolveEntry(a.Path)
	if err := f.checkWritable(ctx, abs); err != nil {
		return tools.Result{}, err
	}
	if _, err := f.checkDeletable(abs, a.Recursive); err != nil {
		return tools.Result{}, err
	}
	trash, err := f.toTrash(abs)
	if err != nil {
		return tools.Result{}, err
	}
	f.State.forget(abs)
	f.State.record(Change{Op: "delete", Path: abs, Trash: trash})
	return tools.Result{Output: fmt.Sprintf("deleted %s (moved to %s; fs_undo can restore it)\n", f.rel(abs), trash), Mutated: true,
		Metadata: map[string]string{"path": abs, "trash": trash}}, nil
}

// toTrash moves abs into a fresh folder in the session trash.
func (f FS) toTrash(abs string) (string, error) {
	if f.TrashDir == "" {
		return "", tools.Errorf(tools.KindUnavailable, "no trash directory is configured; refusing to remove %s without a recoverable copy", abs)
	}
	base := filepath.Join(f.TrashDir, f.SessionID)
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("cannot create trash dir (refusing to remove %s): %w", abs, err)
	}
	dir, err := os.MkdirTemp(base, time.Now().UTC().Format("20060102T150405")+"-*")
	if err != nil {
		return "", fmt.Errorf("cannot create trash dir (refusing to remove %s): %w", abs, err)
	}
	dest := filepath.Join(dir, filepath.Base(abs))
	if err := moveAny(abs, dest); err != nil {
		os.Remove(dir)
		return "", err
	}
	return dest, nil
}

// rename is os.Rename; tests replace it to exercise the copy fallback.
var rename = os.Rename

// moveAny renames src to dest, falling back to copy and remove when a rename
// is impossible (for example across filesystems). dest must not exist.
func moveAny(src, dest string) error {
	if _, err := os.Lstat(dest); err == nil {
		return tools.Errorf(tools.KindInvalidInput, "%s already exists", dest)
	}
	err := rename(src, dest)
	if err == nil || errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		return mapErr(err)
	}
	if _, _, cerr := copyTree(context.Background(), src, dest, false); cerr != nil {
		os.RemoveAll(dest)
		return fmt.Errorf("move %s -> %s failed: %v (copy fallback: %v)", src, dest, err, cerr)
	}
	return mapErr(os.RemoveAll(src))
}

func countEntries(root string, skipGit bool) (int, error) {
	n := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if skipGit && d.IsDir() && d.Name() == ".git" && p != root {
			return filepath.SkipDir
		}
		n++
		if n > maxCopyEntries*4 {
			return filepath.SkipAll
		}
		return nil
	})
	return n, mapErr(err)
}

// copyTree copies a file, symlink or directory tree. With filtered, .git
// directories and credential files are skipped (and reported).
func copyTree(ctx context.Context, src, dest string, filtered bool) (n int, skipped []string, err error) {
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dest, rel)
		if filtered && p != src && d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if filtered && policy.IsSensitive(p) {
			skipped = append(skipped, p)
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		n++
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case info.Mode().IsRegular():
			return copyFile(p, target, info.Mode().Perm())
		}
		n-- // sockets, devices and pipes are not copied
		return nil
	})
	return n, skipped, mapErr(err)
}

func copyFile(src, dest string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
