// Package executor is the only path from a model-proposed tool call to an
// actual side effect: look up -> validate -> policy -> approval -> run with
// limits -> redact/truncate -> audit -> record verification evidence.
package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"scoutclaw/internal/audit"
	"scoutclaw/internal/policy"
	"scoutclaw/internal/redact"
	"scoutclaw/internal/tools"
	"scoutclaw/internal/verify"
)

// ApprovalRequest is what the user sees before a risky action runs.
type ApprovalRequest struct {
	Tool     string
	Summary  string
	Decision policy.Decision
	Preview  string // diff for file edits
}

type ApprovalResponse struct {
	Approved bool
	Remember bool // allow the same kind of medium-risk action for the rest of the session
}

type Approver interface {
	Approve(ctx context.Context, req ApprovalRequest) (ApprovalResponse, error)
}

// DenyAll is used when no interactive approver is available.
type DenyAll struct{}

func (DenyAll) Approve(context.Context, ApprovalRequest) (ApprovalResponse, error) {
	return ApprovalResponse{}, nil
}

type Call struct {
	ID    string
	Name  string
	Input json.RawMessage
}

type Outcome struct {
	Call     Call
	Decision policy.Decision
	Approved bool
	Result   tools.Result
	Text     string // observation returned to the model
	Duration time.Duration
}

func (o Outcome) OK() bool { return o.Result.OK() }

type Executor struct {
	Registry    *tools.Registry
	Policy      *policy.Engine
	Approver    Approver
	Audit       *audit.Logger
	Redactor    *redact.Redactor
	Tracker     *verify.Tracker
	SessionID   string
	ToolTimeout time.Duration
	MaxOutput   int

	mu       sync.Mutex
	allowed  map[string]bool // session-level approvals keyed by policy key
	failures map[string]int  // identical failed calls
}

const maxIdenticalFailures = 2

func (x *Executor) Run(ctx context.Context, call Call) Outcome {
	start := time.Now()
	out := Outcome{Call: call}
	finish := func(res tools.Result) Outcome {
		out.Result = res
		out.Duration = time.Since(start)
		out.Text = x.render(res)
		x.auditResult(out)
		return out
	}
	fail := func(k tools.Kind, msg string) Outcome {
		return finish(tools.Result{Kind: k, ExitCode: 1, Output: msg})
	}

	x.Audit.Log(audit.Event{SessionID: x.SessionID, Kind: "tool_request", Tool: call.Name, Data: map[string]any{"call_id": call.ID, "input": string(call.Input)}})

	tool, ok := x.Registry.Get(call.Name)
	if !ok {
		return fail(tools.KindInvalidInput, fmt.Sprintf("unknown tool %q. Available tools: %s", call.Name, x.toolNames()))
	}
	if err := tool.Validate(call.Input); err != nil {
		return fail(tools.KindOf(err), err.Error()+"\nExpected schema: "+string(tool.InputSchema()))
	}

	fp := fingerprint(call)
	if x.failureCount(fp) >= maxIdenticalFailures {
		return fail(tools.KindFailed, fmt.Sprintf("not executed: this exact call already failed %d times. Repeating it will not help; "+
			"use a different approach, change the arguments based on the error, or report the blocker.", maxIdenticalFailures))
	}

	d := x.Policy.Decide(call.Name, call.Input)
	out.Decision = d
	x.Audit.Log(audit.Event{SessionID: x.SessionID, Kind: "decision", Tool: call.Name, Data: map[string]any{
		"call_id": call.ID, "risk": d.Risk.String(), "action": d.Action.String(), "reason": d.Reason, "forbidden": d.Forbidden}})

	grant := tools.Grant{}
	switch d.Action {
	case policy.Deny:
		return fail(tools.KindDenied, "denied by policy: "+d.Reason+". Do not retry this; choose a safer alternative or report what is blocked.")
	case policy.Ask:
		if d.Key != "" && x.isRemembered(d.Key) {
			break
		}
		req := ApprovalRequest{Tool: call.Name, Summary: Summarize(call), Decision: d}
		if pv, ok := tool.(tools.Previewer); ok {
			if p, err := pv.Preview(ctx, call.Input); err == nil {
				req.Preview = p
			} else {
				return fail(tools.KindOf(err), err.Error())
			}
		}
		resp, err := x.Approver.Approve(ctx, req)
		x.Audit.Log(audit.Event{SessionID: x.SessionID, Kind: "approval", Tool: call.Name, Data: map[string]any{
			"call_id": call.ID, "approved": resp.Approved && err == nil, "remember": resp.Remember, "summary": Summarize(call)}})
		if err != nil {
			return fail(tools.KindCancelled, "approval prompt aborted: "+err.Error())
		}
		if !resp.Approved {
			return fail(tools.KindDenied, "the user declined this action. Do not retry it; propose an alternative or explain what is blocked.")
		}
		out.Approved = true
		if resp.Remember && d.Risk == policy.Medium && d.Key != "" {
			x.remember(d.Key)
		}
	}
	grant.OutsideWorkspace = d.Outside && (out.Approved || d.Action == policy.Allow)
	if d.Action == policy.Allow {
		out.Approved = true
	}

	tctx, cancel := context.WithTimeout(tools.WithGrant(ctx, grant), x.ToolTimeout+5*time.Second)
	defer cancel()
	res, err := safeExecute(tctx, tool, call.Input)
	if err != nil {
		res.Kind = tools.KindOf(err)
		if res.ExitCode == 0 {
			res.ExitCode = 1
		}
		res.Output = strings.TrimSpace(res.Output + "\n" + err.Error())
	}

	if !res.OK() && res.Kind != tools.KindCancelled {
		x.countFailure(fp)
	}
	x.recordEvidence(call, d, &res)
	return finish(res)
}

// ResetTask forgets per-task failure counts. Session-level approvals are kept.
func (x *Executor) ResetTask() {
	x.mu.Lock()
	x.failures = nil
	x.mu.Unlock()
}

func safeExecute(ctx context.Context, t tools.Tool, in json.RawMessage) (res tools.Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = tools.Errorf(tools.KindInternal, "tool %s panicked: %v", t.Name(), r)
		}
	}()
	return t.Execute(ctx, in)
}

// recordEvidence feeds the verification layer from what actually ran.
func (x *Executor) recordEvidence(call Call, d policy.Decision, res *tools.Result) {
	if x.Tracker == nil || res.Kind == tools.KindCancelled {
		return
	}
	purpose := res.Metadata["purpose"]
	isShell := call.Name == "shell_exec" || call.Name == "shell_script"
	if isShell && purpose == "verify" {
		x.Tracker.RecordCheck(Summarize(call), res.ExitCode, res.OK())
		return
	}
	mutated := res.Mutated
	if isShell && purpose != "inspect" && res.Kind != tools.KindUnavailable {
		mutated = mutated || d.Risk == policy.High || (d.Risk == policy.Medium && purpose == "")
	}
	if mutated {
		res.Mutated = true
		x.Tracker.RecordChange(Summarize(call))
	}
}

// render builds the observation text: a status line, then redacted, bounded output.
func (x *Executor) render(res tools.Result) string {
	status := "ok"
	switch {
	case res.Kind == tools.KindFailed && res.ExitCode != 0:
		status = fmt.Sprintf("failed (exit %d)", res.ExitCode)
	case res.Kind != tools.KindNone:
		status = string(res.Kind)
	case res.ExitCode != 0:
		status = fmt.Sprintf("failed (exit %d)", res.ExitCode)
	}
	body := x.Redactor.Redact(res.Output)
	if x.MaxOutput > 0 && len(body) > x.MaxOutput {
		half := x.MaxOutput / 2
		body = body[:half] + fmt.Sprintf("\n... [%d bytes omitted by ScoutClaw] ...\n", len(body)-x.MaxOutput) + body[len(body)-half:]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "status: %s", status)
	if d := res.Metadata["duration"]; d != "" {
		fmt.Fprintf(&b, " | duration: %s", d)
	}
	if res.Truncated {
		b.WriteString(" | output truncated")
	}
	if res.Mutated {
		b.WriteString(" | changed state")
	}
	b.WriteString("\n---\n")
	if strings.TrimSpace(body) == "" {
		b.WriteString("(no output)\n")
	} else {
		b.WriteString(body)
		if !strings.HasSuffix(body, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

func (x *Executor) auditResult(o Outcome) {
	preview := x.Redactor.Redact(o.Result.Output)
	if len(preview) > 600 {
		preview = preview[:600] + "…"
	}
	status := "ok"
	if !o.Result.OK() {
		status = string(o.Result.Kind)
		if status == "" {
			status = "failed"
		}
	}
	x.Audit.Log(audit.Event{SessionID: x.SessionID, Kind: "tool_result", Tool: o.Call.Name, Data: map[string]any{
		"call_id": o.Call.ID, "status": status, "exit_code": o.Result.ExitCode, "duration_ms": o.Duration.Milliseconds(),
		"truncated": o.Result.Truncated, "mutated": o.Result.Mutated, "metadata": o.Result.Metadata, "output_preview": preview}})
}

func (x *Executor) toolNames() string {
	var n []string
	for _, t := range x.Registry.List() {
		n = append(n, t.Name())
	}
	return strings.Join(n, ", ")
}

func (x *Executor) isRemembered(key string) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.allowed[key]
}

func (x *Executor) remember(key string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.allowed == nil {
		x.allowed = map[string]bool{}
	}
	x.allowed[key] = true
}

func (x *Executor) failureCount(fp string) int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.failures[fp]
}

func (x *Executor) countFailure(fp string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.failures == nil {
		x.failures = map[string]int{}
	}
	x.failures[fp]++
}

func fingerprint(c Call) string {
	var v any
	norm := c.Input
	if json.Unmarshal(c.Input, &v) == nil {
		if b, err := json.Marshal(v); err == nil { // canonical key order
			norm = b
		}
	}
	h := sha256.Sum256(append([]byte(c.Name+"\x00"), norm...))
	return hex.EncodeToString(h[:8])
}

// Summarize renders a call as a short human-readable line.
func Summarize(c Call) string {
	var m map[string]any
	_ = json.Unmarshal(c.Input, &m)
	str := func(k string) string { s, _ := m[k].(string); return s }
	switch c.Name {
	case "shell_exec":
		parts := []string{str("command")}
		if a, ok := m["args"].([]any); ok {
			for _, v := range a {
				parts = append(parts, fmt.Sprint(v))
			}
		}
		return strings.Join(parts, " ")
	case "shell_script":
		s := strings.TrimSpace(str("script"))
		if len(s) > 200 {
			s = s[:200] + "…"
		}
		return "sh -c " + fmt.Sprintf("%q", s)
	case "fs_write", "fs_patch", "fs_read", "fs_stat", "fs_list":
		return c.Name + " " + str("path")
	case "fs_search":
		return fmt.Sprintf("fs_search %s %s %s", str("path"), str("pattern"), str("name_glob"))
	case "net_dns":
		return "dns " + str("host")
	case "net_connect":
		return fmt.Sprintf("tcp %s:%v", str("host"), m["port"])
	case "net_http":
		return "http " + str("url")
	}
	return c.Name
}
