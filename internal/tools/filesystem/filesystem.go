// Package filesystem provides inspection and (policy-gated) editing tools.
package filesystem

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"scoutclaw/internal/policy"
	"scoutclaw/internal/tools"
)

type FS struct {
	Scope      policy.Scope
	BackupDir  string // where pre-edit copies are stored
	SessionID  string
	MaxReadLen int // bytes returned by fs_read
}

const maxEditSize = 2 << 20

type listArgs struct {
	Path       string `json:"path"`
	Depth      int    `json:"depth"`
	MaxEntries int    `json:"max_entries"`
}
type readArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset_line"`
	Limit  int    `json:"limit_lines"`
}
type statArgs struct {
	Path string `json:"path"`
}
type searchArgs struct {
	Path       string `json:"path"`
	Pattern    string `json:"pattern"`
	NameGlob   string `json:"name_glob"`
	MaxResults int    `json:"max_results"`
	IgnoreCase bool   `json:"ignore_case"`
}
type writeArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}
type patchArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

func needPath(p string) error {
	if strings.TrimSpace(p) == "" {
		return errors.New("path is required")
	}
	if strings.ContainsRune(p, 0) {
		return errors.New("path contains NUL")
	}
	return nil
}

func (f FS) Tools() []tools.Tool {
	return []tools.Tool{
		&tools.Typed[listArgs]{
			ToolName: "fs_list",
			Desc:     "List directory contents (name, type, size, modified time). depth 1-3 recurses. Skips .git and node_modules when recursing.",
			Schema:   `{"type":"object","properties":{"path":{"type":"string","description":"Directory (default: workspace)."},"depth":{"type":"integer","minimum":1,"maximum":3},"max_entries":{"type":"integer"}},"additionalProperties":false}`,
			ValidateFn: func(a *listArgs) error {
				if a.Path == "" {
					a.Path = "."
				}
				if a.Depth < 0 || a.Depth > 3 {
					return errors.New("depth must be 1-3")
				}
				return needPath(a.Path)
			},
			Run: f.list,
		},
		&tools.Typed[readArgs]{
			ToolName: "fs_read",
			Desc:     "Read a text file with line numbers. Use offset_line/limit_lines for large files and logs (default 300 lines). Credential files are refused.",
			Schema:   `{"type":"object","properties":{"path":{"type":"string"},"offset_line":{"type":"integer","description":"1-based first line (default 1). Negative values read from the end, e.g. -100 = last 100 lines."},"limit_lines":{"type":"integer"}},"required":["path"],"additionalProperties":false}`,
			ValidateFn: func(a *readArgs) error {
				if a.Limit < 0 {
					return errors.New("limit_lines must be >= 0")
				}
				return needPath(a.Path)
			},
			Run: f.read,
		},
		&tools.Typed[statArgs]{
			ToolName:   "fs_stat",
			Desc:       "Show file metadata: type, size, permissions, owner, modified time, symlink target.",
			Schema:     `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`,
			ValidateFn: func(a *statArgs) error { return needPath(a.Path) },
			Run:        f.stat,
		},
		&tools.Typed[searchArgs]{
			ToolName: "fs_search",
			Desc:     "Search for files by name glob and/or text by regular expression (Go RE2 syntax). Skips .git, node_modules and binary files.",
			Schema:   `{"type":"object","properties":{"path":{"type":"string","description":"Root directory (default: workspace)."},"pattern":{"type":"string","description":"Regex to search inside files."},"name_glob":{"type":"string","description":"Glob on file names, e.g. *.log"},"max_results":{"type":"integer"},"ignore_case":{"type":"boolean"}},"additionalProperties":false}`,
			ValidateFn: func(a *searchArgs) error {
				if a.Path == "" {
					a.Path = "."
				}
				if a.Pattern == "" && a.NameGlob == "" {
					return errors.New("pattern or name_glob is required")
				}
				if a.Pattern != "" {
					if _, err := regexp.Compile(a.Pattern); err != nil {
						return fmt.Errorf("bad pattern: %v", err)
					}
				}
				if a.NameGlob != "" {
					if _, err := filepath.Match(a.NameGlob, "x"); err != nil {
						return fmt.Errorf("bad name_glob: %v", err)
					}
				}
				return needPath(a.Path)
			},
			Run: f.search,
		},
		&tools.Typed[writeArgs]{
			ToolName:   "fs_write",
			Desc:       "Create or fully overwrite a text file. Shows a diff for approval and keeps a backup of any previous contents. Prefer fs_patch for edits to existing files.",
			Schema:     `{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"],"additionalProperties":false}`,
			ValidateFn: func(a *writeArgs) error { return validateWrite(a.Path, a.Content) },
			Run:        f.write,
			PreviewFn:  f.previewWrite,
		},
		&tools.Typed[patchArgs]{
			ToolName: "fs_patch",
			Desc:     "Replace exact text in an existing file. old_string must match exactly once (or set replace_all). Shows a diff for approval and keeps a backup.",
			Schema:   `{"type":"object","properties":{"path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"},"replace_all":{"type":"boolean"}},"required":["path","old_string","new_string"],"additionalProperties":false}`,
			ValidateFn: func(a *patchArgs) error {
				if a.OldString == "" {
					return errors.New("old_string must not be empty")
				}
				if a.OldString == a.NewString {
					return errors.New("old_string and new_string are identical")
				}
				return needPath(a.Path)
			},
			Run:       f.patch,
			PreviewFn: f.previewPatch,
		},
	}
}

func validateWrite(path, content string) error {
	if len(content) > maxEditSize {
		return fmt.Errorf("content exceeds %d bytes", maxEditSize)
	}
	if strings.ContainsRune(content, 0) {
		return errors.New("content contains NUL bytes; only text files can be written")
	}
	return needPath(path)
}

func (f FS) resolve(p string) string { return f.Scope.Resolve(p, "") }

func (f FS) list(ctx context.Context, a listArgs) (tools.Result, error) {
	root := f.resolve(a.Path)
	depth := max(a.Depth, 1)
	limit := a.MaxEntries
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	st, err := os.Stat(root)
	if err != nil {
		return tools.Result{}, mapErr(err)
	}
	if !st.IsDir() {
		return tools.Result{}, tools.Errorf(tools.KindInvalidInput, "%s is not a directory", root)
	}
	var b strings.Builder
	count, truncated := 0, false
	var walk func(dir string, level int)
	walk = func(dir string, level int) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			fmt.Fprintf(&b, "%s[unreadable: %v]\n", strings.Repeat("  ", level-1), err)
			return
		}
		for _, e := range entries {
			if ctx.Err() != nil {
				return
			}
			if count >= limit {
				truncated = true
				return
			}
			count++
			info, _ := e.Info()
			kind, size, mod := "file", int64(0), ""
			if info != nil {
				size, mod = info.Size(), info.ModTime().Format("2006-01-02 15:04")
				switch {
				case info.Mode()&fs.ModeSymlink != 0:
					kind = "link"
				case info.IsDir():
					kind = "dir"
				}
			}
			fmt.Fprintf(&b, "%s%-5s %10d  %s  %s\n", strings.Repeat("  ", level-1), kind, size, mod, e.Name())
			if kind == "dir" && level < depth && e.Name() != ".git" && e.Name() != "node_modules" {
				walk(filepath.Join(dir, e.Name()), level+1)
			}
		}
	}
	walk(root, 1)
	out := fmt.Sprintf("%s (%d entries shown)\n%s", root, count, b.String())
	if truncated {
		out += fmt.Sprintf("[more entries omitted; limit %d]\n", limit)
	}
	return tools.Result{Output: out, Truncated: truncated}, nil
}

func (f FS) read(ctx context.Context, a readArgs) (tools.Result, error) {
	p := f.resolve(a.Path)
	st, err := os.Stat(p)
	if err != nil {
		return tools.Result{}, mapErr(err)
	}
	if st.IsDir() {
		return tools.Result{}, tools.Errorf(tools.KindInvalidInput, "%s is a directory; use fs_list", p)
	}
	file, err := os.Open(p)
	if err != nil {
		return tools.Result{}, mapErr(err)
	}
	defer file.Close()
	head := make([]byte, 8000)
	n, _ := file.Read(head)
	if bytes.IndexByte(head[:n], 0) >= 0 {
		return tools.Result{}, tools.Errorf(tools.KindInvalidInput, "%s looks like a binary file (%d bytes); not reading it", p, st.Size())
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return tools.Result{}, err
	}
	limit := a.Limit
	if limit == 0 {
		limit = 300
	}
	limit = min(limit, 2000)
	maxBytes := f.MaxReadLen
	if maxBytes <= 0 {
		maxBytes = 32 * 1024
	}

	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	var lines []string
	total := 0
	start := a.Offset
	if start == 0 {
		start = 1
	}
	if start < 0 { // tail mode: keep a sliding window
		window := min(-start, limit)
		if -start > limit {
			limit = min(-start, 2000)
			window = limit
		}
		for sc.Scan() {
			total++
			lines = append(lines, sc.Text())
			if len(lines) > window {
				lines = lines[1:]
			}
		}
		start = total - len(lines) + 1
	} else {
		for sc.Scan() {
			total++
			if total >= start && len(lines) < limit {
				lines = append(lines, sc.Text())
			}
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, bufio.ErrTooLong) {
		return tools.Result{}, err
	}
	var b strings.Builder
	used, truncated := 0, false
	for i, l := range lines {
		row := fmt.Sprintf("%6d\t%s\n", start+i, l)
		if used+len(row) > maxBytes {
			truncated = true
			break
		}
		used += len(row)
		b.WriteString(row)
	}
	shown := strings.Count(b.String(), "\n")
	header := fmt.Sprintf("%s: lines %d-%d of %d\n", p, start, start+shown-1, total)
	if shown == 0 {
		header = fmt.Sprintf("%s: empty or offset beyond end (%d lines)\n", p, total)
	}
	if start+shown-1 < total {
		truncated = true
	}
	return tools.Result{Output: header + b.String(), Truncated: truncated}, nil
}

func (f FS) stat(ctx context.Context, a statArgs) (tools.Result, error) {
	p := f.resolve(a.Path)
	li, err := os.Lstat(p)
	if err != nil {
		return tools.Result{}, mapErr(err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "path: %s\ntype: %s\nsize: %d\nmode: %s\nmodified: %s\n", p, typeName(li.Mode()), li.Size(), li.Mode().Perm(), li.ModTime().Format(time.RFC3339))
	if li.Mode()&fs.ModeSymlink != 0 {
		if t, err := os.Readlink(p); err == nil {
			fmt.Fprintf(&b, "symlink_target: %s\n", t)
		}
	}
	b.WriteString(ownerInfo(li))
	fmt.Fprintf(&b, "in_workspace: %v\n", f.Scope.Contains(p))
	return tools.Result{Output: b.String()}, nil
}

func typeName(m fs.FileMode) string {
	switch {
	case m&fs.ModeSymlink != 0:
		return "symlink"
	case m.IsDir():
		return "directory"
	case m.IsRegular():
		return "file"
	}
	return "special"
}

var skipDirs = map[string]bool{".git": true, "node_modules": true, ".venv": true, "__pycache__": true}

func (f FS) search(ctx context.Context, a searchArgs) (tools.Result, error) {
	root := f.resolve(a.Path)
	limit := a.MaxResults
	if limit <= 0 || limit > 300 {
		limit = 100
	}
	var re *regexp.Regexp
	if a.Pattern != "" {
		expr := a.Pattern
		if a.IgnoreCase {
			expr = "(?i)" + expr
		}
		re = regexp.MustCompile(expr)
	}
	var b strings.Builder
	found, scanned := 0, 0
	stop := errors.New("limit")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if a.NameGlob != "" {
			if ok, _ := filepath.Match(a.NameGlob, d.Name()); !ok {
				return nil
			}
		}
		if policy.IsSensitive(path) {
			return nil
		}
		if re == nil {
			found++
			fmt.Fprintln(&b, path)
		} else {
			info, err := d.Info()
			if err != nil || info.Size() > 2<<20 {
				return nil
			}
			scanned++
			matches, err := grepFile(path, re, limit-found)
			if err != nil {
				return nil
			}
			for _, m := range matches {
				fmt.Fprintf(&b, "%s:%d: %s\n", path, m.line, m.text)
				found++
			}
		}
		if found >= limit {
			return stop
		}
		return nil
	})
	if err != nil && !errors.Is(err, stop) {
		return tools.Result{}, err
	}
	out := b.String()
	if found == 0 {
		out = "no matches\n"
	}
	truncated := errors.Is(err, stop)
	if truncated {
		out += fmt.Sprintf("[stopped at %d results]\n", limit)
	}
	return tools.Result{Output: out, Truncated: truncated}, nil
}

type match struct {
	line int
	text string
}

func grepFile(path string, re *regexp.Regexp, max int) ([]match, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	head := make([]byte, 4096)
	n, _ := file.Read(head)
	if bytes.IndexByte(head[:n], 0) >= 0 {
		return nil, nil
	}
	file.Seek(0, io.SeekStart)
	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	var out []match
	for i := 1; sc.Scan(); i++ {
		if re.MatchString(sc.Text()) {
			t := sc.Text()
			if len(t) > 240 {
				t = t[:240] + "…"
			}
			out = append(out, match{i, t})
			if len(out) >= max {
				break
			}
		}
	}
	return out, nil
}

func mapErr(err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return tools.Errorf(tools.KindNotFound, "%v", err)
	case errors.Is(err, fs.ErrPermission):
		return tools.Errorf(tools.KindDenied, "%v (OS permission denied; ScoutClaw does not escalate privileges)", err)
	}
	return err
}
