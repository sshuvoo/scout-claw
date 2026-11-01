package filesystem

import (
	"reflect"
	"strings"
	"testing"
)

func TestParsePatch(t *testing.T) {
	tests := []struct {
		name    string
		patch   string
		want    []patchFile
		wantErr string
	}{
		{
			name: "codex add, update with move and header, delete",
			patch: `*** Begin Patch
*** Add File: new.txt
+hello
+
+world
*** Update File: a.go
*** Move to: b.go
@@ func f() {
 ctx
-old
+new
*** Delete File: gone.txt
*** End Patch`,
			want: []patchFile{
				{kind: patchAdd, path: "new.txt", lines: []string{"hello", "", "world"}},
				{kind: patchUpdate, path: "a.go", moveTo: "b.go", hunks: []hunk{{header: "func f() {", old: []string{"ctx", "old"}, new: []string{"ctx", "new"}}}},
				{kind: patchDelete, path: "gone.txt"},
			},
		},
		{
			name: "codex hunk without @@, blank context line, end of file",
			patch: `*** Begin Patch
*** Update File: a.txt
 one

-two
+TWO
*** End of File
*** End Patch`,
			want: []patchFile{{kind: patchUpdate, path: "a.txt", hunks: []hunk{{old: []string{"one", "", "two"}, new: []string{"one", "", "TWO"}, eof: true}}}},
		},
		{
			name:  "crlf input is accepted",
			patch: "*** Begin Patch\r\n*** Delete File: x\r\n*** End Patch\r\n",
			want:  []patchFile{{kind: patchDelete, path: "x"}},
		},
		{
			name: "unified diff: update, add, delete, rename",
			patch: `diff --git a/a.go b/a.go
index 111..222 100644
--- a/a.go
+++ b/a.go
@@ -10,3 +10,3 @@ func f() {
 ctx
--- old sql comment
+-- new sql comment
--- /dev/null
+++ b/new.go
@@ -0,0 +1,2 @@
+package x
+
--- a/old.go
+++ /dev/null
@@ -1 +0,0 @@
-package x
--- a/r1.go	2024-01-01 00:00:00
+++ b/r2.go	2024-01-01 00:00:00
@@ -1 +1 @@
-a
+b
\ No newline at end of file`,
			want: []patchFile{
				{kind: patchUpdate, path: "a.go", hunks: []hunk{{hint: 10, old: []string{"ctx", "-- old sql comment"}, new: []string{"ctx", "-- new sql comment"}}}},
				{kind: patchAdd, path: "new.go", lines: []string{"package x", ""}},
				{kind: patchDelete, path: "old.go"},
				{kind: patchUpdate, path: "r1.go", moveTo: "r2.go", hunks: []hunk{{hint: 1, old: []string{"a"}, new: []string{"b"}}}},
			},
		},
		{name: "empty", patch: "  \n", wantErr: "empty"},
		{name: "no end marker", patch: "*** Begin Patch\n*** Delete File: x\n", wantErr: "missing its *** End Patch"},
		{name: "add line without plus", patch: "*** Begin Patch\n*** Add File: x\nnope\n*** End Patch", wantErr: "must start with '+'"},
		{name: "bad hunk line", patch: "*** Begin Patch\n*** Update File: x\n?what\n*** End Patch", wantErr: "must start with ' ', '-' or '+'"},
		{name: "update with no change", patch: "*** Begin Patch\n*** Update File: x\n same\n*** End Patch", wantErr: "has no changes"},
		{name: "unknown directive", patch: "*** Begin Patch\n*** Rename File: x\n*** End Patch", wantErr: "expected *** Add File"},
		{name: "end of file before hunk", patch: "*** Begin Patch\n*** Update File: x\n*** End of File\n*** End Patch", wantErr: "before any hunk"},
		{name: "same path twice", patch: "*** Begin Patch\n*** Delete File: x\n*** Add File: x\n+a\n*** End Patch", wantErr: "more than once"},
		{name: "prose only", patch: "please change the file", wantErr: "no file changes"},
		{name: "unified without hunks", patch: "--- a/x\n+++ b/x\n", wantErr: "no @@ hunks"},
		{name: "too large", patch: "*** Begin Patch\n*** Add File: x\n+" + strings.Repeat("a", maxPatchSize) + "\n*** End Patch", wantErr: "exceeds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePatch(tt.patch)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestApplyHunks(t *testing.T) {
	src := "package x\n\nfunc a() {\n\treturn 1\n}\n\nfunc b() {\n\treturn 1\n}\n"
	tests := []struct {
		name    string
		text    string
		hunks   []hunk
		want    string
		wantErr string
	}{
		{name: "replace with context", text: src,
			hunks: []hunk{{old: []string{"func a() {", "\treturn 1"}, new: []string{"func a() {", "\treturn 10"}}},
			want:  strings.Replace(src, "return 1", "return 10", 1)},
		{name: "header disambiguates", text: src,
			hunks: []hunk{{header: "func b() {", old: []string{"\treturn 1"}, new: []string{"\treturn 2"}}},
			want:  "package x\n\nfunc a() {\n\treturn 1\n}\n\nfunc b() {\n\treturn 2\n}\n"},
		{name: "ambiguous without header", text: src,
			hunks: []hunk{{old: []string{"\treturn 1"}, new: []string{"\treturn 2"}}}, wantErr: "match 2 places"},
		{name: "unified hint picks the nearest match", text: src,
			hunks: []hunk{{hint: 8, old: []string{"\treturn 1"}, new: []string{"\treturn 2"}}},
			want:  "package x\n\nfunc a() {\n\treturn 1\n}\n\nfunc b() {\n\treturn 2\n}\n"},
		{name: "later hunks search after earlier ones", text: "x\na\ny\na\n",
			hunks: []hunk{{old: []string{"x", "a"}, new: []string{"x", "A"}}, {old: []string{"a"}, new: []string{"B"}}},
			want:  "x\nA\ny\nB\n"},
		{name: "indentation mismatch is tolerated", text: "if x {\n\t\tcall()\n}\n",
			hunks: []hunk{{old: []string{"if x {", "    call()"}, new: []string{"if x {", "    call2()"}}},
			want:  "if x {\n    call2()\n}\n"},
		{name: "trailing whitespace tolerated", text: "a  \nb\n",
			hunks: []hunk{{old: []string{"a", "b"}, new: []string{"a", "c"}}}, want: "a\nc\n"},
		{name: "end of file anchor", text: "x\ny\nx\n",
			hunks: []hunk{{old: []string{"x"}, new: []string{"z"}, eof: true}}, want: "x\ny\nz\n"},
		{name: "pure insertion at end", text: "a\n", hunks: []hunk{{new: []string{"b"}}}, want: "a\nb\n"},
		{name: "pure insertion after header", text: "a\nb\n", hunks: []hunk{{header: "a", new: []string{"mid"}}}, want: "a\nmid\nb\n"},
		{name: "pure insertion at hint", text: "a\nb\nc\n", hunks: []hunk{{hint: 2, new: []string{"x"}}}, want: "a\nx\nb\nc\n"},
		{name: "missing final newline kept", text: "a\nb", hunks: []hunk{{old: []string{"b"}, new: []string{"c"}}}, want: "a\nc"},
		{name: "not found shows near miss", text: src,
			hunks: []hunk{{old: []string{"func a() {", "\treturn 99"}, new: []string{"x"}}}, wantErr: "first line appears at line(s) 3"},
		{name: "header not found", text: src, hunks: []hunk{{header: "func zzz", old: []string{"x"}, new: []string{"y"}}}, wantErr: `@@ line "func zzz" was not found`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := applyHunks(tt.text, tt.hunks)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("got %q, %v\nwant %q", got, err, tt.want)
			}
		})
	}
}

func TestSeekPrefersStrictestMatcher(t *testing.T) {
	lines := []string{"  a", "a", "a "}
	if got := seek(lines, []string{"a"}, 0, false); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("exact match should win: %v", got)
	}
	if got := seek(lines, []string{"a"}, 2, false); !reflect.DeepEqual(got, []int{2}) {
		t.Fatalf("trailing-space match from index 2: %v", got)
	}
	if got := seek(lines, []string{"b"}, 0, false); got != nil {
		t.Fatalf("no match expected: %v", got)
	}
}
