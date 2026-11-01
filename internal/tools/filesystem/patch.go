package filesystem

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// A parsed multi-file patch. Two input formats are accepted: the
// "*** Begin Patch" format used by OpenAI's Codex CLI, and unified diffs.

type patchKind string

const (
	patchAdd    patchKind = "add"
	patchUpdate patchKind = "update"
	patchDelete patchKind = "delete"
)

type patchFile struct {
	kind   patchKind
	path   string
	moveTo string   // update only: rename after editing
	lines  []string // add only: the new file's lines
	hunks  []hunk   // update only
}

type hunk struct {
	header string   // "@@ text": the hunk is searched for after a line matching text
	hint   int      // unified diffs: 1-based old start line, used to break ties
	old    []string // context and removed lines, in order
	new    []string // context and added lines, in order
	eof    bool     // the hunk must match at the end of the file
}

const maxPatchSize = 1 << 20

var unifiedHunk = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+\d+(?:,\d+)? @@`)

func parsePatch(text string) ([]patchFile, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("patch is empty")
	}
	if len(text) > maxPatchSize {
		return nil, fmt.Errorf("patch exceeds %d bytes", maxPatchSize)
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	var files []patchFile
	var err error
	if strings.Contains(text, "*** Begin Patch") || strings.Contains(text, "*** Update File:") ||
		strings.Contains(text, "*** Add File:") || strings.Contains(text, "*** Delete File:") {
		files, err = parseCodexPatch(lines)
	} else {
		files, err = parseUnifiedDiff(lines)
	}
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, errors.New("patch contains no file changes")
	}
	seen := map[string]bool{}
	for _, f := range files {
		for _, p := range []string{f.path, f.moveTo} {
			if p == "" {
				continue
			}
			if seen[p] {
				return nil, fmt.Errorf("%s appears more than once in the patch; combine its changes into one section", p)
			}
			seen[p] = true
		}
	}
	return files, nil
}

func parseCodexPatch(lines []string) ([]patchFile, error) {
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i < len(lines) && strings.TrimSpace(lines[i]) == "*** Begin Patch" {
		i++
	}
	var files []patchFile
	for i < len(lines) {
		line := lines[i]
		switch {
		case strings.TrimSpace(line) == "*** End Patch":
			return files, nil
		case strings.TrimSpace(line) == "":
			i++
		case strings.HasPrefix(line, "*** Add File: "):
			f := patchFile{kind: patchAdd, path: strings.TrimSpace(strings.TrimPrefix(line, "*** Add File: "))}
			for i++; i < len(lines) && !strings.HasPrefix(lines[i], "*** "); i++ {
				if !strings.HasPrefix(lines[i], "+") {
					return nil, fmt.Errorf("Add File %s: line %d must start with '+': %q", f.path, i+1, lines[i])
				}
				f.lines = append(f.lines, lines[i][1:])
			}
			files = append(files, f)
		case strings.HasPrefix(line, "*** Delete File: "):
			files = append(files, patchFile{kind: patchDelete, path: strings.TrimSpace(strings.TrimPrefix(line, "*** Delete File: "))})
			i++
		case strings.HasPrefix(line, "*** Update File: "):
			f := patchFile{kind: patchUpdate, path: strings.TrimSpace(strings.TrimPrefix(line, "*** Update File: "))}
			i++
			if i < len(lines) && strings.HasPrefix(lines[i], "*** Move to: ") {
				f.moveTo = strings.TrimSpace(strings.TrimPrefix(lines[i], "*** Move to: "))
				i++
			}
			var cur *hunk
			for ; i < len(lines); i++ {
				l := lines[i]
				if strings.HasPrefix(l, "*** ") && strings.TrimSpace(l) != "*** End of File" {
					break
				}
				switch {
				case strings.TrimSpace(l) == "*** End of File":
					if cur == nil {
						return nil, fmt.Errorf("Update File %s: *** End of File before any hunk", f.path)
					}
					cur.eof = true
				case strings.HasPrefix(l, "@@"):
					f.hunks = append(f.hunks, hunk{header: strings.TrimSpace(strings.TrimPrefix(l, "@@"))})
					cur = &f.hunks[len(f.hunks)-1]
				default:
					if cur == nil {
						f.hunks = append(f.hunks, hunk{})
						cur = &f.hunks[len(f.hunks)-1]
					}
					if err := addHunkLine(cur, l); err != nil {
						return nil, fmt.Errorf("Update File %s: line %d: %v", f.path, i+1, err)
					}
				}
			}
			if f.moveTo == "" && !hasChanges(f.hunks) {
				return nil, fmt.Errorf("Update File %s has no changes", f.path)
			}
			files = append(files, f)
		default:
			return nil, fmt.Errorf("line %d: expected *** Add File, *** Update File, *** Delete File or *** End Patch, got %q", i+1, line)
		}
	}
	return nil, errors.New("patch is missing its *** End Patch line")
}

func addHunkLine(h *hunk, l string) error {
	switch {
	case l == "": // models often drop the space on blank context lines
		h.old, h.new = append(h.old, ""), append(h.new, "")
	case l[0] == ' ':
		h.old, h.new = append(h.old, l[1:]), append(h.new, l[1:])
	case l[0] == '-':
		h.old = append(h.old, l[1:])
	case l[0] == '+':
		h.new = append(h.new, l[1:])
	case strings.HasPrefix(l, `\ No newline`):
	default:
		return fmt.Errorf("must start with ' ', '-' or '+': %q", l)
	}
	return nil
}

func hasChanges(hs []hunk) bool {
	for _, h := range hs {
		if strings.Join(h.old, "\n") != strings.Join(h.new, "\n") || len(h.old) != len(h.new) {
			return true
		}
	}
	return false
}

func diffPath(field string) string {
	p := strings.TrimSpace(field)
	if i := strings.IndexByte(p, '\t'); i >= 0 { // "--- a/x\t2024-01-01 ..."
		p = p[:i]
	}
	if p == "/dev/null" {
		return p
	}
	if strings.HasPrefix(p, "a/") || strings.HasPrefix(p, "b/") {
		p = p[2:]
	}
	return p
}

func parseUnifiedDiff(lines []string) ([]patchFile, error) {
	var files []patchFile
	for i := 0; i < len(lines); {
		if !strings.HasPrefix(lines[i], "--- ") || i+1 >= len(lines) || !strings.HasPrefix(lines[i+1], "+++ ") {
			i++ // diff --git, index, mode lines and prose are skipped
			continue
		}
		from, to := diffPath(lines[i][4:]), diffPath(lines[i+1][4:])
		i += 2
		var hunks []hunk
		// A removed line can itself start with "-- ", so "--- " only starts
		// the next file when "+++ " follows it.
		nextFile := func(i int) bool {
			return strings.HasPrefix(lines[i], "diff ") ||
				strings.HasPrefix(lines[i], "--- ") && i+1 < len(lines) && strings.HasPrefix(lines[i+1], "+++ ")
		}
		for i < len(lines) && !nextFile(i) {
			l := lines[i]
			if m := unifiedHunk.FindStringSubmatch(l); m != nil {
				start, _ := strconv.Atoi(m[1])
				hunks = append(hunks, hunk{hint: start})
			} else if len(hunks) > 0 {
				if err := addHunkLine(&hunks[len(hunks)-1], l); err != nil {
					return nil, fmt.Errorf("%s: line %d: %v", to, i+1, err)
				}
			}
			i++
		}
		switch {
		case from == "/dev/null" && to == "/dev/null":
			return nil, errors.New("a diff section has /dev/null on both sides")
		case from == "/dev/null":
			var add []string
			for _, h := range hunks {
				add = append(add, h.new...)
			}
			files = append(files, patchFile{kind: patchAdd, path: to, lines: add})
		case to == "/dev/null":
			files = append(files, patchFile{kind: patchDelete, path: from})
		default:
			f := patchFile{kind: patchUpdate, path: from, hunks: hunks}
			if to != from {
				f.moveTo = to
			}
			if len(hunks) == 0 && f.moveTo == "" {
				return nil, fmt.Errorf("%s: no @@ hunks found", from)
			}
			files = append(files, f)
		}
	}
	return files, nil
}

// lineMatchers are tried in order: exact, then ignoring trailing whitespace,
// then ignoring all surrounding whitespace (models often get indentation of
// context lines slightly wrong).
var lineMatchers = []func(a, b string) bool{
	func(a, b string) bool { return a == b },
	func(a, b string) bool { return strings.TrimRight(a, " \t") == strings.TrimRight(b, " \t") },
	func(a, b string) bool { return strings.TrimSpace(a) == strings.TrimSpace(b) },
}

// seek returns every index >= from where pat occurs, using the strictest
// matcher that finds anything.
func seek(lines, pat []string, from int, eof bool) []int {
	for _, eq := range lineMatchers {
		var hits []int
		for i := from; i+len(pat) <= len(lines); i++ {
			if eof && i != len(lines)-len(pat) {
				continue
			}
			ok := true
			for j := range pat {
				if !eq(lines[i+j], pat[j]) {
					ok = false
					break
				}
			}
			if ok {
				hits = append(hits, i)
			}
		}
		if len(hits) > 0 {
			return hits
		}
	}
	return nil
}

// applyHunks applies the hunks of one file to its text. Hunks are located in
// order, each after the previous one.
func applyHunks(text string, hunks []hunk) (string, error) {
	finalNL := text == "" || strings.HasSuffix(text, "\n")
	lines := splitLines(text)
	cursor := 0
	for n, h := range hunks {
		where := fmt.Sprintf("hunk %d", n+1)
		if h.header != "" {
			at := seek(lines, []string{h.header}, cursor, false)
			if len(at) == 0 {
				return "", fmt.Errorf("%s: the @@ line %q was not found", where, h.header)
			}
			cursor = at[0] + 1
		}
		var idx int
		switch {
		case len(h.old) == 0: // pure insertion
			switch {
			case h.header != "":
				idx = cursor
			case h.hint > 0:
				idx = min(max(h.hint-1, cursor), len(lines))
			default:
				idx = len(lines)
			}
		default:
			hits := seek(lines, h.old, cursor, h.eof)
			switch {
			case len(hits) == 0:
				return "", fmt.Errorf("%s: these lines were not found (re-read the file and copy them exactly):\n%s%s",
					where, strings.Join(h.old, "\n"), closeMatch(strings.Join(lines, "\n"), strings.Join(h.old, "\n")))
			case len(hits) == 1 || h.header != "":
				idx = hits[0]
			case h.hint > 0:
				idx = nearest(hits, h.hint-1)
			default:
				return "", fmt.Errorf("%s: its lines match %d places; add more context lines or an @@ line naming the enclosing function", where, len(hits))
			}
		}
		lines = append(lines[:idx], append(append([]string{}, h.new...), lines[idx+len(h.old):]...)...)
		cursor = idx + len(h.new)
	}
	out := strings.Join(lines, "\n")
	if len(lines) > 0 && finalNL {
		out += "\n"
	}
	return out, nil
}

func nearest(hits []int, target int) int {
	best := hits[0]
	for _, h := range hits[1:] {
		if abs(h-target) < abs(best-target) {
			best = h
		}
	}
	return best
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
