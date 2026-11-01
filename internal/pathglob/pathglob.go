// Package pathglob matches slash-separated paths against shell-style globs
// with "**". It is shared by the file search tools, .gitignore handling and
// permission rules.
package pathglob

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Regex translates a glob into an anchored regular expression over
// slash-separated paths. "*" and "?" stay within one path element, "**"
// crosses elements, and "[...]" and "{a,b}" work as in most shells.
func Regex(glob string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch c {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				i++
				switch {
				case i+1 < len(glob) && glob[i+1] == '/': // "**/" = zero or more directories
					i++
					b.WriteString("(?:.*/)?")
				default:
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '[':
			j := strings.IndexByte(glob[i+1:], ']')
			if j < 0 {
				b.WriteString(`\[`)
				continue
			}
			class := glob[i+1 : i+1+j]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i += j + 1
		case '{':
			j := strings.IndexByte(glob[i+1:], '}')
			if j < 0 {
				b.WriteString(`\{`)
				continue
			}
			var alts []string
			for _, a := range strings.Split(glob[i+1:i+1+j], ",") {
				alts = append(alts, regexp.QuoteMeta(a))
			}
			b.WriteString("(?:" + strings.Join(alts, "|") + ")")
			i += j + 1
		case '\\':
			if i+1 < len(glob) {
				i++
				b.WriteString(regexp.QuoteMeta(string(glob[i])))
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// Glob matches a slash-separated relative path. A glob without a slash
// matches the base name at any depth ("*.go"); one with a slash matches the
// whole relative path ("src/**/*.go").
type Glob struct {
	re       *regexp.Regexp
	baseOnly bool
}

// Compile parses a path glob.
func Compile(glob string) (Glob, error) {
	glob = filepath.ToSlash(glob)
	re, err := Regex(strings.TrimPrefix(glob, "./"))
	return Glob{re: re, baseOnly: !strings.Contains(glob, "/")}, err
}

// Match reports whether the slash-separated relative path matches.
func (g Glob) Match(rel string) bool {
	if g.baseOnly {
		return g.re.MatchString(rel[strings.LastIndexByte(rel, '/')+1:])
	}
	return g.re.MatchString(rel)
}
