package pathglob

import (
	"runtime"
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
		{"[unclosed", "[unclosed", true},
	}
	if runtime.GOOS != "windows" { // on Windows a backslash is a path separator, not an escape
		tests = append(tests, struct {
			glob string
			path string
			want bool
		}{`\*.md`, "*.md", true})
	}
	for _, tt := range tests {
		g, err := Compile(tt.glob)
		if err != nil {
			t.Fatalf("compile %q: %v", tt.glob, err)
		}
		if got := g.Match(tt.path); got != tt.want {
			t.Errorf("%q matching %q = %v, want %v", tt.glob, tt.path, got, tt.want)
		}
	}
}
