package filesystem

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"scoutclaw/internal/policy"
	"scoutclaw/internal/tools"
)

const (
	maxSearchFileSize = 2 << 20
	maxGrepLineLen    = 300
	maxMultilineSpan  = 20 // lines shown per multiline match
	maxReadManyFiles  = 20
)

// fileTypes maps fs_grep's type filter to file name globs.
var fileTypes = map[string][]string{
	"go": {"*.go"}, "py": {"*.py", "*.pyi"}, "js": {"*.js", "*.mjs", "*.cjs", "*.jsx"}, "ts": {"*.ts", "*.tsx", "*.mts", "*.cts"},
	"java": {"*.java"}, "kotlin": {"*.kt", "*.kts"}, "rust": {"*.rs"}, "c": {"*.c", "*.h"}, "cpp": {"*.cc", "*.cpp", "*.cxx", "*.hpp", "*.hh", "*.h"},
	"csharp": {"*.cs"}, "ruby": {"*.rb"}, "php": {"*.php"}, "swift": {"*.swift"}, "sh": {"*.sh", "*.bash", "*.zsh"},
	"ps1": {"*.ps1", "*.psm1"}, "md": {"*.md", "*.markdown"}, "json": {"*.json"}, "yaml": {"*.yaml", "*.yml"}, "toml": {"*.toml"},
	"html": {"*.html", "*.htm"}, "css": {"*.css", "*.scss", "*.sass", "*.less"}, "sql": {"*.sql"}, "proto": {"*.proto"},
}

func fileTypeNames() string {
	var n []string
	for k := range fileTypes {
		n = append(n, k)
	}
	sort.Strings(n)
	return strings.Join(n, ", ")
}

type globArgs struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	MaxResults int    `json:"max_results"`
}

type grepArgs struct {
	Pattern      string `json:"pattern"`
	Path         string `json:"path"`
	Glob         string `json:"glob"`
	Type         string `json:"type"`
	IgnoreCase   bool   `json:"ignore_case"`
	FixedStrings bool   `json:"fixed_strings"`
	Multiline    bool   `json:"multiline"`
	Context      int    `json:"context"`
	Before       int    `json:"before"`
	After        int    `json:"after"`
	Output       string `json:"output"`
	MaxResults   int    `json:"max_results"`
}

type readManyArgs struct {
	Files []readArgs `json:"files"`
}

func (f FS) searchTools() []tools.Tool {
	return []tools.Tool{
		&tools.Typed[globArgs]{
			ToolName: "fs_glob",
			Desc: "Find files by glob pattern, newest first. A pattern without '/' matches file names at any depth (*.go); one with '/' matches the path from the search root (src/**/*_test.go). " +
				"Supports *, ?, **, [abc] and {a,b}. Skips .gitignore'd files, .git and node_modules.",
			Schema: `{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string","description":"Directory to search (default: workspace)."},"max_results":{"type":"integer","description":"Default 200, at most 1000."}},"required":["pattern"],"additionalProperties":false}`,
			ValidateFn: func(a *globArgs) error {
				if strings.TrimSpace(a.Pattern) == "" {
					return errors.New("pattern is required")
				}
				if _, err := compilePathGlob(a.Pattern); err != nil {
					return fmt.Errorf("bad pattern: %v", err)
				}
				if a.Path == "" {
					a.Path = "."
				}
				return needPath(a.Path)
			},
			Run: f.glob,
		},
		&tools.Typed[grepArgs]{
			ToolName: "fs_grep",
			Desc: "Search file contents with a regular expression (Go RE2 syntax) or a fixed string. Skips .gitignore'd, binary and credential files. " +
				"output: content (matching lines with line numbers, default), files (paths only), count (matches per file). " +
				"Narrow with glob (*.go, src/**/*.ts) or type (" + fileTypeNames() + "). context/before/after add surrounding lines; multiline lets a pattern span lines.",
			Schema: `{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string","description":"File or directory (default: workspace)."},` +
				`"glob":{"type":"string"},"type":{"type":"string"},"ignore_case":{"type":"boolean"},"fixed_strings":{"type":"boolean","description":"Treat pattern as literal text."},` +
				`"multiline":{"type":"boolean","description":"Let . and the pattern match across lines."},"context":{"type":"integer","maximum":10},"before":{"type":"integer","maximum":10},"after":{"type":"integer","maximum":10},` +
				`"output":{"type":"string","enum":["content","files","count"]},"max_results":{"type":"integer","description":"Matching lines (content) or files (files, count). Default 100, at most 1000."}},` +
				`"required":["pattern"],"additionalProperties":false}`,
			ValidateFn: validateGrep,
			Run:        f.grep,
		},
		&tools.Typed[readManyArgs]{
			ToolName: "fs_read_many",
			Desc:     "Read several text files (or line ranges) in one call, each like fs_read, sharing one output budget. Use it instead of many fs_read calls when you already know which files you need.",
			Schema: `{"type":"object","properties":{"files":{"type":"array","minItems":1,"maxItems":20,"items":{"type":"object","properties":{"path":{"type":"string"},` +
				`"offset_line":{"type":"integer"},"limit_lines":{"type":"integer"}},"required":["path"],"additionalProperties":false}}},"required":["files"],"additionalProperties":false}`,
			ValidateFn: func(a *readManyArgs) error {
				if len(a.Files) == 0 {
					return errors.New("files must not be empty")
				}
				if len(a.Files) > maxReadManyFiles {
					return fmt.Errorf("at most %d files per call", maxReadManyFiles)
				}
				for i, r := range a.Files {
					if r.Limit < 0 {
						return fmt.Errorf("file %d: limit_lines must be >= 0", i+1)
					}
					if err := needPath(r.Path); err != nil {
						return fmt.Errorf("file %d: %v", i+1, err)
					}
				}
				return nil
			},
			Run: f.readMany,
		},
	}
}

func validateGrep(a *grepArgs) error {
	if a.Pattern == "" {
		return errors.New("pattern is required")
	}
	if _, err := grepRegex(*a); err != nil {
		return fmt.Errorf("bad pattern: %v", err)
	}
	if a.Glob != "" {
		if _, err := compilePathGlob(a.Glob); err != nil {
			return fmt.Errorf("bad glob: %v", err)
		}
	}
	if a.Type != "" && fileTypes[a.Type] == nil {
		return fmt.Errorf("unknown type %q (known: %s)", a.Type, fileTypeNames())
	}
	for name, v := range map[string]int{"context": a.Context, "before": a.Before, "after": a.After} {
		if v < 0 || v > 10 {
			return fmt.Errorf("%s must be 0-10", name)
		}
	}
	switch a.Output {
	case "":
		a.Output = "content"
	case "content", "files", "count":
	default:
		return fmt.Errorf("output must be content, files or count")
	}
	if a.Path == "" {
		a.Path = "."
	}
	return needPath(a.Path)
}

func grepRegex(a grepArgs) (*regexp.Regexp, error) {
	expr := a.Pattern
	if a.FixedStrings {
		expr = regexp.QuoteMeta(expr)
	}
	flags := ""
	if a.IgnoreCase {
		flags += "i"
	}
	if a.Multiline {
		flags += "s"
	}
	if flags != "" {
		expr = "(?" + flags + ")" + expr
	}
	return regexp.Compile(expr)
}

func clampLimit(n, def, hi int) int {
	if n <= 0 {
		return def
	}
	return min(n, hi)
}

// walkFiles calls fn for every regular file under root that is not ignored by
// .gitignore, not in an always-skipped directory and not a credential file.
// A root that is itself a file is visited directly.
func walkFiles(ctx context.Context, root string, fn func(abs, rel string, d fs.DirEntry) error) error {
	st, err := os.Stat(root)
	if err != nil {
		return mapErr(err)
	}
	if !st.IsDir() {
		if policy.IsSensitive(root) {
			return tools.Errorf(tools.KindDenied, "%s is a credential or key file", root)
		}
		return fn(root, filepath.Base(root), fs.FileInfoToDirEntry(st))
	}
	ign := newIgnorer(root)
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return nil // unreadable entries are skipped, not fatal
		}
		if p == root {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] || ign.ignored(p, true) {
				return filepath.SkipDir
			}
			ign.load(p)
			return nil
		}
		if !d.Type().IsRegular() || policy.IsSensitive(p) || ign.ignored(p, false) {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		return fn(p, filepath.ToSlash(rel), d)
	})
}

func (f FS) glob(ctx context.Context, a globArgs) (tools.Result, error) {
	root := f.resolve(a.Path)
	g, _ := compilePathGlob(a.Pattern)
	limit := clampLimit(a.MaxResults, 200, 1000)
	type hit struct {
		path string
		mod  time.Time
	}
	var hits []hit
	err := walkFiles(ctx, root, func(abs, rel string, d fs.DirEntry) error {
		if !g.match(rel) {
			return nil
		}
		var mod time.Time
		if info, err := d.Info(); err == nil {
			mod = info.ModTime()
		}
		hits = append(hits, hit{abs, mod})
		return nil
	})
	if err != nil {
		return tools.Result{}, err
	}
	if len(hits) == 0 {
		return tools.Result{Output: "no files match " + a.Pattern + "\n"}, nil
	}
	sort.Slice(hits, func(i, j int) bool {
		if !hits[i].mod.Equal(hits[j].mod) {
			return hits[i].mod.After(hits[j].mod)
		}
		return hits[i].path < hits[j].path
	})
	var b strings.Builder
	for i, h := range hits {
		if i == limit {
			break
		}
		b.WriteString(f.rel(h.path) + "\n")
	}
	truncated := len(hits) > limit
	if truncated {
		fmt.Fprintf(&b, "[%d of %d files shown; narrow the pattern or path]\n", limit, len(hits))
	}
	return tools.Result{Output: b.String(), Truncated: truncated}, nil
}

// grepFileResult is what one file contributed to fs_grep.
type grepFileResult struct {
	lines   []string // file lines (only kept for content output)
	matches []int    // 0-based line indexes of matches (start lines for multiline)
	spans   [][2]int // multiline: first and last line of each match
}

func (f FS) grep(ctx context.Context, a grepArgs) (tools.Result, error) {
	root := f.resolve(a.Path)
	re, _ := grepRegex(a)
	var nameGlobs []pathGlob
	if a.Glob != "" {
		g, _ := compilePathGlob(a.Glob)
		nameGlobs = append(nameGlobs, g)
	}
	var typeGlobs []pathGlob
	for _, t := range fileTypes[a.Type] {
		g, _ := compilePathGlob(t)
		typeGlobs = append(typeGlobs, g)
	}
	var files []string
	err := walkFiles(ctx, root, func(abs, rel string, d fs.DirEntry) error {
		if len(nameGlobs) > 0 && !nameGlobs[0].match(rel) {
			return nil
		}
		if len(typeGlobs) > 0 && !anyMatch(typeGlobs, rel) {
			return nil
		}
		files = append(files, abs)
		return nil
	})
	if err != nil {
		return tools.Result{}, err
	}
	sort.Strings(files)

	// Scan in parallel, then report in path order so output is deterministic.
	results := make([]*grepFileResult, len(files))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < min(runtime.NumCPU(), 8); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				results[i] = grepOne(files[i], re, a.Multiline, a.Output == "content")
			}
		}()
	}
	for i := range files {
		if ctx.Err() != nil {
			break
		}
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return tools.Result{}, err
	}

	limit := clampLimit(a.MaxResults, 100, 1000)
	before, after := max(a.Before, a.Context), max(a.After, a.Context)
	var b strings.Builder
	shown, totalMatches, matchedFiles := 0, 0, 0
	truncated := false
	for i, r := range results {
		if r == nil || len(r.matches) == 0 {
			continue
		}
		matchedFiles++
		totalMatches += len(r.matches)
		if truncated {
			continue
		}
		name := f.rel(files[i])
		switch a.Output {
		case "files":
			if shown == limit {
				truncated = true
				continue
			}
			b.WriteString(name + "\n")
			shown++
		case "count":
			if shown == limit {
				truncated = true
				continue
			}
			fmt.Fprintf(&b, "%s: %d\n", name, len(r.matches))
			shown++
		default:
			n, cut := writeGrepContent(&b, name, r, before, after, limit-shown)
			shown += n
			truncated = cut
		}
	}
	if matchedFiles == 0 {
		return tools.Result{Output: "no matches\n"}, nil
	}
	fmt.Fprintf(&b, "[%d match(es) in %d file(s)", totalMatches, matchedFiles)
	if truncated {
		fmt.Fprintf(&b, "; output stopped at %d, narrow the search", limit)
	}
	b.WriteString("]\n")
	return tools.Result{Output: b.String(), Truncated: truncated}, nil
}

func anyMatch(globs []pathGlob, rel string) bool {
	for _, g := range globs {
		if g.match(rel) {
			return true
		}
	}
	return false
}

// grepOne searches one file; it returns nil for unreadable, oversized or binary files.
func grepOne(path string, re *regexp.Regexp, multiline, keepLines bool) *grepFileResult {
	info, err := os.Stat(path)
	if err != nil || info.Size() > maxSearchFileSize {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil || bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
		return nil
	}
	text := strings.TrimPrefix(string(data), utf8BOM)
	lines := strings.Split(strings.TrimSuffix(strings.ReplaceAll(text, "\r\n", "\n"), "\n"), "\n")
	r := &grepFileResult{}
	if multiline {
		norm := strings.Join(lines, "\n")
		starts := make([]int, len(lines)) // byte offset of each line
		for i, off := 1, 0; i < len(lines); i++ {
			off += len(lines[i-1]) + 1
			starts[i] = off
		}
		lineAt := func(off int) int { return sort.Search(len(starts), func(i int) bool { return starts[i] > off }) - 1 }
		for _, m := range re.FindAllStringIndex(norm, -1) {
			if m[0] == m[1] {
				continue // empty matches say nothing useful
			}
			first, last := lineAt(m[0]), lineAt(m[1]-1)
			r.matches = append(r.matches, first)
			r.spans = append(r.spans, [2]int{first, last})
		}
	} else {
		for i, l := range lines {
			if re.MatchString(l) {
				r.matches = append(r.matches, i)
			}
		}
	}
	if keepLines && len(r.matches) > 0 {
		r.lines = lines
	}
	return r
}

func cutLine(s string) string {
	if len(s) > maxGrepLineLen {
		return s[:maxGrepLineLen] + "…"
	}
	return s
}

// writeGrepContent prints matches ripgrep-style: "path:N: text" for matching
// lines, "path-N- text" for context, "--" between separate groups. It returns
// how many matches it printed and whether it stopped at budget.
func writeGrepContent(b *strings.Builder, name string, r *grepFileResult, before, after, budget int) (int, bool) {
	type block struct{ lo, hi int } // inclusive line range to print
	var blocks []block
	isMatch := map[int]bool{}
	printed := 0
	for k, m := range r.matches {
		if printed == budget {
			break
		}
		printed++
		lo, hi := m, m
		if r.spans != nil {
			hi = min(r.spans[k][1], m+maxMultilineSpan-1)
			for l := lo; l <= hi; l++ {
				isMatch[l] = true
			}
		} else {
			isMatch[m] = true
		}
		lo, hi = max(lo-before, 0), min(hi+after, len(r.lines)-1)
		if n := len(blocks); n > 0 && lo <= blocks[n-1].hi+1 {
			blocks[n-1].hi = max(blocks[n-1].hi, hi)
		} else {
			blocks = append(blocks, block{lo, hi})
		}
	}
	for i, bl := range blocks {
		if i > 0 {
			b.WriteString("--\n")
		}
		for l := bl.lo; l <= bl.hi; l++ {
			sep := "-"
			if isMatch[l] {
				sep = ":"
			}
			fmt.Fprintf(b, "%s%s%d%s %s\n", name, sep, l+1, sep, cutLine(r.lines[l]))
		}
	}
	if before > 0 || after > 0 {
		b.WriteString("--\n")
	}
	return printed, printed < len(r.matches)
}

func (f FS) readMany(ctx context.Context, a readManyArgs) (tools.Result, error) {
	budget := f.MaxReadLen
	if budget <= 0 {
		budget = 32 * 1024
	}
	remaining := budget
	var b strings.Builder
	truncated := false
	for i, r := range a.Files {
		if ctx.Err() != nil {
			return tools.Result{}, ctx.Err()
		}
		if i > 0 {
			b.WriteString("\n")
		}
		if remaining < 256 {
			var rest []string
			for _, s := range a.Files[i:] {
				rest = append(rest, s.Path)
			}
			fmt.Fprintf(&b, "[output budget used up; not read: %s]\n", strings.Join(rest, ", "))
			truncated = true
			break
		}
		abs := f.resolve(r.Path)
		if policy.IsSensitive(abs) {
			fmt.Fprintf(&b, "%s: refused (credential or key file)\n", r.Path)
			continue
		}
		g := f
		g.MaxReadLen = remaining
		res, err := g.read(ctx, r)
		if err != nil {
			fmt.Fprintf(&b, "%s: error: %v\n", r.Path, err)
			continue
		}
		b.WriteString(res.Output)
		remaining -= len(res.Output)
		truncated = truncated || res.Truncated
	}
	return tools.Result{Output: b.String(), Truncated: truncated, OutputLimit: budget + 4096}, nil
}
