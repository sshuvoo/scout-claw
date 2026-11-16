// Package agent runs the investigate -> act -> observe -> verify loop. It
// never touches the system itself: every action is a tool call that goes
// through the executor (validation, policy, approval, limits, audit).
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"scoutclaw/internal/audit"
	"scoutclaw/internal/executor"
	"scoutclaw/internal/llm"
	"scoutclaw/internal/tools"
	"scoutclaw/internal/tools/task"
	"scoutclaw/internal/verify"
)

type Status string

const (
	StatusCompleted Status = "completed"
	StatusCancelled Status = "cancelled"
	StatusLimit     Status = "limit" // step or time budget exhausted
	StatusStuck     Status = "stuck" // repeated failures with no progress
	StatusError     Status = "error" // provider or runtime failure
)

// Reporter receives progress for display. All methods must be cheap and non-blocking.
type Reporter interface {
	Delta(text string) // a chunk of the model's output text, as it streams in
	EndTurn()          // the model's output text for the current turn is complete
	Plan(steps []task.Step, hypothesis string)
	ToolStart(summary string)
	ToolEnd(o executor.Outcome)
	Notice(msg string)
}

type nopReporter struct{}

func (nopReporter) Delta(string)             {}
func (nopReporter) EndTurn()                 {}
func (nopReporter) Plan([]task.Step, string) {}
func (nopReporter) ToolStart(string)         {}
func (nopReporter) ToolEnd(executor.Outcome) {}
func (nopReporter) Notice(string)            {}

type Limits struct {
	MaxSteps            int
	TaskTimeout         time.Duration
	MaxConsecutiveFails int   // consecutive failed tool calls before giving up
	CompactAt           int64 // context tokens at which the provider compacts the conversation (0: off)
}

type Agent struct {
	LLM      llm.Provider
	Exec     *executor.Executor
	Registry *tools.Registry
	State    *task.State
	Limits   Limits
	Audit    *audit.Logger
	Reporter Reporter

	SessionID  string
	Preamble   func(prompt string) string // builds the first user message of a session
	PreviousID string                     // provider conversation pointer (resumable)
	Carry      []llm.ToolOutput           // outputs the provider still expects from the last task

	// ContextTokens is the input size of the latest model turn: roughly how
	// much conversation the model is carrying.
	ContextTokens int64
}

type Result struct {
	Status  Status
	Report  *task.Report // set when the model called finish
	Text    string       // free-text answer or wrap-up when there is no report
	Verify  verify.Status
	Steps   int
	Usage   llm.Usage
	Elapsed time.Duration
	Err     error
}

func (a *Agent) Run(ctx context.Context, prompt string) Result {
	start := time.Now()
	// Keep provider history for tool-loop continuations, but do not replay an
	// entire completed task chain for each new user request. Pending tool
	// outputs are the exception: they must be delivered on their original chain.
	if len(a.Carry) == 0 {
		a.PreviousID = ""
	}
	a.ContextTokens = 0
	rep := a.Reporter
	if rep == nil {
		rep = nopReporter{}
	}
	if a.Limits.MaxConsecutiveFails <= 0 {
		a.Limits.MaxConsecutiveFails = 6
	}
	a.State.Reset()
	a.Exec.ResetTask()
	a.State.OnPlan = rep.Plan
	if a.Limits.TaskTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.Limits.TaskTimeout)
		defer cancel()
	}
	a.Audit.Log(audit.Event{SessionID: a.SessionID, Kind: "task_start", Data: map[string]any{"prompt": prompt}})

	res := a.loop(ctx, prompt, rep)
	res.Elapsed = time.Since(start)
	if rep := a.State.Report(); rep != nil {
		res.Report, res.Verify = rep, rep.Status
	}
	a.Audit.Log(audit.Event{SessionID: a.SessionID, Kind: "task_end", Data: map[string]any{
		"status": res.Status, "verification": res.Verify, "steps": res.Steps, "elapsed_ms": res.Elapsed.Milliseconds(),
		"tokens_in": res.Usage.InputTokens, "tokens_in_cached": res.Usage.CachedInputTokens, "tokens_out": res.Usage.OutputTokens}})
	return res
}

func (a *Agent) loop(ctx context.Context, prompt string, rep Reporter) (res Result) {
	user := prompt
	if a.Preamble != nil && a.PreviousID == "" {
		user = a.Preamble(prompt)
	}
	instructions := SystemPrompt
	var toolSpecs []llm.ToolSpec
	if len(a.Carry) == 0 && isSmallTalk(prompt) {
		instructions = SmallTalkSystemPrompt
		user = prompt
	} else {
		toolSpecs = a.specsFor(prompt)
	}
	req := llm.Request{
		Instructions: instructions,
		UserText:     user,
		ToolOutputs:  a.Carry,
		Tools:        toolSpecs,
		PreviousID:   a.PreviousID,
		OnDelta:      rep.Delta,
	}
	// pending holds outputs the provider still expects; whatever is undelivered
	// when the task ends is carried into the next task of this session.
	pending := a.Carry
	a.Carry = nil
	defer func() {
		if len(pending) > 0 {
			a.Carry = pending
		}
	}()

	consecFail, empty := 0, 0
	compacted := false      // remind the model of the task on its next turn
	compactionSeen := false // the server reports a compaction on every turn past the threshold; restate once per task
	remind := func(text string) string {
		if !compacted {
			return text
		}
		compacted = false
		return strings.TrimSpace(a.reminder(prompt) + "\n\n" + text)
	}
	for step := 1; step <= a.Limits.MaxSteps; step++ {
		res.Steps = step
		req.CompactAt = a.Limits.CompactAt
		resp, err := a.LLM.Generate(ctx, req)
		if err != nil {
			return a.failed(ctx, res, err)
		}
		a.PreviousID = resp.ID
		a.ContextTokens = resp.Usage.InputTokens
		pending = nil
		res.Usage.InputTokens += resp.Usage.InputTokens
		res.Usage.CachedInputTokens += resp.Usage.CachedInputTokens
		res.Usage.OutputTokens += resp.Usage.OutputTokens
		rep.EndTurn()
		if resp.Compacted {
			a.Audit.Log(audit.Event{SessionID: a.SessionID, Kind: "compaction", Data: map[string]any{"context_tokens": resp.Usage.InputTokens}})
			if !compactionSeen {
				compactionSeen, compacted = true, true
				rep.Notice(fmt.Sprintf("conversation compacted (context was %d tokens); the task and plan are restated to the model", resp.Usage.InputTokens))
			}
		}

		if len(resp.ToolCalls) == 0 {
			switch {
			case resp.Incomplete != "":
				rep.Notice("model stopped early (" + resp.Incomplete + "); asking it to continue")
				req = llm.Request{Instructions: instructions, UserText: remind("Your last response was cut off. Continue, concisely: call a tool or finish."), Tools: toolSpecs, PreviousID: a.PreviousID, OnDelta: rep.Delta}
				continue
			case strings.TrimSpace(resp.Text) == "":
				empty++
				if empty > 2 {
					res.Status, res.Err = StatusError, errors.New("the model returned empty responses repeatedly")
					return res
				}
				req = llm.Request{Instructions: instructions, UserText: remind("Your last response was empty. Call a tool to continue, or call finish."), Tools: toolSpecs, PreviousID: a.PreviousID, OnDelta: rep.Delta}
				continue
			}
			// A plain-text answer ends the task. Its verification status is judged
			// from evidence, never from the model's wording.
			claimed := verify.NotNeeded
			if len(a.State.Tracker.Changes()) > 0 {
				claimed = verify.Unverified
			}
			as := a.State.Tracker.Assess(claimed, false)
			res.Status, res.Text, res.Verify = StatusCompleted, resp.Text, as.Status
			return res
		}

		var outputs []llm.ToolOutput
		pending = unanswered(resp.ToolCalls, outputs)
		for _, tc := range resp.ToolCalls {
			if ctx.Err() != nil {
				pending = unanswered(resp.ToolCalls, outputs)
				return a.ended(ctx, res)
			}
			call := executor.Call{ID: tc.ID, Name: tc.Name, Input: json.RawMessage(tc.Arguments)}
			rep.ToolStart(executor.Summarize(call))
			out := a.Exec.Run(ctx, call)
			rep.ToolEnd(out)
			outputs = append(outputs, llm.ToolOutput{CallID: tc.ID, Output: out.Text})
			pending = unanswered(resp.ToolCalls, outputs)

			switch tc.Name {
			case "update_plan", "finish":
			default:
				if out.OK() {
					consecFail = 0
				} else if out.Result.Kind != tools.KindCancelled {
					consecFail++
				}
			}
		}
		pending = outputs

		if a.State.Report() != nil {
			res.Status = StatusCompleted
			return res
		}
		if ctx.Err() != nil {
			return a.ended(ctx, res)
		}
		req = llm.Request{Instructions: instructions, UserText: remind(""), ToolOutputs: outputs, Tools: toolSpecs, PreviousID: a.PreviousID, OnDelta: rep.Delta}
		if consecFail >= a.Limits.MaxConsecutiveFails {
			res.Status = StatusStuck
			res = a.wrapUp(ctx, res, &req, fmt.Sprintf("%d tool calls in a row failed", consecFail), rep)
			if res.Text != "" {
				pending = nil // delivered with the wrap-up request
			}
			return res
		}
	}
	res.Status = StatusLimit
	res = a.wrapUp(ctx, res, &req, fmt.Sprintf("the step limit (%d) was reached", a.Limits.MaxSteps), rep)
	if res.Text != "" {
		pending = nil
	}
	return res
}

func isSmallTalk(prompt string) bool {
	switch strings.ToLower(strings.TrimSpace(prompt)) {
	case "hi", "hi!", "hello", "hello!", "hey", "hey!", "howdy", "good morning", "good afternoon", "good evening",
		"who are you", "who are you?", "who made you", "who made you?", "what can you do", "what can you do?", "thanks", "thank you":
		return true
	default:
		return false
	}
}

// unanswered returns placeholder outputs for calls without a result yet, so the
// provider conversation stays consistent if the task ends early.
func unanswered(calls []llm.ToolCall, done []llm.ToolOutput) []llm.ToolOutput {
	have := map[string]bool{}
	out := append([]llm.ToolOutput(nil), done...)
	for _, d := range done {
		have[d.CallID] = true
	}
	for _, c := range calls {
		if !have[c.ID] {
			out = append(out, llm.ToolOutput{CallID: c.ID, Output: "status: cancelled\n---\nnot executed: the task ended before this call ran\n"})
		}
	}
	return out
}

func (a *Agent) ended(ctx context.Context, res Result) Result {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		res.Status, res.Err = StatusLimit, fmt.Errorf("task timeout (%s) reached", a.Limits.TaskTimeout)
	} else {
		res.Status = StatusCancelled
	}
	return res
}

func (a *Agent) failed(ctx context.Context, res Result, err error) Result {
	if ctx.Err() != nil {
		return a.ended(ctx, res)
	}
	res.Status, res.Err = StatusError, err
	return res
}

// wrapUp asks the model, without tools, for an honest status report after a
// budget was exhausted. Failure of this call is not fatal.
func (a *Agent) wrapUp(ctx context.Context, res Result, req *llm.Request, why string, rep Reporter) Result {
	if ctx.Err() != nil {
		return a.ended(ctx, res)
	}
	req.Tools = nil
	req.CompactAt = a.Limits.CompactAt
	req.UserText = "STOP: " + why + ". Do not call any tools. Write a brief honest status report: what you found, what you changed, what is verified versus only suspected, and what remains."
	resp, err := a.LLM.Generate(ctx, *req)
	if err != nil {
		res.Err = fmt.Errorf("%s; wrap-up request also failed: %w", why, err)
		return res
	}
	rep.EndTurn()
	a.PreviousID = resp.ID
	res.Usage.InputTokens += resp.Usage.InputTokens
	res.Usage.CachedInputTokens += resp.Usage.CachedInputTokens
	res.Usage.OutputTokens += resp.Usage.OutputTokens
	res.Text = resp.Text
	res.Err = errors.New(why)
	as := a.State.Tracker.Assess(verify.Unverified, false)
	res.Verify = as.Status
	return res
}

// reminder restates what the compacted conversation must not lose.
func (a *Agent) reminder(prompt string) string {
	var b strings.Builder
	b.WriteString("[ScoutClaw] Earlier turns of this conversation were compacted to save context. Keep working on the current request:\n")
	b.WriteString(prompt)
	if plan, hyp := a.State.CurrentPlan(); len(plan) > 0 {
		b.WriteString("\nCurrent plan:")
		for _, s := range plan {
			fmt.Fprintf(&b, "\n- [%s] %s", s.Status, s.Step)
		}
		if hyp != "" {
			b.WriteString("\nHypothesis: " + hyp)
		}
	}
	b.WriteString("\nUse fs_changes to see the files you changed so far, and read files again before editing them if you no longer have their exact contents.")
	return b.String()
}

func (a *Agent) specs() []llm.ToolSpec {
	var out []llm.ToolSpec
	for _, t := range a.Registry.List() {
		out = append(out, llm.ToolSpec{Name: t.Name(), Description: t.Description(), Schema: t.InputSchema()})
	}
	return out
}

// specsFor sends only the tool families suggested by clearly scoped requests.
// Unclassified requests retain the full registry so the router cannot hide a
// capability the model may need.
func (a *Agent) specsFor(prompt string) []llm.ToolSpec {
	p := strings.ToLower(prompt)
	files := hasAny(p, "file", "folder", "directory", "workspace", "project", "repository", "repo", "code", "source", "edit", "write", "read", "patch", "create", "delete", "rename", "move", "copy", "build", "test", "implement", "refactor", "bug", "fix", "documentation", "docs", ".go", ".py", ".js", ".ts", ".json", ".yaml", ".md")
	network := hasAny(p, "network", "dns", "port", "url", "http", "internet", "connectivity", "firewall", "ip address", "socket")
	process := hasAny(p, "process", "service", "daemon", "background job", "background process", "server", "pid")
	system := hasAny(p, "system info", "system information", "cpu", "memory", "disk", "storage", "uptime", "kernel", "environment variable", "env var", "hostname", "health check")
	runtime := hasAny(p, "runtime", "compiler", "toolchain", "package manager", "go version", "node version", "python version", "java version", "install package")
	if !files && !network && !process && !system && !runtime {
		return a.specs()
	}

	selected := map[string]bool{"finish": true, "update_plan": true}
	for _, spec := range a.specs() {
		switch {
		case strings.HasPrefix(spec.Name, "shell_"):
			selected[spec.Name] = true
		case files && (strings.HasPrefix(spec.Name, "fs_") || spec.Name == "project_info"):
			selected[spec.Name] = true
		case network && strings.HasPrefix(spec.Name, "net_"):
			selected[spec.Name] = true
		case process && (strings.HasPrefix(spec.Name, "proc_") || spec.Name == "process_list"):
			selected[spec.Name] = true
		case system && (spec.Name == "sys_info" || spec.Name == "process_list" || spec.Name == "env_list"):
			selected[spec.Name] = true
		case runtime && spec.Name == "dev_runtimes":
			selected[spec.Name] = true
		}
	}
	all := a.specs()
	out := make([]llm.ToolSpec, 0, len(selected))
	for _, spec := range all {
		if selected[spec.Name] {
			out = append(out, spec)
		}
	}
	return out
}

func hasAny(text string, terms ...string) bool {
	for _, term := range terms {
		if strings.Contains(text, term) {
			return true
		}
	}
	return false
}
