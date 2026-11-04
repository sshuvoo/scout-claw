// Package policy classifies every proposed tool call by risk and decides
// whether it may run, needs approval, or is refused. It is enforced in code by
// the executor; the system prompt is not a security boundary.
package policy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"scoutclaw/internal/config"
)

type Risk int

const (
	Low Risk = iota
	Medium
	High
)

func (r Risk) String() string { return [...]string{"low", "medium", "high"}[r] }

type Action int

const (
	Allow Action = iota
	Ask
	Deny
)

func (a Action) String() string { return [...]string{"allow", "ask", "deny"}[a] }

// Decision is the policy verdict for one tool call.
type Decision struct {
	Risk      Risk
	Action    Action
	Forbidden bool   // refused regardless of approval mode
	Reason    string // why this risk level / why it was refused
	Effects   string // expected side effects, shown when asking for approval
	Recovery  string // how to undo, if known (shown for high risk)
	Key       string // stable key for "allow for this session" (medium risk only)
	Outside   bool   // touches paths outside the workspace
	Deletes   bool   // removes files; the "edits" approval mode never auto-approves this
	Rule      string // the config rule that decided the action, if any
}

type Engine struct {
	Scope Scope
	Mode  config.Approval
	Rules RuleSet // permission rules from config files
}

func New(scope Scope, mode config.Approval) *Engine { return &Engine{Scope: scope, Mode: mode} }

// Decide classifies the call, applies permission rules and then the approval
// mode. Order: built-in refusals, deny rules, ask rules, allow rules, mode.
func (e *Engine) Decide(tool string, input json.RawMessage) Decision {
	d := e.Classify(tool, input)
	if d.Forbidden {
		d.Action = Deny
		return d
	}
	if r, ok := e.Rules.match(RuleDeny, tool, input, e.Scope); ok {
		d.Action, d.Rule = Deny, r.Describe()
		d.Reason = "denied by rule " + r.Describe()
		return d
	}
	if r, ok := e.Rules.match(RuleAsk, tool, input, e.Scope); ok {
		d.Rule = r.Describe()
		if e.Mode == config.ApprovalReadOnly && d.Risk != Low {
			d.Action, d.Reason = Deny, "approval policy is readonly; "+d.Reason
		} else {
			d.Action = Ask
			d.Key = "" // an ask rule means: ask every time
		}
		return d
	}
	if r, ok := e.Rules.match(RuleAllow, tool, input, e.Scope); ok && e.Mode != config.ApprovalReadOnly && (d.Risk != High || r.Personal) {
		d.Action, d.Rule = Allow, r.Describe()
		return d
	}
	switch {
	case d.Forbidden:
		d.Action = Deny
	case d.Risk == Low:
		d.Action = Allow
	case e.Mode == config.ApprovalReadOnly:
		d.Action = Deny
		d.Reason = "approval policy is readonly; " + d.Reason
	case d.Risk == Medium && e.Mode == config.ApprovalAuto:
		d.Action = Allow
	case d.Risk == Medium && e.Mode == config.ApprovalEdits && fileEditTools[tool] && !d.Outside && !d.Deletes:
		d.Action = Allow
	case tool == "fs_patch" && d.Risk == Medium && !d.Outside && !d.Deletes:
		// fs_patch only ever touches text it already read (old_string ->
		// new_string); it keeps a backup and, inside a git repo, is also
		// trivially diffable/revertable, so it never needs a popup on its
		// own — only "outside workspace" and "readonly mode" still gate it.
		d.Action = Allow
	default:
		d.Action = Ask
	}
	return d
}

// Classify assigns a risk level from the tool, its actual arguments and targets.
func (e *Engine) Classify(tool string, input json.RawMessage) Decision {
	switch tool {
	case "fs_list", "fs_read", "fs_stat", "fs_search", "fs_changes", "fs_glob", "fs_grep":
		return e.classifyFSRead(input)
	case "fs_read_many":
		return e.classifyReadMany(input)
	case "fs_apply_patch":
		return e.classifyApplyPatch(input)
	case "project_info":
		return e.classifyFSRead(input)
	case "fs_write", "fs_patch", "fs_lines", "fs_mkdir":
		return e.classifyFSWrite(tool, input)
	case "fs_copy", "fs_move":
		return e.classifyTransfer(tool, input)
	case "fs_delete":
		return e.classifyDelete(input)
	case "fs_undo":
		return Decision{Risk: Medium, Reason: "reverts file changes ScoutClaw made in this session", Effects: "restores earlier file contents",
			Recovery: "the current contents are backed up before they are replaced", Key: "fs_undo:workspace"}
	case "shell_exec":
		return e.classifyExec(input)
	case "proc_start":
		return e.classifyProcStart(input)
	case "proc_output", "proc_list", "proc_stop":
		return Decision{Risk: Low, Reason: "inspects or stops a background process ScoutClaw started"}
	case "shell_script", "shell_powershell":
		return e.classifyScript(tool, input)
	case "sys_info", "process_list", "env_list", "dev_runtimes",
		"net_dns", "net_connect", "net_http", "update_plan", "finish":
		return Decision{Risk: Low, Reason: "bounded read-only diagnostic"}
	}
	return Decision{Risk: High, Forbidden: true, Reason: fmt.Sprintf("unknown tool %q has no policy", tool)}
}

func (e *Engine) classifyFSRead(input json.RawMessage) Decision {
	var in struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(input, &in) != nil || in.Path == "" {
		return Decision{Risk: Low, Reason: "read-only"} // invalid input is rejected by validation
	}
	abs := e.Scope.Resolve(in.Path, "")
	if IsSensitive(abs) {
		return Decision{Risk: High, Forbidden: true, Reason: fmt.Sprintf("%s is a credential or key file; ScoutClaw never reads these", abs)}
	}
	return Decision{Risk: Low, Reason: "read-only inspection"}
}

func (e *Engine) classifyReadMany(input json.RawMessage) Decision {
	var in struct {
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	_ = json.Unmarshal(input, &in)
	for _, f := range in.Files {
		if abs := e.Scope.Resolve(f.Path, ""); f.Path != "" && IsSensitive(abs) {
			return Decision{Risk: High, Forbidden: true, Reason: fmt.Sprintf("%s is a credential or key file; ScoutClaw never reads these", abs)}
		}
	}
	return Decision{Risk: Low, Reason: "read-only inspection"}
}

// fileEditTools are auto-approved inside the workspace in the "edits" approval mode.
var fileEditTools = map[string]bool{"fs_write": true, "fs_patch": true, "fs_lines": true, "fs_mkdir": true,
	"fs_copy": true, "fs_move": true, "fs_undo": true, "fs_apply_patch": true}

// patchPaths extracts every path a patch touches (both formats accepted by
// fs_apply_patch) and which of them it deletes.
func patchPaths(patch string) (paths []string, deleted map[string]bool) {
	deleted = map[string]bool{}
	lines := strings.Split(strings.ReplaceAll(patch, "\r\n", "\n"), "\n")
	strip := func(p string) string {
		p = strings.TrimSpace(p)
		if i := strings.IndexByte(p, '\t'); i >= 0 {
			p = p[:i]
		}
		if strings.HasPrefix(p, "a/") || strings.HasPrefix(p, "b/") {
			p = p[2:]
		}
		return p
	}
	for i, l := range lines {
		for _, prefix := range []string{"*** Add File: ", "*** Update File: ", "*** Move to: ", "*** Delete File: "} {
			if p, ok := strings.CutPrefix(l, prefix); ok {
				paths = append(paths, strings.TrimSpace(p))
				if prefix == "*** Delete File: " {
					deleted[strings.TrimSpace(p)] = true
				}
			}
		}
		if strings.HasPrefix(l, "--- ") && i+1 < len(lines) && strings.HasPrefix(lines[i+1], "+++ ") {
			from, to := strip(l[4:]), strip(lines[i+1][4:])
			for _, p := range []string{from, to} {
				if p != "/dev/null" {
					paths = append(paths, p)
				}
			}
			if to == "/dev/null" && from != "/dev/null" {
				deleted[from] = true
			}
		}
	}
	return paths, deleted
}

func (e *Engine) classifyApplyPatch(input json.RawMessage) Decision {
	var in struct {
		Patch string `json:"patch"`
	}
	_ = json.Unmarshal(input, &in)
	paths, deleted := patchPaths(in.Patch)
	if len(paths) == 0 {
		return Decision{Risk: Medium, Reason: "file modification", Key: "fs_apply_patch:workspace"}
	}
	worst, outside := Decision{Risk: Low}, false
	for _, p := range paths {
		var pd Decision
		if deleted[p] {
			in, _ := json.Marshal(map[string]string{"path": p})
			pd = e.classifyDelete(in)
		} else {
			pd = e.classifyWritePath("fs_apply_patch", e.Scope.Resolve(p, ""))
		}
		outside = outside || pd.Outside
		worst = higher(worst, pd)
	}
	d := Decision{Risk: worst.Risk, Forbidden: worst.Forbidden, Outside: outside, Deletes: len(deleted) > 0,
		Reason:   fmt.Sprintf("patch changes %d file(s) inside the workspace", len(paths)),
		Effects:  "applies a patch to: " + strings.Join(paths, ", "),
		Recovery: "previous contents are backed up and deleted files go to the session trash; fs_undo reverts the patch"}
	if worst.Forbidden || worst.Risk == High {
		d.Reason = fmt.Sprintf("patch changes %d file(s); %s", len(paths), worst.Reason)
	}
	if d.Risk == Medium && !d.Deletes {
		d.Key = "fs_apply_patch:workspace"
	}
	return d
}

func (e *Engine) classifyFSWrite(tool string, input json.RawMessage) Decision {
	var in struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(input, &in)
	if in.Path == "" {
		return Decision{Risk: Medium, Reason: "file modification", Key: tool + ":workspace"}
	}
	return e.classifyWritePath(tool, e.Scope.Resolve(in.Path, ""))
}

// classifyWritePath rates a change to one resolved path.
func (e *Engine) classifyWritePath(tool, abs string) Decision {
	switch {
	case IsSensitive(abs):
		return Decision{Risk: High, Forbidden: true, Reason: fmt.Sprintf("%s is a credential or key file; ScoutClaw never modifies these", abs)}
	case !e.Scope.Contains(abs):
		return Decision{Risk: High, Outside: true, Reason: fmt.Sprintf("modifies %s, outside the workspace %s", abs, e.Scope.Root),
			Effects:  "changes a file outside the project",
			Recovery: "ScoutClaw keeps a backup of the previous contents and prints its location"}
	case tool != "fs_mkdir" && inGitRepo(abs):
		return Decision{Risk: Low, Reason: "edits a file inside a git repository; use git diff/checkout to review or revert"}
	}
	return Decision{Risk: Medium, Reason: "modifies a file inside the workspace", Effects: "edits " + abs,
		Recovery: "a backup of the previous contents is kept", Key: tool + ":workspace"}
}

// inGitRepo reports whether abs sits inside a git working tree, walking up
// from its directory to look for a ".git" entry (directory for a normal
// repo, file for a worktree or submodule).
func inGitRepo(abs string) bool {
	dir := filepath.Dir(abs)
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

func higher(a, b Decision) Decision {
	if b.Forbidden || (!a.Forbidden && b.Risk > a.Risk) {
		return b
	}
	return a
}

func (e *Engine) classifyTransfer(tool string, input json.RawMessage) Decision {
	var in struct {
		Src  string `json:"src"`
		Dest string `json:"dest"`
	}
	_ = json.Unmarshal(input, &in)
	if in.Src == "" || in.Dest == "" {
		return Decision{Risk: Medium, Reason: "file operation", Key: tool + ":workspace"}
	}
	src, dest := e.Scope.ResolveNoFollow(in.Src, ""), e.Scope.ResolveNoFollow(in.Dest, "")
	if IsSensitive(src) {
		return Decision{Risk: High, Forbidden: true, Reason: fmt.Sprintf("%s is a credential or key file; ScoutClaw never copies or moves these", src)}
	}
	d := e.classifyWritePath(tool, dest)
	if tool == "fs_move" {
		if within(src, e.Scope.Root) || isProtectedDir(src) {
			return Decision{Risk: High, Forbidden: true, Reason: fmt.Sprintf("moving %s would move the workspace, the home directory or a system directory", src)}
		}
		d = higher(d, e.classifyWritePath(tool, src))
	}
	verb := map[string]string{"fs_copy": "copies", "fs_move": "moves"}[tool]
	d.Effects = fmt.Sprintf("%s %s to %s", verb, src, dest)
	return d
}

func (e *Engine) classifyDelete(input json.RawMessage) Decision {
	var in struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
	}
	_ = json.Unmarshal(input, &in)
	if in.Path == "" {
		return Decision{Risk: Medium, Reason: "file deletion"}
	}
	abs := e.Scope.ResolveNoFollow(in.Path, "")
	recovery := "moved to the session trash; fs_undo restores it"
	switch {
	case IsSensitive(abs):
		return Decision{Risk: High, Forbidden: true, Reason: fmt.Sprintf("%s is a credential or key file; ScoutClaw never deletes these", abs)}
	case abs == e.Scope.Root || within(abs, e.Scope.Root) || isProtectedDir(abs):
		return Decision{Risk: High, Forbidden: true, Reason: fmt.Sprintf("deleting %s would remove the workspace, the home directory or a system directory", abs)}
	case filepath.Base(abs) == ".git":
		return Decision{Risk: High, Forbidden: true, Reason: "deleting a .git directory destroys the repository history"}
	case !e.Scope.Contains(abs):
		return Decision{Risk: High, Outside: true, Reason: fmt.Sprintf("deletes %s, outside the workspace %s", abs, e.Scope.Root),
			Effects: "removes a file or directory outside the project", Recovery: recovery}
	case in.Recursive:
		return Decision{Risk: High, Reason: "recursively deletes a directory inside the workspace", Effects: "removes " + abs + " and everything in it", Recovery: recovery}
	}
	return Decision{Risk: Medium, Reason: "deletes a file inside the workspace", Effects: "removes " + abs, Recovery: recovery, Key: "fs_delete:workspace", Deletes: true}
}

// within reports whether child is inside parent (or equal to it).
func within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

type execInput struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Workdir string   `json:"workdir"`
}

func (e *Engine) classifyExec(input json.RawMessage) Decision {
	var in execInput
	if json.Unmarshal(input, &in) != nil || in.Command == "" {
		return Decision{Risk: Medium, Reason: "unparseable command"}
	}
	workdir := e.Scope.Root
	if in.Workdir != "" {
		workdir = e.Scope.Resolve(in.Workdir, "")
	}
	argv := append([]string{in.Command}, in.Args...)
	d := e.classifyArgv(argv, workdir)
	d.Effects = coalesce(d.Effects, "runs "+strings.Join(argv, " "))
	if d.Risk == Medium && !d.Forbidden && (!e.Scope.Contains(workdir) || e.touchesOutside(withoutSwitches(argv), workdir)) {
		d.Risk, d.Outside = High, true
		d.Reason += "; operates on paths outside the workspace"
	}
	return d
}

// classifyProcStart rates the command like shell_exec, but never below medium:
// a background process keeps running (and may listen on ports) after the call.
func (e *Engine) classifyProcStart(input json.RawMessage) Decision {
	d := e.classifyExec(input)
	if d.Risk == Low {
		d.Risk = Medium
		d.Reason = "starts a long-running background process"
	}
	d.Key = ""
	if d.Risk == Medium && !d.Forbidden && !d.Outside {
		// "Allow for this session" covers the same program and subcommand only.
		var in execInput
		_ = json.Unmarshal(input, &in)
		key := "proc_start:" + filepath.Base(in.Command)
		for _, a := range in.Args {
			if !strings.HasPrefix(a, "-") {
				key += " " + a
				break
			}
		}
		d.Key = key
	}
	d.Effects = "keeps running in the background until proc_stop or ScoutClaw exits; " + d.Effects
	d.Recovery = coalesce(d.Recovery, "proc_stop stops it and everything it spawned")
	return d
}

func (e *Engine) classifyScript(tool string, input json.RawMessage) Decision {
	var in struct {
		Script  string `json:"script"`
		Workdir string `json:"workdir"`
	}
	_ = json.Unmarshal(input, &in)
	workdir := e.Scope.Root
	if in.Workdir != "" {
		workdir = e.Scope.Resolve(in.Workdir, "")
	}
	if tool == "shell_script" {
		return e.classifyBash(in.Script, workdir)
	}
	if why := forbiddenPowerShell(in.Script); why != "" {
		return Decision{Risk: High, Forbidden: true, Reason: why}
	}
	return Decision{Risk: High, Reason: "PowerShell script: it can call any cmdlet, .NET API or program",
		Effects: "runs the full script through PowerShell", Recovery: "depends on the script; review it carefully"}
}

func coalesce(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
