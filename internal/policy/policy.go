// Package policy classifies every proposed tool call by risk and decides
// whether it may run, needs approval, or is refused. It is enforced in code by
// the executor; the system prompt is not a security boundary.
package policy

import (
	"encoding/json"
	"fmt"
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
}

type Engine struct {
	Scope Scope
	Mode  config.Approval
}

func New(scope Scope, mode config.Approval) *Engine { return &Engine{Scope: scope, Mode: mode} }

// Decide classifies the call and applies the approval mode.
func (e *Engine) Decide(tool string, input json.RawMessage) Decision {
	d := e.Classify(tool, input)
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
	default:
		d.Action = Ask
	}
	return d
}

// Classify assigns a risk level from the tool, its actual arguments and targets.
func (e *Engine) Classify(tool string, input json.RawMessage) Decision {
	switch tool {
	case "fs_list", "fs_read", "fs_stat", "fs_search":
		return e.classifyFSRead(input)
	case "fs_write", "fs_patch":
		return e.classifyFSWrite(tool, input)
	case "shell_exec":
		return e.classifyExec(input)
	case "shell_script":
		return e.classifyScript(input)
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

func (e *Engine) classifyFSWrite(tool string, input json.RawMessage) Decision {
	var in struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(input, &in)
	if in.Path == "" {
		return Decision{Risk: Medium, Reason: "file modification", Key: tool + ":workspace"}
	}
	abs := e.Scope.Resolve(in.Path, "")
	switch {
	case IsSensitive(abs):
		return Decision{Risk: High, Forbidden: true, Reason: fmt.Sprintf("%s is a credential or key file; ScoutClaw never modifies these", abs)}
	case !e.Scope.Contains(abs):
		return Decision{Risk: High, Outside: true, Reason: fmt.Sprintf("modifies %s, outside the workspace %s", abs, e.Scope.Root),
			Effects:  "changes a file outside the project",
			Recovery: "ScoutClaw keeps a backup of the previous contents and prints its location"}
	}
	return Decision{Risk: Medium, Reason: "modifies a file inside the workspace", Effects: "edits " + abs,
		Recovery: "a backup of the previous contents is kept", Key: tool + ":workspace"}
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
	if d.Risk == Medium && !d.Forbidden && (!e.Scope.Contains(workdir) || e.touchesOutside(argv[1:], workdir)) {
		d.Risk, d.Outside = High, true
		d.Reason += "; operates on paths outside the workspace"
	}
	return d
}

func (e *Engine) classifyScript(input json.RawMessage) Decision {
	var in struct {
		Script string `json:"script"`
	}
	_ = json.Unmarshal(input, &in)
	if why := forbiddenScript(in.Script); why != "" {
		return Decision{Risk: High, Forbidden: true, Reason: why}
	}
	return Decision{Risk: High, Reason: "shell script: every construct in it (pipes, redirects, substitutions) is an execution surface",
		Effects: "runs the full script through /bin/sh", Recovery: "depends on the script; review it carefully"}
}

func coalesce(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
