package filesystem

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"scoutclaw/internal/tools"
)

type lineOp struct {
	Op     string  `json:"op"` // insert, replace, delete
	Start  int     `json:"start"`
	End    int     `json:"end"`
	Expect *string `json:"expect"`
	Text   *string `json:"text"`
}

type linesArgs struct {
	Path string   `json:"path"`
	Ops  []lineOp `json:"ops"`
}

func (f FS) linesTool() tools.Tool {
	return &tools.Typed[linesArgs]{
		ToolName: "fs_lines",
		Desc: "Edit a file you have read by line number. Each op is one of: " +
			"insert (add text after line start; start 0 = top of file, -1 = end of file), " +
			"replace (replace lines start..end with text), delete (remove lines start..end). end defaults to start. " +
			"Line numbers refer to the file as it is before this call; all ops apply atomically. " +
			"expect is required for replace, delete and insert with start > 0: the current text of line start (or of the whole range, newline-separated). " +
			"If it does not match, nothing changes and the actual lines are shown. Shows a diff for approval and keeps a backup.",
		Schema: `{"type":"object","properties":{"path":{"type":"string"},"ops":{"type":"array","minItems":1,"maxItems":50,"items":{"type":"object","properties":{` +
			`"op":{"type":"string","enum":["insert","replace","delete"]},` +
			`"start":{"type":"integer","description":"1-based line. For insert: the line to insert after (0 = top, -1 = end)."},` +
			`"end":{"type":"integer","description":"Last line of the range for replace/delete (default: start)."},` +
			`"expect":{"type":"string","description":"Current text of line start, or of every line in the range joined by newlines."},` +
			`"text":{"type":"string","description":"Lines to insert or to put in place of the range (insert/replace only)."}},` +
			`"required":["op","start"],"additionalProperties":false}}},"required":["path","ops"],"additionalProperties":false}`,
		ValidateFn: validateLines,
		Run:        f.lines,
		PreviewFn:  f.previewLines,
	}
}

func validateLines(a *linesArgs) error {
	if len(a.Ops) == 0 {
		return errors.New("ops must not be empty")
	}
	if len(a.Ops) > maxEditsPerCall {
		return fmt.Errorf("at most %d ops per call", maxEditsPerCall)
	}
	for i := range a.Ops {
		o := &a.Ops[i]
		bad := func(format string, v ...any) error {
			return fmt.Errorf("op %d (%s): %s", i+1, o.Op, fmt.Sprintf(format, v...))
		}
		switch o.Op {
		case "insert":
			if o.Start < -1 {
				return bad("start must be >= -1")
			}
			if o.End != 0 {
				return bad("end is not used by insert")
			}
			if o.Text == nil {
				return bad("text is required")
			}
			if o.Start > 0 && o.Expect == nil {
				return bad("expect (the current text of line %d) is required", o.Start)
			}
		case "replace", "delete":
			if o.Start < 1 {
				return bad("start must be >= 1")
			}
			if o.End == 0 {
				o.End = o.Start
			}
			if o.End < o.Start {
				return bad("end (%d) is before start (%d)", o.End, o.Start)
			}
			if o.Expect == nil {
				return bad("expect (the current text of line %d) is required", o.Start)
			}
			if o.Op == "replace" && o.Text == nil {
				return bad("text is required")
			}
			if o.Op == "delete" && o.Text != nil {
				return bad("text is not used by delete")
			}
		default:
			return fmt.Errorf("op %d: unknown op %q (want insert, replace or delete)", i+1, o.Op)
		}
	}
	return needPath(a.Path)
}

// sameLine compares ignoring trailing whitespace, which models often drop.
func sameLine(a, b string) bool {
	return strings.TrimRight(a, " \t\r") == strings.TrimRight(b, " \t\r")
}

// around shows numbered lines near lo..hi (1-based) for error messages.
func around(lines []string, lo, hi int) string {
	var b strings.Builder
	for n := max(lo-2, 1); n <= min(hi+2, len(lines)); n++ {
		fmt.Fprintf(&b, "%6d\t%s\n", n, lines[n-1])
	}
	return b.String()
}

// applyLineOps applies the ops to t.text. Ops address the original line
// numbers; they are applied bottom-up so they do not shift each other.
func applyLineOps(t textFile, ops []lineOp) (string, error) {
	lines := splitLines(t.text)
	finalNL := t.text == "" || strings.HasSuffix(t.text, "\n")
	n := len(lines)
	type span struct {
		lo, hi float64 // inserts sit between lines: after line p is p+0.5
		op     lineOp
		idx    int
	}
	spans := make([]span, 0, len(ops))
	for i, o := range ops {
		fail := func(format string, v ...any) error {
			return tools.Errorf(tools.KindInvalidInput, "op %d (%s): %s", i+1, o.Op, fmt.Sprintf(format, v...))
		}
		check := func(lo, hi int) error {
			want := strings.Split(strings.TrimSuffix(t.normalize(*o.Expect), "\n"), "\n")
			got := lines[lo-1 : lo]
			if len(want) > 1 {
				got = lines[lo-1 : hi]
			}
			if len(want) == len(got) && slices.EqualFunc(want, got, sameLine) {
				return nil
			}
			return fail("expect does not match line %d; the file has different content there (re-read it):\n%s", lo, around(lines, lo, hi))
		}
		switch o.Op {
		case "insert":
			p := o.Start
			if p == -1 {
				p = n
			}
			if p > n {
				return "", fail("cannot insert after line %d: the file has %d lines", p, n)
			}
			if o.Start > 0 {
				if err := check(p, p); err != nil {
					return "", err
				}
			}
			spans = append(spans, span{float64(p) + 0.5, float64(p) + 0.5, o, i})
		default:
			if o.End > n {
				return "", fail("lines %d-%d are out of range: the file has %d lines", o.Start, o.End, n)
			}
			if err := check(o.Start, o.End); err != nil {
				return "", err
			}
			spans = append(spans, span{float64(o.Start), float64(o.End), o, i})
		}
	}
	sort.SliceStable(spans, func(a, b int) bool { return spans[a].lo > spans[b].lo })
	for k := 1; k < len(spans); k++ {
		upper, lower := spans[k-1], spans[k]
		if lower.hi >= upper.lo {
			return "", tools.Errorf(tools.KindInvalidInput, "ops %d and %d overlap or insert at the same place; combine them into one op", lower.idx+1, upper.idx+1)
		}
	}
	textLines := func(s *string) []string {
		return strings.Split(strings.TrimSuffix(t.normalize(*s), "\n"), "\n")
	}
	for _, sp := range spans {
		o := sp.op
		switch o.Op {
		case "insert":
			lines = slices.Insert(lines, int(sp.lo), textLines(o.Text)...)
		case "replace":
			lines = slices.Replace(lines, o.Start-1, o.End, textLines(o.Text)...)
		case "delete":
			lines = slices.Delete(lines, o.Start-1, o.End)
		}
	}
	out := strings.Join(lines, "\n")
	if len(lines) > 0 && finalNL {
		out += "\n"
	}
	if out == t.text {
		return "", tools.Errorf(tools.KindInvalidInput, "the ops leave %s unchanged", t.abs)
	}
	return out, nil
}

func (f FS) previewLines(ctx context.Context, a linesArgs) (string, error) {
	abs := f.resolve(a.Path)
	t, err := f.loadExisting(abs)
	if err != nil {
		return "", err
	}
	updated, err := applyLineOps(t, a.Ops)
	if err != nil {
		return "", err
	}
	return "edit " + f.rel(abs) + "\n" + UnifiedDiff(f.rel(abs), t.text, updated), nil
}

func (f FS) lines(ctx context.Context, a linesArgs) (tools.Result, error) {
	abs := f.resolve(a.Path)
	if err := f.checkWritable(ctx, abs); err != nil {
		return tools.Result{}, err
	}
	t, err := f.loadExisting(abs)
	if err != nil {
		return tools.Result{}, err
	}
	updated, err := applyLineOps(t, a.Ops)
	if err != nil {
		return tools.Result{}, err
	}
	return f.commit(t, t.encode(updated), "lines")
}
