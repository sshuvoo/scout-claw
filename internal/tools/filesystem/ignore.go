package filesystem

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"scoutclaw/internal/pathglob"
)

type ignoreRule struct {
	re      *regexp.Regexp
	negate  bool
	dirOnly bool
}

// ignoreFile holds the rules of one .gitignore; they apply to paths below base.
type ignoreFile struct {
	base  string
	rules []ignoreRule
}

func parseIgnoreFile(path string) *ignoreFile {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	ig := &ignoreFile{base: filepath.Dir(path)}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if r, ok := parseIgnoreLine(sc.Text()); ok {
			ig.rules = append(ig.rules, r)
		}
	}
	if len(ig.rules) == 0 {
		return nil
	}
	return ig
}

// parseIgnoreLine implements the gitignore pattern rules that matter in
// practice: comments, negation, trailing "/" for directories, anchoring by a
// leading or inner "/", and "**".
func parseIgnoreLine(line string) (ignoreRule, bool) {
	line = strings.TrimRight(line, " \t\r")
	if line == "" || strings.HasPrefix(line, "#") {
		return ignoreRule{}, false
	}
	var r ignoreRule
	if strings.HasPrefix(line, "!") {
		r.negate, line = true, line[1:]
	} else if strings.HasPrefix(line, `\`) {
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		r.dirOnly, line = true, strings.TrimRight(line, "/")
	}
	if line == "" {
		return ignoreRule{}, false
	}
	anchored := strings.Contains(line, "/")
	line = strings.TrimPrefix(line, "/")
	if !anchored {
		line = "**/" + line
	}
	re, err := pathglob.Regex(line)
	if err != nil {
		return ignoreRule{}, false
	}
	r.re = re
	return r, true
}

// decide reports whether rel (relative to the file's base) is ignored or
// re-included by this file; matched is false if no rule applies.
func (ig *ignoreFile) decide(rel string, isDir bool) (ignored, matched bool) {
	for _, r := range ig.rules {
		if r.dirOnly && !isDir {
			continue
		}
		if r.re.MatchString(rel) {
			ignored, matched = !r.negate, true // the last matching rule wins
		}
	}
	return ignored, matched
}

// ignorer evaluates the .gitignore files that apply to a walk: those of the
// walk root's ancestors up to the repository root, and those found on the way down.
type ignorer struct {
	files map[string]*ignoreFile // directory -> its .gitignore (nil when absent)
}

func newIgnorer(root string) *ignorer {
	ig := &ignorer{files: map[string]*ignoreFile{}}
	for dir := root; ; dir = filepath.Dir(dir) {
		ig.load(dir)
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil || filepath.Dir(dir) == dir {
			break
		}
	}
	return ig
}

func (ig *ignorer) load(dir string) *ignoreFile {
	if f, ok := ig.files[dir]; ok {
		return f
	}
	f := parseIgnoreFile(filepath.Join(dir, ".gitignore"))
	ig.files[dir] = f
	return f
}

// ignored reports whether abs is excluded. Deeper .gitignore files override
// shallower ones, as in git.
func (ig *ignorer) ignored(abs string, isDir bool) bool {
	var chain []*ignoreFile
	for dir := filepath.Dir(abs); ; dir = filepath.Dir(dir) {
		if f, ok := ig.files[dir]; ok && f != nil {
			chain = append(chain, f)
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	result := false
	for i := len(chain) - 1; i >= 0; i-- {
		rel, err := filepath.Rel(chain[i].base, abs)
		if err != nil {
			continue
		}
		if ign, ok := chain[i].decide(filepath.ToSlash(rel), isDir); ok {
			result = ign
		}
	}
	return result
}
