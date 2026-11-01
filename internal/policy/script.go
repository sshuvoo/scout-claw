package policy

import (
	"fmt"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// scriptCommands parses a bash script and returns every simple command in it
// (including those inside pipelines, subshells, functions and $(...)), plus
// the literal targets of its redirections. Words that are not fully literal
// after quote removal are returned as "" so callers can tell a dynamic value
// from a known one.
func scriptCommands(script string) (cmds [][]string, redirects []string, err error) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(script), "")
	if err != nil {
		return nil, nil, err
	}
	syntax.Walk(file, func(n syntax.Node) bool {
		switch x := n.(type) {
		case *syntax.CallExpr:
			if len(x.Args) == 0 {
				return true // bare assignment
			}
			argv := make([]string, len(x.Args))
			for i, w := range x.Args {
				argv[i], _ = literal(w)
			}
			cmds = append(cmds, argv)
		case *syntax.Redirect:
			if x.Word != nil && x.Op != syntax.Hdoc && x.Op != syntax.DashHdoc {
				if s, ok := literal(x.Word); ok {
					redirects = append(redirects, s)
				}
			}
		}
		return true
	})
	return cmds, redirects, nil
}

// literal returns the value of w after quote removal, if it contains no
// expansions. r”m and "rm" both yield rm; $X and $(cmd) do not. Glob
// characters are kept as-is (rm -rf /* yields "/*").
func literal(w *syntax.Word) (string, bool) {
	var b strings.Builder
	var add func(parts []syntax.WordPart) bool
	add = func(parts []syntax.WordPart) bool {
		for _, p := range parts {
			switch x := p.(type) {
			case *syntax.Lit:
				b.WriteString(x.Value)
			case *syntax.SglQuoted:
				b.WriteString(x.Value)
			case *syntax.DblQuoted:
				if !add(x.Parts) {
					return false
				}
			default:
				return false
			}
		}
		return true
	}
	if !add(w.Parts) {
		return "", false
	}
	return b.String(), true
}

// classifyBash classifies a bash script by every command in it. Scripts are
// always high risk; the result is forbidden if any command, nested script or
// redirection target would be, and Effects lists what will run.
func (e *Engine) classifyBash(script, workdir string) Decision {
	if why := forbiddenScript(script); why != "" {
		return Decision{Risk: High, Forbidden: true, Reason: why}
	}
	d := Decision{Risk: High, Reason: "shell script: every construct in it (pipes, redirects, substitutions) is an execution surface",
		Recovery: "depends on the script; review it carefully"}
	cmds, redirects, err := scriptCommands(script)
	if err != nil {
		d.Reason = "shell script that does not parse as bash (" + err.Error() + ")"
		d.Effects = "runs the full script through bash"
		return d
	}
	for _, r := range redirects {
		if IsSensitive(e.Scope.Resolve(r, workdir)) {
			return forbid(fmt.Sprintf("redirection to %q, a credential or key file", r))
		}
	}
	var summary []string
	dynamic := false
	for _, argv := range cmds {
		if argv[0] == "" {
			dynamic = true
			summary = append(summary, "<dynamic command>")
			continue
		}
		known := argv[:0:0]
		for _, a := range argv {
			if a != "" {
				known = append(known, a)
			}
		}
		if c := e.classifyArgv(known, workdir); c.Forbidden {
			return c
		}
		summary = append(summary, strings.Join(known, " "))
	}
	if dynamic {
		d.Reason += "; some command names are computed at run time and could not be checked"
	}
	d.Effects = "runs: " + truncate(strings.Join(summary, "; "), 400)
	return d
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

const (
	psDelete    = `\b(Remove-Item|ri|rm|del|erase|rd|rmdir)\b`
	psProtected = `['"]?([a-z]:[\\/]?\*?|\$env:(SystemDrive|SystemRoot|windir|USERPROFILE|HOMEDRIVE|ProgramFiles|ProgramData)[\\/]?\*?|~[\\/]?\*?|\$HOME[\\/]?\*?|[a-z]:[\\/](Windows|Users|Program Files( \(x86\))?|ProgramData)[\\/]?\*?)['"]?`
)

// PowerShell has no Go parser, so its scripts are checked against patterns
// only and are always high risk.
var forbiddenPowerShellPatterns = []struct {
	re  *regexp.Regexp
	why string
}{
	{regexp.MustCompile(`(?i)` + psDelete + `[^\n;|]*\s(-r\w*|/s)\b[^\n;|]*\s` + psProtected + `(\s|;|\||$)`), "recursive deletion of a drive, the home directory or a system directory"},
	{regexp.MustCompile(`(?i)` + psDelete + `\s+(-\w+\s+)*` + psProtected + `\s[^\n;|]*(-r\w*|/s)\b`), "recursive deletion of a drive, the home directory or a system directory"},
	{regexp.MustCompile(`(?i)\b(Format-Volume|Clear-Disk|Initialize-Disk|Remove-Partition)\b|\bformat(\.com)?\s+[a-z]:`), "formatting or wiping a disk"},
	{regexp.MustCompile(`(?i)\bdiskpart\b`), "running diskpart"},
	{regexp.MustCompile(`(?i)\b(vssadmin|wbadmin)\b[^\n;|]*\bdelete\b|\bbcdedit\b[^\n;|]*\b(recoveryenabled|bootstatuspolicy)\b`), "deleting backups or disabling system recovery"},
	{regexp.MustCompile(`(?i)\bSet-MpPreference\b[^\n;|]*-Disable|\bAdd-MpPreference\b[^\n;|]*-Exclusion`), "weakening antivirus protection"},
	{regexp.MustCompile(`(?i)\s-(e|ec|enc|encodedcommand)\s+[A-Za-z0-9+/=]{8,}`), "encoded PowerShell command that cannot be reviewed"},
	{regexp.MustCompile(`(?i)\breg(\.exe)?\s+save\s+hklm\\(sam|security|system)\b`), "dumping the credential registry hives"},
	{regexp.MustCompile(`(?i)\.ssh[\\/]id_|\.aws[\\/]credentials|\.git-credentials|\.gnupg|Microsoft[\\/](Credentials|Protect)[\\/]`), "touches credential or key files"},
}

var (
	psDownload = regexp.MustCompile(`(?i)\b(iwr|irm|curl|wget|Invoke-WebRequest|Invoke-RestMethod|DownloadString|DownloadFile|Net\.WebClient|Start-BitsTransfer)\b`)
	psEval     = regexp.MustCompile(`(?i)\b(iex|Invoke-Expression)\b|\[ScriptBlock\]::Create`)
)

func forbiddenPowerShell(script string) string {
	for _, p := range forbiddenPowerShellPatterns {
		if p.re.MatchString(script) {
			return "refused: " + p.why
		}
	}
	if psDownload.MatchString(script) && psEval.MatchString(script) {
		return "refused: evaluating downloaded content as code"
	}
	return ""
}
