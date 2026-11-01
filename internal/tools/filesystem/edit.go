package filesystem

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"scoutclaw/internal/policy"
	"scoutclaw/internal/tools"
)

const utf8BOM = "\ufeff"

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

// textFile is a text file as loaded for editing. Edits work on text, which has
// the BOM removed and CRLF turned into LF when the whole file uses CRLF;
// encode puts the file's own format back.
type textFile struct {
	abs    string
	exists bool
	mode   os.FileMode
	raw    string // bytes on disk
	text   string
	bom    bool
	crlf   bool
}

func (t textFile) encode(s string) string {
	if t.crlf {
		s = strings.ReplaceAll(s, "\n", "\r\n")
	}
	if t.bom {
		s = utf8BOM + s
	}
	return s
}

// normalize prepares model-supplied text for comparison with t.text.
func (t textFile) normalize(s string) string {
	if strings.Contains(t.text, "\r") { // mixed line endings: compare verbatim
		return s
	}
	return strings.ReplaceAll(s, "\r\n", "\n")
}

func loadText(abs string) (textFile, error) {
	t := textFile{abs: abs, mode: 0o644}
	st, err := os.Stat(abs)
	if os.IsNotExist(err) {
		return t, nil
	}
	if err != nil {
		return t, mapErr(err)
	}
	if st.IsDir() {
		return t, tools.Errorf(tools.KindInvalidInput, "%s is a directory", abs)
	}
	if st.Size() > maxEditSize {
		return t, tools.Errorf(tools.KindInvalidInput, "%s is larger than %d bytes", abs, maxEditSize)
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return t, mapErr(err)
	}
	if strings.IndexByte(string(b), 0) >= 0 {
		return t, tools.Errorf(tools.KindInvalidInput, "%s is a binary file", abs)
	}
	t.exists, t.mode, t.raw = true, st.Mode().Perm(), string(b)
	t.text = t.raw
	if strings.HasPrefix(t.text, utf8BOM) {
		t.bom, t.text = true, strings.TrimPrefix(t.text, utf8BOM)
	}
	if n := strings.Count(t.text, "\n"); n > 0 && strings.Count(t.text, "\r\n") == n {
		t.crlf, t.text = true, strings.ReplaceAll(t.text, "\r\n", "\n")
	}
	return t, nil
}

// loadForEdit loads a file and, if it exists, requires that the agent has read
// its current contents.
func (f FS) loadForEdit(abs string) (textFile, error) {
	t, err := loadText(abs)
	if err != nil || !t.exists {
		return t, err
	}
	return t, f.State.checkFresh(abs, t.raw)
}

// loadExisting is loadForEdit for tools that only edit existing files.
func (f FS) loadExisting(abs string) (textFile, error) {
	t, err := f.loadForEdit(abs)
	if err == nil && !t.exists {
		err = tools.Errorf(tools.KindNotFound, "%s does not exist; use fs_write to create it", abs)
	}
	return t, err
}

// writtenContent returns the new raw contents for fs_write. Overwrites keep
// the existing file's BOM and line endings.
func (t textFile) writtenContent(content string) string {
	if !t.exists {
		return content
	}
	return t.encode(t.normalize(strings.TrimPrefix(content, utf8BOM)))
}

func (f FS) previewWrite(ctx context.Context, a writeArgs) (string, error) {
	abs := f.resolve(a.Path)
	t, err := f.loadForEdit(abs)
	if err != nil {
		return "", err
	}
	label := "new file"
	if t.exists {
		label = "overwrite"
	}
	return fmt.Sprintf("%s %s\n%s", label, f.rel(abs), UnifiedDiff(f.rel(abs), t.text, t.decode(t.writtenContent(a.Content)))), nil
}

// decode is the inverse of encode, for diffs against new raw contents.
func (t textFile) decode(raw string) string {
	s := strings.TrimPrefix(raw, utf8BOM)
	if t.crlf {
		s = strings.ReplaceAll(s, "\r\n", "\n")
	}
	return s
}

func (f FS) write(ctx context.Context, a writeArgs) (tools.Result, error) {
	abs := f.resolve(a.Path)
	if err := f.checkWritable(ctx, abs); err != nil {
		return tools.Result{}, err
	}
	t, err := f.loadForEdit(abs)
	if err != nil {
		return tools.Result{}, err
	}
	updated := t.writtenContent(a.Content)
	if t.exists && t.raw == updated {
		return tools.Result{Output: "no change: file already has this content\n"}, nil
	}
	return f.commit(t, updated, "write")
}

// patchedContent applies the edits in order to t.text and returns the new text.
func patchedContent(t textFile, edits []editPair) (string, error) {
	text := t.text
	for i, e := range edits {
		where := ""
		if len(edits) > 1 {
			where = fmt.Sprintf("edit %d: ", i+1)
		}
		old, repl := t.normalize(e.OldString), t.normalize(e.NewString)
		n := strings.Count(text, old)
		switch {
		case n == 0:
			return "", tools.Errorf(tools.KindInvalidInput, "%sold_string not found in %s; re-read the file and copy the text exactly.%s", where, t.abs, closeMatch(text, old))
		case n > 1 && !e.ReplaceAll:
			return "", tools.Errorf(tools.KindInvalidInput, "%sold_string matches %d places in %s; add surrounding context to make it unique or set replace_all", where, n, t.abs)
		}
		count := 1
		if e.ReplaceAll {
			count = -1
		}
		text = strings.Replace(text, old, repl, count)
	}
	if text == t.text {
		return "", tools.Errorf(tools.KindInvalidInput, "the edits leave %s unchanged", t.abs)
	}
	return text, nil
}

// closeMatch explains near misses so the model can correct its old_string.
func closeMatch(text, old string) string {
	if strings.TrimSpace(old) == "" {
		return ""
	}
	fl := strings.Split(text, "\n")
	ol := strings.Split(strings.Trim(old, "\n"), "\n")
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	for i := 0; i+len(ol) <= len(fl); i++ {
		match := true
		for j := range ol {
			if norm(fl[i+j]) != norm(ol[j]) {
				match = false
				break
			}
		}
		if match {
			block := fl[i : i+len(ol)]
			if len(block) > 30 {
				block = append(block[:30:30], "…")
			}
			return fmt.Sprintf(" It matches lines %d-%d if whitespace is ignored; the exact text there is:\n%s", i+1, i+len(ol), strings.Join(block, "\n"))
		}
	}
	first := ""
	for _, l := range ol {
		if first = norm(l); first != "" {
			break
		}
	}
	if first == "" {
		return ""
	}
	var at []string
	for i, l := range fl {
		if norm(l) == first {
			at = append(at, strconv.Itoa(i+1))
			if len(at) == 5 {
				break
			}
		}
	}
	if len(at) == 0 {
		return ""
	}
	return fmt.Sprintf(" Its first line appears at line(s) %s, but the lines after it differ.", strings.Join(at, ", "))
}

func (f FS) previewPatch(ctx context.Context, a patchArgs) (string, error) {
	abs := f.resolve(a.Path)
	t, err := f.loadExisting(abs)
	if err != nil {
		return "", err
	}
	updated, err := patchedContent(t, a.Edits)
	if err != nil {
		return "", err
	}
	return "edit " + f.rel(abs) + "\n" + UnifiedDiff(f.rel(abs), t.text, updated), nil
}

func (f FS) patch(ctx context.Context, a patchArgs) (tools.Result, error) {
	abs := f.resolve(a.Path)
	if err := f.checkWritable(ctx, abs); err != nil {
		return tools.Result{}, err
	}
	t, err := f.loadExisting(abs)
	if err != nil {
		return tools.Result{}, err
	}
	updated, err := patchedContent(t, a.Edits)
	if err != nil {
		return tools.Result{}, err
	}
	return f.commit(t, t.encode(updated), "patch")
}

// backup copies data into the session's backup directory and returns its path.
func (f FS) backup(abs string, data []byte) (string, error) {
	if f.BackupDir == "" {
		return "", nil
	}
	dir := filepath.Join(f.BackupDir, f.SessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("cannot create backup dir (refusing to edit without a backup): %w", err)
	}
	bp := filepath.Join(dir, fmt.Sprintf("%s-%s", time.Now().UTC().Format("20060102T150405.000000"), filepath.Base(abs)))
	if err := os.WriteFile(bp, data, 0o600); err != nil {
		return "", fmt.Errorf("backup failed (refusing to edit): %w", err)
	}
	return bp, nil
}

// writeAtomic replaces abs with data via a temporary file and rename.
func writeAtomic(abs string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return mapErr(err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".scoutclaw-*")
	if err != nil {
		return mapErr(err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return mapErr(os.Rename(tmp.Name(), abs))
}

// commit backs up the old file, replaces it atomically and journals the change.
func (f FS) commit(t textFile, updated, op string) (tools.Result, error) {
	meta := map[string]string{"path": t.abs}
	c := Change{Op: op, Path: t.abs, Created: !t.exists, After: hashOf(updated)}
	if t.exists {
		bp, err := f.backup(t.abs, []byte(t.raw))
		if err != nil {
			return tools.Result{}, err
		}
		c.Backup, c.Before = bp, hashOf(t.raw)
		if bp != "" {
			meta["backup"] = bp
		}
	}
	if err := writeAtomic(t.abs, []byte(updated), t.mode); err != nil {
		return tools.Result{}, err
	}
	f.State.sawContent(t.abs, updated)
	c = f.State.record(c)
	meta["change"] = strconv.Itoa(c.Seq)

	diff := UnifiedDiff(f.rel(t.abs), t.text, t.decode(updated))
	added, removed := diffStat(diff)
	verb := "edited"
	if !t.exists {
		verb = "created"
	}
	out := fmt.Sprintf("%s %s (+%d -%d)\n%s", verb, f.rel(t.abs), added, removed, diff)
	if c.Backup != "" {
		out += "backup of previous contents: " + c.Backup + "\n"
	}
	return tools.Result{Output: out, Mutated: true, Metadata: meta}, nil
}
