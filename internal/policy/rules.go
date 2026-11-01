package policy

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"scoutclaw/internal/pathglob"
)

// Permission rules come from config files: "Tool" or "Tool(spec)". The spec
// is a path glob for file tools, a command for shell_exec/proc_start and
// "domain:host-pattern" for network tools.

type RuleAction int

const (
	RuleAllow RuleAction = iota
	RuleAsk
	RuleDeny
)

func (a RuleAction) String() string { return [...]string{"allow", "ask", "deny"}[a] }

// ParseRuleAction accepts allow, ask or deny.
func ParseRuleAction(s string) (RuleAction, error) {
	switch s {
	case "allow":
		return RuleAllow, nil
	case "ask":
		return RuleAsk, nil
	case "deny":
		return RuleDeny, nil
	}
	return 0, fmt.Errorf("unknown rule action %q (want allow, ask or deny)", s)
}

type specKind int

const (
	specNone specKind = iota
	specPath
	specCommand
	specDomain
)

type Rule struct {
	Action   RuleAction
	Text     string
	Origin   string // config file the rule came from
	Personal bool   // from the global or personal file: may allow high-risk actions

	tool   *regexp.Regexp
	kind   specKind
	glob   pathglob.Glob  // relative path pattern, anchored at the project root
	abs    *regexp.Regexp // absolute path pattern (/etc/**, ~/notes/**)
	words  []string       // command words
	prefix bool           // command ends with " *"
	domain *regexp.Regexp
}

var ruleSyntax = regexp.MustCompile(`^([a-z0-9_*]+)(?:\((.*)\))?$`)

// scriptTools run arbitrary code; patterns cannot describe what they do.
var scriptTools = []string{"shell_script", "shell_powershell"}

// ParseRule parses one rule. personal marks rules from files only the user writes.
func ParseRule(text string, action RuleAction, origin string, personal bool) (Rule, error) {
	text = strings.TrimSpace(text)
	m := ruleSyntax.FindStringSubmatch(text)
	if m == nil {
		return Rule{}, fmt.Errorf("rule %q: want Tool or Tool(spec), e.g. fs_write(src/**) or shell_exec(go test *)", text)
	}
	r := Rule{Action: action, Text: text, Origin: origin, Personal: personal}
	toolRe, err := regexp.Compile("^" + strings.ReplaceAll(regexp.QuoteMeta(m[1]), `\*`, `[a-z0-9_]*`) + "$")
	if err != nil {
		return Rule{}, err
	}
	r.tool = toolRe
	hasSpec := strings.Contains(text, "(")
	spec := strings.TrimSpace(m[2])
	for _, s := range scriptTools {
		if toolRe.MatchString(s) {
			if action == RuleAllow {
				return Rule{}, fmt.Errorf("rule %q: %s runs arbitrary code, so it can be denied or asked about but never allowed by a rule", text, s)
			}
			if hasSpec {
				return Rule{}, fmt.Errorf("rule %q: %s takes no pattern (scripts cannot be matched safely); use %s alone", text, s, s)
			}
		}
	}
	if !hasSpec {
		return r, nil
	}
	if spec == "" {
		return Rule{}, fmt.Errorf("rule %q: empty pattern; drop the parentheses to match every use", text)
	}
	family := strings.TrimSuffix(strings.SplitN(m[1], "*", 2)[0], "_")
	switch {
	case strings.HasPrefix(m[1], "fs_") || m[1] == "project_info":
		r.kind = specPath
		p := filepath.ToSlash(spec)
		if p == "~" || strings.HasPrefix(p, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				p = filepath.ToSlash(home) + strings.TrimPrefix(p, "~")
			}
		}
		if strings.HasPrefix(p, "/") || filepath.IsAbs(spec) || (len(p) > 2 && p[1] == ':') {
			if r.abs, err = pathglob.Regex(foldPath(resolveGlobPrefix(p))); err != nil {
				return Rule{}, fmt.Errorf("rule %q: %v", text, err)
			}
		} else if r.glob, err = pathglob.Compile(foldPath(p)); err != nil {
			return Rule{}, fmt.Errorf("rule %q: %v", text, err)
		}
	case m[1] == "shell_exec" || m[1] == "proc_start":
		r.kind = specCommand
		r.words = strings.Fields(spec)
		if n := len(r.words); n > 0 && r.words[n-1] == "*" {
			r.prefix, r.words = true, r.words[:n-1]
		}
		if len(r.words) == 0 {
			return Rule{}, fmt.Errorf("rule %q: name the command, e.g. shell_exec(go test *)", text)
		}
	case strings.HasPrefix(m[1], "net_"):
		host, ok := strings.CutPrefix(spec, "domain:")
		if !ok || host == "" {
			return Rule{}, fmt.Errorf("rule %q: network rules take domain:host, e.g. net_http(domain:*.github.com)", text)
		}
		r.kind = specDomain
		r.domain = regexp.MustCompile("(?i)^" + strings.ReplaceAll(regexp.QuoteMeta(host), `\*`, `.*`) + "$")
	default:
		return Rule{}, fmt.Errorf("rule %q: %s rules take no pattern (family %q); use %s alone", text, m[1], family, m[1])
	}
	return r, nil
}

// resolveGlobPrefix resolves symlinks in the fixed directory part of an
// absolute pattern, because input paths are compared after symlink
// resolution (/tmp/x/** must match /private/tmp/x/a on macOS).
func resolveGlobPrefix(p string) string {
	cut := strings.IndexAny(p, "*?[{")
	if cut < 0 {
		cut = len(p)
	}
	slash := strings.LastIndex(p[:cut], "/")
	if slash <= 0 {
		return p
	}
	dir := filepath.FromSlash(p[:slash])
	return filepath.ToSlash(realPath(dir)) + p[slash:]
}

func foldPath(p string) string {
	if caseInsensitiveFS {
		return strings.ToLower(p)
	}
	return p
}

// Describe names the rule and where it came from, for decisions and the audit log.
func (r Rule) Describe() string {
	if r.Origin == "" {
		return r.Action.String() + " " + r.Text
	}
	return fmt.Sprintf("%s %s (%s)", r.Action, r.Text, r.Origin)
}

// RuleSet holds the rules of all config files. Relative path patterns are
// anchored at Base, the project root.
type RuleSet struct {
	Rules []Rule
	Base  string
}

// Add appends a rule (used when the user picks "always in this project").
func (rs *RuleSet) Add(r Rule) { rs.Rules = append(rs.Rules, r) }

// match returns the first rule of the given action that applies to the call.
func (rs RuleSet) match(action RuleAction, tool string, input json.RawMessage, scope Scope) (Rule, bool) {
	for _, r := range rs.Rules {
		if r.Action == action && r.tool.MatchString(tool) && r.applies(tool, input, scope, rs.Base) {
			return r, true
		}
	}
	return Rule{}, false
}

func (r Rule) applies(tool string, input json.RawMessage, scope Scope, base string) bool {
	switch r.kind {
	case specNone:
		return true
	case specPath:
		paths := inputPaths(tool, input, scope)
		if len(paths) == 0 {
			return false
		}
		// deny and ask apply if any path matches; allow only if all of them do,
		// so an allow rule for src/** cannot approve a patch that also touches other files.
		for _, p := range paths {
			ok := r.matchPath(p, base)
			if ok && r.Action != RuleAllow {
				return true
			}
			if !ok && r.Action == RuleAllow {
				return false
			}
		}
		return r.Action == RuleAllow
	case specCommand:
		argv := commandArgv(input)
		if len(argv) < len(r.words) || (!r.prefix && len(argv) != len(r.words)) {
			return false
		}
		for i, w := range r.words {
			if argv[i] != w {
				return false
			}
		}
		return true
	case specDomain:
		host := inputHost(tool, input)
		return host != "" && r.domain.MatchString(host)
	}
	return false
}

func (r Rule) matchPath(abs, base string) bool {
	if r.abs != nil {
		return r.abs.MatchString(foldPath(filepath.ToSlash(abs)))
	}
	if base == "" {
		return false
	}
	rel, err := filepath.Rel(base, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return r.glob.Match(foldPath(filepath.ToSlash(rel)))
}

// inputPaths extracts every path a call touches, resolved like the tools do.
func inputPaths(tool string, input json.RawMessage, scope Scope) []string {
	var in struct {
		Path  string `json:"path"`
		Src   string `json:"src"`
		Dest  string `json:"dest"`
		Patch string `json:"patch"`
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	_ = json.Unmarshal(input, &in)
	var raw []string
	switch tool {
	case "fs_copy", "fs_move":
		raw = []string{in.Src, in.Dest}
	case "fs_read_many":
		for _, f := range in.Files {
			raw = append(raw, f.Path)
		}
	case "fs_apply_patch":
		raw, _ = patchPaths(in.Patch)
	default:
		p := in.Path
		if p == "" {
			p = "."
		}
		raw = []string{p}
	}
	var out []string
	for _, p := range raw {
		if p != "" {
			out = append(out, scope.Resolve(p, ""))
		}
	}
	return out
}

func commandArgv(input json.RawMessage) []string {
	var in execInput
	if json.Unmarshal(input, &in) != nil || in.Command == "" {
		return nil
	}
	name := filepath.Base(in.Command)
	if runtime.GOOS == "windows" {
		name = strings.TrimSuffix(strings.ToLower(name), ".exe")
	}
	return append([]string{name}, in.Args...)
}

func inputHost(tool string, input json.RawMessage) string {
	var in struct {
		Host string `json:"host"`
		URL  string `json:"url"`
	}
	_ = json.Unmarshal(input, &in)
	if in.URL != "" {
		if u, err := url.Parse(in.URL); err == nil {
			return u.Hostname()
		}
	}
	return in.Host
}

// SuggestRule proposes the rule saved when the user approves an action for
// the project ("always"). It is narrow on purpose: the same program and
// subcommand, or the same file tool. Empty means no rule should be offered.
func SuggestRule(tool string, input json.RawMessage, d Decision) string {
	if d.Risk != Medium || d.Outside || d.Forbidden || d.Deletes {
		return ""
	}
	switch {
	case tool == "shell_exec" || tool == "proc_start":
		argv := commandArgv(input)
		if len(argv) == 0 {
			return ""
		}
		words := []string{argv[0]}
		for _, a := range argv[1:] {
			if !strings.HasPrefix(a, "-") {
				words = append(words, a)
				break
			}
		}
		return fmt.Sprintf("%s(%s *)", tool, strings.Join(words, " "))
	case strings.HasPrefix(tool, "fs_"):
		return tool
	}
	return ""
}
