package filesystem

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"scoutclaw/internal/policy"
	"scoutclaw/internal/tools"
)

// checkWritable is the tool-level second line of defence behind the policy engine.
func (f FS) checkWritable(ctx context.Context, abs string) error {
	if policy.IsSensitive(abs) {
		return tools.Errorf(tools.KindDenied, "%s is a credential or key file", abs)
	}
	if !f.Scope.Contains(abs) && !tools.GrantFrom(ctx).OutsideWorkspace {
		return tools.Errorf(tools.KindDenied, "%s is outside the workspace %s and no approval grant was supplied", abs, f.Scope.Root)
	}
	return nil
}

func readExisting(abs string) (content string, exists bool, mode os.FileMode, err error) {
	st, err := os.Stat(abs)
	if os.IsNotExist(err) {
		return "", false, 0o644, nil
	}
	if err != nil {
		return "", false, 0, mapErr(err)
	}
	if st.IsDir() {
		return "", true, 0, tools.Errorf(tools.KindInvalidInput, "%s is a directory", abs)
	}
	if st.Size() > maxEditSize {
		return "", true, 0, tools.Errorf(tools.KindInvalidInput, "%s is larger than %d bytes", abs, maxEditSize)
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return "", true, 0, mapErr(err)
	}
	if strings.IndexByte(string(b), 0) >= 0 {
		return "", true, 0, tools.Errorf(tools.KindInvalidInput, "%s is a binary file", abs)
	}
	return string(b), true, st.Mode().Perm(), nil
}

func (f FS) previewWrite(ctx context.Context, a writeArgs) (string, error) {
	abs := f.resolve(a.Path)
	old, exists, _, err := readExisting(abs)
	if err != nil {
		return "", err
	}
	label := "new file"
	if exists {
		label = "overwrite"
	}
	return fmt.Sprintf("%s %s\n%s", label, abs, UnifiedDiff(abs, old, a.Content)), nil
}

func (f FS) write(ctx context.Context, a writeArgs) (tools.Result, error) {
	abs := f.resolve(a.Path)
	if err := f.checkWritable(ctx, abs); err != nil {
		return tools.Result{}, err
	}
	old, exists, mode, err := readExisting(abs)
	if err != nil {
		return tools.Result{}, err
	}
	if exists && old == a.Content {
		return tools.Result{Output: "no change: file already has this content\n"}, nil
	}
	return f.commit(abs, old, a.Content, exists, mode)
}

func (f FS) patchedContent(abs string, a patchArgs) (old, updated string, mode os.FileMode, err error) {
	old, exists, mode, err := readExisting(abs)
	if err != nil {
		return "", "", 0, err
	}
	if !exists {
		return "", "", 0, tools.Errorf(tools.KindNotFound, "%s does not exist; use fs_write to create it", abs)
	}
	n := strings.Count(old, a.OldString)
	switch {
	case n == 0:
		return "", "", 0, tools.Errorf(tools.KindInvalidInput, "old_string not found in %s; re-read the file and copy the text exactly", abs)
	case n > 1 && !a.ReplaceAll:
		return "", "", 0, tools.Errorf(tools.KindInvalidInput, "old_string matches %d places in %s; add surrounding context to make it unique or set replace_all", n, abs)
	}
	count := 1
	if a.ReplaceAll {
		count = -1
	}
	return old, strings.Replace(old, a.OldString, a.NewString, count), mode, nil
}

func (f FS) previewPatch(ctx context.Context, a patchArgs) (string, error) {
	abs := f.resolve(a.Path)
	old, updated, _, err := f.patchedContent(abs, a)
	if err != nil {
		return "", err
	}
	return "edit " + abs + "\n" + UnifiedDiff(abs, old, updated), nil
}

func (f FS) patch(ctx context.Context, a patchArgs) (tools.Result, error) {
	abs := f.resolve(a.Path)
	if err := f.checkWritable(ctx, abs); err != nil {
		return tools.Result{}, err
	}
	old, updated, mode, err := f.patchedContent(abs, a)
	if err != nil {
		return tools.Result{}, err
	}
	return f.commit(abs, old, updated, true, mode)
}

// commit backs up the old file, then replaces it atomically.
func (f FS) commit(abs, old, updated string, existed bool, mode os.FileMode) (tools.Result, error) {
	meta := map[string]string{"path": abs}
	if existed && f.BackupDir != "" {
		dir := filepath.Join(f.BackupDir, f.SessionID)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return tools.Result{}, fmt.Errorf("cannot create backup dir (refusing to edit without a backup): %w", err)
		}
		bp := filepath.Join(dir, fmt.Sprintf("%s-%s", time.Now().UTC().Format("20060102T150405.000"), filepath.Base(abs)))
		if err := os.WriteFile(bp, []byte(old), 0o600); err != nil {
			return tools.Result{}, fmt.Errorf("backup failed (refusing to edit): %w", err)
		}
		meta["backup"] = bp
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return tools.Result{}, mapErr(err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".scoutclaw-*")
	if err != nil {
		return tools.Result{}, mapErr(err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(updated); err != nil {
		tmp.Close()
		return tools.Result{}, err
	}
	if err := tmp.Close(); err != nil {
		return tools.Result{}, err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return tools.Result{}, err
	}
	if err := os.Rename(tmp.Name(), abs); err != nil {
		return tools.Result{}, mapErr(err)
	}
	out := UnifiedDiff(abs, old, updated)
	if b := meta["backup"]; b != "" {
		out += "backup of previous contents: " + b + "\n"
	}
	return tools.Result{Output: out, Mutated: true, Metadata: meta}, nil
}

type diffOp struct {
	kind byte // ' ', '-', '+'
	text string
}

// UnifiedDiff renders a compact unified diff with 3 lines of context.
func UnifiedDiff(name, a, b string) string {
	al, bl := splitLines(a), splitLines(b)
	// LCS table (files are capped at 2 MiB; guard the quadratic cost).
	if len(al)*len(bl) > 4_000_000 {
		return fmt.Sprintf("--- %s\n+++ %s\n[diff omitted: file too large; %d -> %d lines]\n", name, name, len(al), len(bl))
	}
	dp := make([][]int32, len(al)+1)
	for i := range dp {
		dp[i] = make([]int32, len(bl)+1)
	}
	for i := len(al) - 1; i >= 0; i-- {
		for j := len(bl) - 1; j >= 0; j-- {
			if al[i] == bl[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < len(al) && j < len(bl) {
		switch {
		case al[i] == bl[j]:
			ops = append(ops, diffOp{' ', al[i]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, diffOp{'-', al[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', bl[j]})
			j++
		}
	}
	for ; i < len(al); i++ {
		ops = append(ops, diffOp{'-', al[i]})
	}
	for ; j < len(bl); j++ {
		ops = append(ops, diffOp{'+', bl[j]})
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", name, name)
	const ctx = 3
	changed := false
	for k := 0; k < len(ops); {
		if ops[k].kind == ' ' {
			k++
			continue
		}
		changed = true
		start := max(k-ctx, 0)
		end := k
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			next := end
			for next < len(ops) && ops[next].kind == ' ' {
				next++
			}
			if next >= len(ops) || next-end > 2*ctx {
				end = min(end+ctx, len(ops))
				break
			}
			end = next
		}
		fmt.Fprintf(&sb, "@@ around line %d @@\n", lineNo(ops, start))
		for _, o := range ops[start:end] {
			sb.WriteString(string(o.kind) + o.text + "\n")
		}
		k = end
	}
	if !changed {
		return "(no differences)\n"
	}
	return sb.String()
}

// lineNo returns the old-file line number at op index idx.
func lineNo(ops []diffOp, idx int) int {
	n := 1
	for _, o := range ops[:idx] {
		if o.kind != '+' {
			n++
		}
	}
	return n
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}
