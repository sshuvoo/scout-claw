package filesystem

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPathGlob(t *testing.T) {
	tests := []struct {
		glob string
		path string
		want bool
	}{
		{"*.go", "main.go", true},
		{"*.go", "pkg/deep/x.go", true}, // no slash: base name at any depth
		{"*.go", "main.gox", false},
		{"src/*.go", "src/a.go", true},
		{"src/*.go", "src/sub/a.go", false}, // * stays in one element
		{"src/**/*.go", "src/a.go", true},   // ** may be zero directories
		{"src/**/*.go", "src/x/y/a.go", true},
		{"src/**", "src/x/y", true},
		{"**/test/*", "a/b/test/f", true},
		{"./src/*.go", "src/a.go", true},
		{"?.txt", "a.txt", true},
		{"?.txt", "ab.txt", false},
		{"[ab].txt", "b.txt", true},
		{"[!ab].txt", "c.txt", true},
		{"[!ab].txt", "a.txt", false},
		{"*.{ts,tsx}", "x/app.tsx", true},
		{"*.{ts,tsx}", "app.js", false},
		{"a.b", "axb", false}, // regex metacharacters are literal
		{`\*.md`, "*.md", true},
		{"[unclosed", "[unclosed", true},
	}
	for _, tt := range tests {
		g, err := compilePathGlob(tt.glob)
		if err != nil {
			t.Fatalf("compile %q: %v", tt.glob, err)
		}
		if got := g.match(tt.path); got != tt.want {
			t.Errorf("%q matching %q = %v, want %v", tt.glob, tt.path, got, tt.want)
		}
	}
}

func TestParseIgnoreLine(t *testing.T) {
	tests := []struct {
		line           string
		ok             bool
		negate, dirOny bool
		matches        []string
		notMatches     []string
	}{
		{line: "", ok: false},
		{line: "# comment", ok: false},
		{line: "/", ok: false},
		{line: "*.log", ok: true, matches: []string{"a.log", "x/y/a.log"}, notMatches: []string{"a.log.txt"}},
		{line: "build/", ok: true, dirOny: true, matches: []string{"build", "x/build"}},
		{line: "/dist", ok: true, matches: []string{"dist"}, notMatches: []string{"x/dist"}},
		{line: "docs/*.html", ok: true, matches: []string{"docs/a.html"}, notMatches: []string{"x/docs/a.html"}},
		{line: "!keep.log", ok: true, negate: true, matches: []string{"keep.log", "x/keep.log"}},
		{line: `\#hash`, ok: true, matches: []string{"#hash"}},
		{line: "**/cache", ok: true, matches: []string{"cache", "a/b/cache"}},
		{line: "trailing   ", ok: true, matches: []string{"trailing"}},
	}
	for _, tt := range tests {
		r, ok := parseIgnoreLine(tt.line)
		if ok != tt.ok {
			t.Errorf("%q: ok = %v, want %v", tt.line, ok, tt.ok)
			continue
		}
		if !ok {
			continue
		}
		if r.negate != tt.negate || r.dirOnly != tt.dirOny {
			t.Errorf("%q: negate=%v dirOnly=%v", tt.line, r.negate, r.dirOnly)
		}
		for _, p := range tt.matches {
			if !r.re.MatchString(p) {
				t.Errorf("%q should match %q", tt.line, p)
			}
		}
		for _, p := range tt.notMatches {
			if r.re.MatchString(p) {
				t.Errorf("%q should not match %q", tt.line, p)
			}
		}
	}
}

func TestIgnorer(t *testing.T) {
	repo := t.TempDir()
	mustWrite(t, filepath.Join(repo, ".gitignore"), "*.log\n!keep.log\nbuild/\n/root-only.txt\n")
	mustWrite(t, filepath.Join(repo, "sub", ".gitignore"), "local.txt\n!special.log\n")
	os.Mkdir(filepath.Join(repo, ".git"), 0o755)
	ig := newIgnorer(repo)
	ig.load(filepath.Join(repo, "sub"))
	tests := []struct {
		rel   string
		isDir bool
		want  bool
	}{
		{"a.log", false, true},
		{"keep.log", false, false},
		{"build", true, true},
		{"build", false, false}, // dir-only rule does not hit a file
		{"root-only.txt", false, true},
		{"sub/root-only.txt", false, false},
		{"sub/local.txt", false, true},
		{"local.txt", false, false}, // sub's rules only apply below sub
		{"sub/x.log", false, true},
		{"sub/special.log", false, false}, // a deeper file re-includes it
		{"src/main.go", false, false},
	}
	for _, tt := range tests {
		if got := ig.ignored(filepath.Join(repo, filepath.FromSlash(tt.rel)), tt.isDir); got != tt.want {
			t.Errorf("ignored(%s, dir=%v) = %v, want %v", tt.rel, tt.isDir, got, tt.want)
		}
	}

	// Searching a subdirectory still applies the repository's top-level rules.
	sub := newIgnorer(filepath.Join(repo, "sub"))
	if !sub.ignored(filepath.Join(repo, "sub", "y.log"), false) {
		t.Error("ancestor .gitignore not applied when starting below the repo root")
	}
	// Ancestors above the repository root are not consulted.
	outer := t.TempDir()
	mustWrite(t, filepath.Join(outer, ".gitignore"), "*\n")
	inner := filepath.Join(outer, "repo")
	os.MkdirAll(filepath.Join(inner, ".git"), 0o755)
	if newIgnorer(inner).ignored(filepath.Join(inner, "main.go"), false) {
		t.Error(".gitignore above the repository root must not apply")
	}
}
