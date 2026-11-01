package filesystem

import (
	"strings"
	"testing"
)

func sp(s string) *string { return &s }

func TestApplyLineOps(t *testing.T) {
	five := textFile{abs: "/f", text: "l1\nl2\nl3\nl4\nl5\n"}
	tests := []struct {
		name    string
		file    textFile
		ops     []lineOp
		want    string
		wantErr string
	}{
		{name: "insert at top", file: five, ops: []lineOp{{Op: "insert", Start: 0, Text: sp("top")}},
			want: "top\nl1\nl2\nl3\nl4\nl5\n"},
		{name: "insert at end", file: five, ops: []lineOp{{Op: "insert", Start: -1, Text: sp("end")}},
			want: "l1\nl2\nl3\nl4\nl5\nend\n"},
		{name: "insert after line with guard", file: five, ops: []lineOp{{Op: "insert", Start: 2, Expect: sp("l2"), Text: sp("a\nb")}},
			want: "l1\nl2\na\nb\nl3\nl4\nl5\n"},
		{name: "insert text trailing newline is not doubled", file: five, ops: []lineOp{{Op: "insert", Start: 5, Expect: sp("l5"), Text: sp("x\n")}},
			want: "l1\nl2\nl3\nl4\nl5\nx\n"},
		{name: "replace single line", file: five, ops: []lineOp{{Op: "replace", Start: 3, End: 3, Expect: sp("l3"), Text: sp("L3")}},
			want: "l1\nl2\nL3\nl4\nl5\n"},
		{name: "replace range with fewer lines", file: five, ops: []lineOp{{Op: "replace", Start: 2, End: 4, Expect: sp("l2"), Text: sp("mid")}},
			want: "l1\nmid\nl5\n"},
		{name: "replace range guarded by every line", file: five, ops: []lineOp{{Op: "replace", Start: 2, End: 3, Expect: sp("l2\nl3"), Text: sp("x")}},
			want: "l1\nx\nl4\nl5\n"},
		{name: "delete range", file: five, ops: []lineOp{{Op: "delete", Start: 1, End: 2, Expect: sp("l1")}},
			want: "l3\nl4\nl5\n"},
		{name: "delete every line", file: five, ops: []lineOp{{Op: "delete", Start: 1, End: 5, Expect: sp("l1")}},
			want: ""},
		{name: "guard ignores trailing whitespace", file: textFile{abs: "/f", text: "a  \nb\n"}, ops: []lineOp{{Op: "delete", Start: 1, End: 1, Expect: sp("a")}},
			want: "b\n"},
		{name: "guard matches blank line", file: textFile{abs: "/f", text: "a\n\nb\n"}, ops: []lineOp{{Op: "delete", Start: 2, End: 2, Expect: sp("")}},
			want: "a\nb\n"},
		{name: "several ops use original numbering", file: five, ops: []lineOp{
			{Op: "delete", Start: 1, End: 1, Expect: sp("l1")},
			{Op: "replace", Start: 5, End: 5, Expect: sp("l5"), Text: sp("L5")},
			{Op: "insert", Start: 3, Expect: sp("l3"), Text: sp("after3")},
		}, want: "l2\nl3\nafter3\nl4\nL5\n"},
		{name: "insert right after a replaced range is allowed", file: five, ops: []lineOp{
			{Op: "replace", Start: 2, End: 3, Expect: sp("l2"), Text: sp("x")},
			{Op: "insert", Start: 3, Expect: sp("l3"), Text: sp("y")},
		}, want: "l1\nx\ny\nl4\nl5\n"},
		{name: "insert into empty file", file: textFile{abs: "/f", text: ""}, ops: []lineOp{{Op: "insert", Start: 0, Text: sp("first")}},
			want: "first\n"},
		{name: "no final newline is kept", file: textFile{abs: "/f", text: "a\nb"}, ops: []lineOp{{Op: "insert", Start: -1, Text: sp("c")}},
			want: "a\nb\nc"},
		{name: "crlf text input is normalized", file: five, ops: []lineOp{{Op: "replace", Start: 1, End: 1, Expect: sp("l1\r\n"), Text: sp("a\r\nb")}},
			want: "a\nb\nl2\nl3\nl4\nl5\n"},

		{name: "guard mismatch shows context", file: five, ops: []lineOp{{Op: "delete", Start: 3, End: 3, Expect: sp("nope")}},
			wantErr: "expect does not match line 3"},
		{name: "range guard with wrong line count", file: five, ops: []lineOp{{Op: "delete", Start: 2, End: 4, Expect: sp("l2\nl3")}},
			wantErr: "expect does not match"},
		{name: "range past end", file: five, ops: []lineOp{{Op: "delete", Start: 4, End: 6, Expect: sp("l4")}},
			wantErr: "out of range"},
		{name: "insert past end", file: five, ops: []lineOp{{Op: "insert", Start: 6, Expect: sp("x"), Text: sp("y")}},
			wantErr: "the file has 5 lines"},
		{name: "overlapping ranges", file: five, ops: []lineOp{
			{Op: "delete", Start: 1, End: 3, Expect: sp("l1")},
			{Op: "delete", Start: 3, End: 4, Expect: sp("l3")},
		}, wantErr: "overlap"},
		{name: "insert inside a deleted range", file: five, ops: []lineOp{
			{Op: "delete", Start: 2, End: 4, Expect: sp("l2")},
			{Op: "insert", Start: 2, Expect: sp("l2"), Text: sp("x")},
		}, wantErr: "overlap"},
		{name: "two inserts at the same place", file: five, ops: []lineOp{
			{Op: "insert", Start: 0, Text: sp("a")},
			{Op: "insert", Start: 0, Text: sp("b")},
		}, wantErr: "same place"},
		{name: "no net change", file: five, ops: []lineOp{{Op: "replace", Start: 1, End: 1, Expect: sp("l1"), Text: sp("l1")}},
			wantErr: "unchanged"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := applyLineOps(tt.file, tt.ops)
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

func TestValidateLines(t *testing.T) {
	tests := []struct {
		name    string
		args    linesArgs
		wantErr string
	}{
		{"valid insert at top", linesArgs{Path: "f", Ops: []lineOp{{Op: "insert", Start: 0, Text: sp("x")}}}, ""},
		{"valid delete", linesArgs{Path: "f", Ops: []lineOp{{Op: "delete", Start: 1, Expect: sp("a")}}}, ""},
		{"missing path", linesArgs{Ops: []lineOp{{Op: "insert", Start: 0, Text: sp("x")}}}, "path is required"},
		{"no ops", linesArgs{Path: "f"}, "ops must not be empty"},
		{"too many ops", linesArgs{Path: "f", Ops: make([]lineOp, maxEditsPerCall+1)}, "at most"},
		{"unknown op", linesArgs{Path: "f", Ops: []lineOp{{Op: "move", Start: 1}}}, "unknown op"},
		{"insert below -1", linesArgs{Path: "f", Ops: []lineOp{{Op: "insert", Start: -2, Text: sp("x")}}}, "start must be >= -1"},
		{"insert with end", linesArgs{Path: "f", Ops: []lineOp{{Op: "insert", Start: 0, End: 2, Text: sp("x")}}}, "end is not used"},
		{"insert without text", linesArgs{Path: "f", Ops: []lineOp{{Op: "insert", Start: 0}}}, "text is required"},
		{"insert after line without expect", linesArgs{Path: "f", Ops: []lineOp{{Op: "insert", Start: 3, Text: sp("x")}}}, "expect"},
		{"replace start 0", linesArgs{Path: "f", Ops: []lineOp{{Op: "replace", Start: 0, Expect: sp("a"), Text: sp("x")}}}, "start must be >= 1"},
		{"end before start", linesArgs{Path: "f", Ops: []lineOp{{Op: "delete", Start: 3, End: 2, Expect: sp("a")}}}, "before start"},
		{"replace without expect", linesArgs{Path: "f", Ops: []lineOp{{Op: "replace", Start: 1, Text: sp("x")}}}, "expect"},
		{"replace without text", linesArgs{Path: "f", Ops: []lineOp{{Op: "replace", Start: 1, Expect: sp("a")}}}, "text is required"},
		{"delete with text", linesArgs{Path: "f", Ops: []lineOp{{Op: "delete", Start: 1, Expect: sp("a"), Text: sp("x")}}}, "text is not used"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := tt.args
			err := validateLines(&a)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
	a := linesArgs{Path: "f", Ops: []lineOp{{Op: "delete", Start: 4, Expect: sp("a")}}}
	if err := validateLines(&a); err != nil || a.Ops[0].End != 4 {
		t.Fatalf("end should default to start: %v %+v", err, a.Ops[0])
	}
}
