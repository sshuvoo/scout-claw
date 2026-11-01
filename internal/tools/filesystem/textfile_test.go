package filesystem

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"scoutclaw/internal/tools"
)

func TestLoadText(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
		return p
	}
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	big := write("big.txt", strings.Repeat("a", maxEditSize+1))

	tests := []struct {
		name     string
		path     string
		wantErr  tools.Kind
		exists   bool
		text     string
		bom      bool
		crlf     bool
		roundRaw bool // encode(text) must reproduce raw
	}{
		{name: "missing", path: filepath.Join(dir, "nope.txt")},
		{name: "lf", path: write("lf.txt", "a\nb\n"), exists: true, text: "a\nb\n", roundRaw: true},
		{name: "crlf", path: write("crlf.txt", "a\r\nb\r\n"), exists: true, text: "a\nb\n", crlf: true, roundRaw: true},
		{name: "bom crlf", path: write("bom.txt", "\ufeffa\r\nb"), exists: true, text: "a\nb", bom: true, crlf: true, roundRaw: true},
		{name: "bom lf", path: write("bomlf.txt", "\ufeffa\n"), exists: true, text: "a\n", bom: true, roundRaw: true},
		{name: "mixed endings stay verbatim", path: write("mixed.txt", "a\r\nb\nc\r\n"), exists: true, text: "a\r\nb\nc\r\n", roundRaw: true},
		{name: "empty", path: write("empty.txt", ""), exists: true, text: "", roundRaw: true},
		{name: "directory", path: filepath.Join(dir, "sub"), wantErr: tools.KindInvalidInput},
		{name: "binary", path: write("bin.dat", "a\x00b"), wantErr: tools.KindInvalidInput},
		{name: "too large", path: big, wantErr: tools.KindInvalidInput},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := loadText(tt.path)
			if tt.wantErr != tools.KindNone {
				if tools.KindOf(err) != tt.wantErr {
					t.Fatalf("err = %v, want kind %s", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.exists != tt.exists || got.text != tt.text || got.bom != tt.bom || got.crlf != tt.crlf {
				t.Fatalf("got exists=%v text=%q bom=%v crlf=%v", got.exists, got.text, got.bom, got.crlf)
			}
			if tt.exists && runtime.GOOS != "windows" && got.mode.Perm() != 0o640 { // Windows has no Unix modes
				t.Errorf("mode = %v", got.mode)
			}
			if tt.roundRaw && got.encode(got.text) != got.raw {
				t.Errorf("encode(text) = %q, raw %q", got.encode(got.text), got.raw)
			}
			if tt.roundRaw && got.decode(got.raw) != got.text {
				t.Errorf("decode(raw) = %q, text %q", got.decode(got.raw), got.text)
			}
		})
	}
}

func TestNormalize(t *testing.T) {
	tests := []struct {
		name, fileText, in, want string
	}{
		{"pure file: CRLF input becomes LF", "a\nb\n", "a\r\nb", "a\nb"},
		{"pure file: LF input unchanged", "a\nb\n", "a\nb", "a\nb"},
		{"mixed file: input kept verbatim", "a\r\nb\n", "a\r\nb", "a\r\nb"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (textFile{text: tt.fileText}).normalize(tt.in); got != tt.want {
				t.Fatalf("normalize(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestWrittenContent(t *testing.T) {
	tests := []struct {
		name    string
		file    textFile
		content string
		want    string
	}{
		{"new file is written verbatim", textFile{}, "a\r\nb", "a\r\nb"},
		{"crlf file keeps crlf", textFile{exists: true, crlf: true, text: "x\n"}, "a\nb\n", "a\r\nb\r\n"},
		{"bom file keeps one bom", textFile{exists: true, bom: true, text: "x\n"}, "\ufeffa\n", "\ufeffa\n"},
		{"lf file turns crlf input into lf", textFile{exists: true, text: "x\n"}, "a\r\nb\r\n", "a\nb\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.file.writtenContent(tt.content); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPatchedContent(t *testing.T) {
	base := textFile{abs: "/f", text: "alpha\nbeta\nbeta\ngamma\n"}
	tests := []struct {
		name    string
		file    textFile
		edits   []editPair
		want    string
		wantErr string
	}{
		{name: "single unique edit", file: base, edits: []editPair{{OldString: "alpha", NewString: "ALPHA"}}, want: "ALPHA\nbeta\nbeta\ngamma\n"},
		{name: "replace all", file: base, edits: []editPair{{OldString: "beta", NewString: "B", ReplaceAll: true}}, want: "alpha\nB\nB\ngamma\n"},
		{name: "edits apply in order", file: base, edits: []editPair{{OldString: "alpha", NewString: "one"}, {OldString: "one\nbeta", NewString: "two"}}, want: "two\nbeta\ngamma\n"},
		{name: "ambiguous", file: base, edits: []editPair{{OldString: "beta", NewString: "B"}}, wantErr: "matches 2 places"},
		{name: "not found", file: base, edits: []editPair{{OldString: "delta", NewString: "D"}}, wantErr: "not found"},
		{name: "second edit names its index", file: base, edits: []editPair{{OldString: "alpha", NewString: "A"}, {OldString: "zzz", NewString: "Z"}}, wantErr: "edit 2:"},
		{name: "no net change", file: base, edits: []editPair{{OldString: "alpha", NewString: "x"}, {OldString: "x", NewString: "alpha"}}, wantErr: "unchanged"},
		{name: "crlf input matches normalized file", file: base, edits: []editPair{{OldString: "alpha\r\nbeta", NewString: "ab"}}, want: "ab\nbeta\ngamma\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := patchedContent(tt.file, tt.edits)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("got %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestCloseMatch(t *testing.T) {
	text := "func f() {\n\tif x {\n\t\treturn 1\n\t}\n}\n"
	tests := []struct {
		name, old, want string
	}{
		{"whitespace-only difference", "if x {\n  return 1\n}", "matches lines 2-4 if whitespace is ignored"},
		{"first line found, rest differs", "if x {\n\t\treturn 2", "first line appears at line(s) 2"},
		{"nothing similar", "while true", ""},
		{"blank old string", "\n\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := closeMatch(text, tt.old)
			if tt.want == "" && got != "" || tt.want != "" && !strings.Contains(got, tt.want) {
				t.Fatalf("closeMatch = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDiffStat(t *testing.T) {
	tests := []struct {
		a, b         string
		added, remov int
	}{
		{"a\nb\n", "a\nB\n", 1, 1},
		{"", "x\ny\n", 2, 0},
		{"x\ny\n", "", 0, 2},
		{"same\n", "same\n", 0, 0},
	}
	for _, tt := range tests {
		added, removed := diffStat(UnifiedDiff("f", tt.a, tt.b))
		if added != tt.added || removed != tt.remov {
			t.Errorf("diffStat(%q -> %q) = +%d -%d, want +%d -%d", tt.a, tt.b, added, removed, tt.added, tt.remov)
		}
	}
}

func TestWriteAtomicKeepsModeAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "deep", "f.txt")
	if err := writeAtomic(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Dir(p))
	if len(entries) != 1 {
		t.Fatalf("temp file left behind: %v", entries)
	}
	if b, _ := os.ReadFile(p); string(b) != "x" {
		t.Fatalf("content %q", b)
	}
}
