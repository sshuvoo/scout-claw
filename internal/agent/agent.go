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
	Plan(steps []task.Step, hypothesis string)
	ToolStart(summary string)
	ToolEnd(o executor.Outcome)
	Notice(msg string)
}

type nopReporter struct{}

func (nopReporter) Delta(string)             {}
func (nopReporter) Plan([]task.Step, string) {}
func (nopReporter) ToolStart(string)         {}
func (nopReporter) ToolEnd(executor.Outcome) {}
func (nopReporter) Notice(string)            {}

type Limits struct {
	MaxSteps            int
	TaskTimeout         time.Duration
	MaxConsecutiveFails int // consecutive failed tool calls before giving up
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
		"tokens_in": res.Usage.InputTokens, "tokens_out": res.Usage.OutputTokens}})
	return res
}

func (a *Agent) loop(ctx context.Context, prompt string, rep Reporter) (res Result) {
	user := prompt
	if a.Preamble != nil && a.PreviousID == "" {
		user = a.Preamble(prompt)
	}
	req := llm.Request{
		Instructions: SystemPrompt,
		UserText:     user,
		ToolOutputs:  a.Carry,
		Tools:        a.specs(),
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
	for step := 1; step <= a.Limits.MaxSteps; step++ {
		res.Steps = step
		resp, err := a.LLM.Generate(ctx, req)
		if err != nil {
			return a.failed(ctx, res, err)
		}
		a.PreviousID = resp.ID
		pending = nil
		res.Usage.InputTokens += resp.Usage.InputTokens
		res.Usage.OutputTokens += resp.Usage.OutputTokens

		if len(resp.ToolCalls) == 0 {
			switch {
			case resp.Incomplete != "":
				rep.Notice("model stopped early (" + resp.Incomplete + "); asking it to continue")
				req = llm.Request{Instructions: SystemPrompt, UserText: "Your last response was cut off. Continue, concisely: call a tool or finish.", Tools: a.specs(), PreviousID: a.PreviousID, OnDelta: rep.Delta}
				continue
			case strings.TrimSpace(resp.Text) == "":
				empty++
				if empty > 2 {
					res.Status, res.Err = StatusError, errors.New("the model returned empty responses repeatedly")
					return res
				}
				req = llm.Request{Instructions: SystemPrompt, UserText: "Your last response was empty. Call a tool to continue, or call finish.", Tools: a.specs(), PreviousID: a.PreviousID, OnDelta: rep.Delta}
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

		if strings.TrimSpace(resp.Text) != "" {
			rep.Delta("\n") // the thought itself already streamed in via Delta
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
		req = llm.Request{Instructions: SystemPrompt, ToolOutputs: outputs, Tools: a.specs(), PreviousID: a.PreviousID, OnDelta: rep.Delta}
		if consecFail >= a.Limits.MaxConsecutiveFails {
			res.Status = StatusStuck
			res = a.wrapUp(ctx, res, &req, fmt.Sprintf("%d tool calls in a row failed", consecFail))
			if res.Text != "" {
				pending = nil // delivered with the wrap-up request
			}
			return res
		}
	}
	res.Status = StatusLimit
	res = a.wrapUp(ctx, res, &req, fmt.Sprintf("the step limit (%d) was reached", a.Limits.MaxSteps))
	if res.Text != "" {
		pending = nil
	}
	return res
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
func (a *Agent) wrapUp(ctx context.Context, res Result, req *llm.Request, why string) Result {
	if ctx.Err() != nil {
		return a.ended(ctx, res)
	}
	req.Tools = nil
	req.UserText = "STOP: " + why + ". Do not call any tools. Write a brief honest status report: what you found, what you changed, what is verified versus only suspected, and what remains."
	resp, err := a.LLM.Generate(ctx, *req)
	if err != nil {
		res.Err = fmt.Errorf("%s; wrap-up request also failed: %w", why, err)
		return res
	}
	a.PreviousID = resp.ID
	res.Usage.InputTokens += resp.Usage.InputTokens
	res.Usage.OutputTokens += resp.Usage.OutputTokens
	res.Text = resp.Text
	res.Err = errors.New(why)
	as := a.State.Tracker.Assess(verify.Unverified, false)
	res.Verify = as.Status
	return res
}

func (a *Agent) specs() []llm.ToolSpec {
	var out []llm.ToolSpec
	for _, t := range a.Registry.List() {
		out = append(out, llm.ToolSpec{Name: t.Name(), Description: t.Description(), Schema: t.InputSchema()})
	}
	return out
}
