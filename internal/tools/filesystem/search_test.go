package filesystem

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"scoutclaw/internal/tools"
)

// searchRepo builds a small repository with ignored, binary and credential files.
func searchRepo(t *testing.T) (FS, string, map[string]tools.Tool) {
	f, ws, m := setup(t)
	files := map[string]string{
		".gitignore":          "*.log\nbuild/\n",
		"main.go":             "package main\n\nfunc main() {\n\tTODO()\n}\n",
		"pkg/util.go":         "package pkg\n\n// TODO: tidy\nfunc Helper() int {\n\treturn 1\n}\n",
		"pkg/util_test.go":    "package pkg\n\nfunc TestHelper(t *testing.T) {}\n",
		"web/app.ts":          "export const todo = 1; // TODO\n",
		"README.md":           "# Demo\nTODO list\n",
		"debug.log":           "TODO in a log\n",
		"build/out.go":        "package build // TODO\n",
		"node_modules/x/y.js": "// TODO\n",
		"server.key":          "TODO secret\n",
	}
	for p, c := range files {
		mustWrite(t, filepath.Join(ws, filepath.FromSlash(p)), c)
	}
	os.WriteFile(filepath.Join(ws, "blob.bin"), []byte("TODO\x00\x01"), 0o644)
	os.Mkdir(filepath.Join(ws, ".git"), 0o755)
	return f, ws, m
}

func lines(s string) []string { return strings.Split(strings.TrimSpace(s), "\n") }

func TestGlob(t *testing.T) {
	_, ws, m := searchRepo(t)
	ctx := context.Background()
	tests := []struct {
		name     string
		in       map[string]any
		want     []string
		excluded []string
	}{
		{"base-name pattern at any depth", map[string]any{"pattern": "*.go"}, []string{"main.go", "pkg/util.go", "pkg/util_test.go"}, []string{"build/out.go"}},
		{"path pattern", map[string]any{"pattern": "pkg/*_test.go"}, []string{"pkg/util_test.go"}, []string{"pkg/util.go"}},
		{"braces", map[string]any{"pattern": "*.{ts,md}"}, []string{"web/app.ts", "README.md"}, []string{"main.go"}},
		{"from a subdirectory", map[string]any{"pattern": "*.go", "path": "pkg"}, []string{"pkg/util.go"}, []string{"main.go"}},
		{"ignored, skipped and credential files never appear", map[string]any{"pattern": "*"}, []string{"main.go"}, []string{"debug.log", "build/out.go", "node_modules/x/y.js", "server.key"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := run(t, m["fs_glob"], ctx, tt.in)
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tt.want {
				if !strings.Contains(r.Output, filepath.FromSlash(w)+"\n") {
					t.Errorf("missing %s in:\n%s", w, r.Output)
				}
			}
			for _, x := range tt.excluded {
				if strings.Contains(r.Output, filepath.FromSlash(x)) {
					t.Errorf("%s should be excluded:\n%s", x, r.Output)
				}
			}
		})
	}

	// newest first, then limit
	old := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(ws, "main.go"), old, old)
	os.Chtimes(filepath.Join(ws, "pkg", "util_test.go"), old, old)
	now := time.Now()
	os.Chtimes(filepath.Join(ws, "pkg", "util.go"), now, now)
	r, _ := run(t, m["fs_glob"], ctx, map[string]any{"pattern": "*.go", "max_results": 1})
	if got := lines(r.Output); got[0] != filepath.Join("pkg", "util.go") || !r.Truncated || !strings.Contains(r.Output, "1 of 3 files") {
		t.Fatalf("want newest first and truncation note: %q", r.Output)
	}
	if r, _ := run(t, m["fs_glob"], ctx, map[string]any{"pattern": "*.rs"}); !strings.Contains(r.Output, "no files match") {
		t.Fatalf("%q", r.Output)
	}
	for _, bad := range []string{`{"pattern":""}`, `{"pattern":"   "}`, `{}`} {
		if m["fs_glob"].Validate(json.RawMessage(bad)) == nil {
			t.Errorf("must be rejected: %s", bad)
		}
	}
}

func TestGrepOutputModesAndFilters(t *testing.T) {
	_, _, m := searchRepo(t)
	ctx := context.Background()
	tests := []struct {
		name     string
		in       map[string]any
		want     []string
		excluded []string
	}{
		{"content with line numbers", map[string]any{"pattern": "TODO"},
			[]string{"main.go:4: \tTODO()", "util.go:3: // TODO: tidy", "README.md:2: TODO list", "[4 match(es) in 4 file(s)]"},
			[]string{"debug.log", "build/out.go", "node_modules", "server.key", "blob.bin"}},
		{"files only", map[string]any{"pattern": "TODO", "output": "files"}, []string{"main.go\n", "web/app.ts\n"}, []string{":4:"}},
		{"count", map[string]any{"pattern": "TODO|todo", "output": "count"}, []string{"web/app.ts: 1"}, nil},
		{"ignore case", map[string]any{"pattern": "helper", "ignore_case": true}, []string{"pkg/util.go:4:", "pkg/util_test.go:3:"}, nil},
		{"fixed strings", map[string]any{"pattern": "TODO()", "fixed_strings": true}, []string{"main.go:4:"}, []string{"pkg/util.go"}},
		{"type filter", map[string]any{"pattern": "TODO", "type": "ts"}, []string{"web/app.ts:1:"}, []string{"main.go"}},
		{"glob filter", map[string]any{"pattern": "func", "glob": "*_test.go"}, []string{"pkg/util_test.go:3:"}, []string{"pkg/util.go:"}},
		{"single file path", map[string]any{"pattern": "return", "path": "pkg/util.go"}, []string{"pkg/util.go:5:"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := run(t, m["fs_grep"], ctx, tt.in)
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tt.want {
				if !strings.Contains(r.Output, filepath.FromSlash(w)) && !strings.Contains(r.Output, w) {
					t.Errorf("missing %q in:\n%s", w, r.Output)
				}
			}
			for _, x := range tt.excluded {
				if strings.Contains(r.Output, filepath.FromSlash(x)) {
					t.Errorf("%q should not appear:\n%s", x, r.Output)
				}
			}
		})
	}
	if r, _ := run(t, m["fs_grep"], ctx, map[string]any{"pattern": "nothing-like-this"}); r.Output != "no matches\n" {
		t.Fatalf("%q", r.Output)
	}
	if _, err := run(t, m["fs_grep"], ctx, map[string]any{"pattern": "x", "path": "server.key"}); tools.KindOf(err) != tools.KindDenied {
		t.Fatalf("grepping a credential file directly must be denied: %v", err)
	}
}

func TestGrepContextMultilineAndLimits(t *testing.T) {
	_, ws, m := setup(t)
	ctx := context.Background()
	var b strings.Builder
	for i := 1; i <= 20; i++ {
		if i == 5 || i == 7 || i == 15 {
			b.WriteString("hit\n")
		} else {
			b.WriteString("line\n")
		}
	}
	mustWrite(t, filepath.Join(ws, "f.txt"), b.String())
	mustWrite(t, filepath.Join(ws, "crlf.txt"), "a\r\nstart\r\nmiddle\r\nend\r\n")

	r, _ := run(t, m["fs_grep"], ctx, map[string]any{"pattern": "hit", "path": "f.txt", "context": 1})
	want := "f.txt-4- line\nf.txt:5: hit\nf.txt-6- line\nf.txt:7: hit\nf.txt-8- line\n--\nf.txt-14- line\nf.txt:15: hit\nf.txt-16- line\n--\n"
	if !strings.HasPrefix(r.Output, want) {
		t.Fatalf("context output:\n%s\nwant prefix:\n%s", r.Output, want)
	}
	r, _ = run(t, m["fs_grep"], ctx, map[string]any{"pattern": "hit", "path": "f.txt", "before": 2})
	if !strings.Contains(r.Output, "f.txt-3- line\nf.txt-4- line\nf.txt:5: hit") || strings.Contains(r.Output, "f.txt-8-") || strings.Contains(r.Output, "f.txt-16-") {
		t.Fatalf("before-only output:\n%s", r.Output)
	}
	r, _ = run(t, m["fs_grep"], ctx, map[string]any{"pattern": "hit", "path": "f.txt", "max_results": 2})
	if !r.Truncated || strings.Contains(r.Output, ":15:") || !strings.Contains(r.Output, "[3 match(es) in 1 file(s); output stopped at 2") {
		t.Fatalf("limit:\n%s", r.Output)
	}
	r, _ = run(t, m["fs_grep"], ctx, map[string]any{"pattern": `start.*end`, "path": "crlf.txt", "multiline": true})
	if !strings.Contains(r.Output, "crlf.txt:2: start\ncrlf.txt:3: middle\ncrlf.txt:4: end") {
		t.Fatalf("multiline across CRLF lines:\n%s", r.Output)
	}
	long := strings.Repeat("x", 1000) + "needle"
	mustWrite(t, filepath.Join(ws, "long.txt"), long+"\n")
	r, _ = run(t, m["fs_grep"], ctx, map[string]any{"pattern": "needle", "path": "long.txt"})
	if len(r.Output) > maxGrepLineLen+200 || !strings.Contains(r.Output, "…") {
		t.Fatalf("long lines must be cut: %d bytes", len(r.Output))
	}
}

func TestValidateGrep(t *testing.T) {
	tests := []struct {
		name    string
		args    grepArgs
		wantErr string
	}{
		{"ok", grepArgs{Pattern: "a"}, ""},
		{"empty pattern", grepArgs{}, "pattern is required"},
		{"bad regex", grepArgs{Pattern: "("}, "bad pattern"},
		{"bad regex is fine as fixed string", grepArgs{Pattern: "(", FixedStrings: true}, ""},
		{"unknown type", grepArgs{Pattern: "a", Type: "cobol"}, "unknown type"},
		{"context too big", grepArgs{Pattern: "a", Context: 11}, "context must be 0-10"},
		{"negative after", grepArgs{Pattern: "a", After: -1}, "after must be 0-10"},
		{"bad output", grepArgs{Pattern: "a", Output: "json"}, "output must be"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := tt.args
			err := validateGrep(&a)
			if tt.wantErr == "" {
				if err != nil || a.Output != "content" || a.Path != "." {
					t.Fatalf("err=%v output=%q path=%q", err, a.Output, a.Path)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestReadMany(t *testing.T) {
	_, _, m := searchRepo(t)
	ctx := context.Background()
	r, err := run(t, m["fs_read_many"], ctx, map[string]any{"files": []map[string]any{
		{"path": "main.go"},
		{"path": "pkg/util.go", "offset_line": 4, "limit_lines": 1},
		{"path": "missing.go"},
		{"path": "server.key"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"main.go: lines 1-5 of 5", "     3\tfunc main", filepath.Join("pkg", "util.go") + ": lines 4-4 of 6",
		"     4\tfunc Helper", "missing.go: error:", "server.key: refused"} {
		if !strings.Contains(r.Output, w) {
			t.Errorf("missing %q in:\n%s", w, r.Output)
		}
	}
	// files read this way count as read for editing
	if _, err := run(t, m["fs_patch"], ctx, map[string]any{"path": "main.go", "old_string": "TODO()", "new_string": "done()"}); err != nil {
		t.Fatalf("edit after fs_read_many: %v", err)
	}

	// the budget is shared: later files are reported, not silently dropped
	_, ws2, m2 := setup(t)
	big := strings.Repeat("0123456789\n", 4000)
	mustWrite(t, filepath.Join(ws2, "a.txt"), big)
	mustWrite(t, filepath.Join(ws2, "b.txt"), big)
	r, _ = run(t, m2["fs_read_many"], ctx, map[string]any{"files": []map[string]any{{"path": "a.txt", "limit_lines": 2000}, {"path": "b.txt"}}})
	if !r.Truncated || !strings.Contains(r.Output, "not read: b.txt") {
		t.Fatalf("budget overflow must be reported:\n%s", r.Output[max(0, len(r.Output)-300):])
	}

	for _, bad := range []string{`{"files":[]}`, `{"files":[{"path":""}]}`, `{"files":[{"path":"a","limit_lines":-1}]}`} {
		if m["fs_read_many"].Validate(json.RawMessage(bad)) == nil {
			t.Errorf("must be rejected: %s", bad)
		}
	}
	many := make([]map[string]any, maxReadManyFiles+1)
	for i := range many {
		many[i] = map[string]any{"path": "x"}
	}
	b, _ := json.Marshal(map[string]any{"files": many})
	if m["fs_read_many"].Validate(b) == nil {
		t.Error("too many files must be rejected")
	}
}
