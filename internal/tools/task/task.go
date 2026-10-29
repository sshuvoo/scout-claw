// Package task provides the agent's own bookkeeping tools: update_plan keeps a
// visible, evidence-based plan and finish ends the task with a structured
// report that the verification layer checks against recorded evidence.
package task

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"scoutclaw/internal/tools"
	"scoutclaw/internal/verify"
)

type Step struct {
	Step   string `json:"step"`
	Status string `json:"status"`
}

type Report struct {
	Summary    string        `json:"summary"`
	RootCause  string        `json:"root_cause,omitempty"`
	Changes    []string      `json:"changes,omitempty"`
	Status     verify.Status `json:"status"`
	Evidence   string        `json:"evidence,omitempty"`
	Unresolved []string      `json:"unresolved,omitempty"`
	Notes      []string      `json:"notes,omitempty"` // added by the runtime, e.g. downgraded claims
}

// State is shared between the agent and the task tools for one task.
type State struct {
	mu         sync.Mutex
	Tracker    *verify.Tracker
	Plan       []Step
	Hypothesis string
	report     *Report
	attempts   int
	OnPlan     func(plan []Step, hypothesis string)
}

func NewState(t *verify.Tracker) *State { return &State{Tracker: t} }

// Reset starts a new task: clears plan, report and verification evidence.
func (s *State) Reset() {
	s.mu.Lock()
	s.Plan, s.Hypothesis, s.report, s.attempts = nil, "", nil, 0
	s.mu.Unlock()
	s.Tracker.Reset()
}

func (s *State) Report() *Report {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.report
}

func (s *State) CurrentPlan() ([]Step, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Step(nil), s.Plan...), s.Hypothesis
}

type planArgs struct {
	Steps      []Step `json:"steps"`
	Hypothesis string `json:"hypothesis"`
}

type finishArgs struct {
	Summary    string   `json:"summary"`
	RootCause  string   `json:"root_cause"`
	Changes    []string `json:"changes"`
	Status     string   `json:"status"`
	Evidence   string   `json:"evidence"`
	Unresolved []string `json:"unresolved"`
}

func (s *State) Tools() []tools.Tool {
	return []tools.Tool{
		&tools.Typed[planArgs]{
			ToolName: "update_plan",
			Desc:     "Record or revise your short plan and current hypothesis. Call it after the first inspection and whenever new evidence changes the diagnosis. It performs no action on the system.",
			Schema: `{"type":"object","properties":{` +
				`"steps":{"type":"array","items":{"type":"object","properties":{"step":{"type":"string"},"status":{"type":"string","enum":["pending","in_progress","done","blocked"]}},"required":["step","status"],"additionalProperties":false}},` +
				`"hypothesis":{"type":"string","description":"Current best explanation, clearly marked as hypothesis until verified."}},"required":["steps"],"additionalProperties":false}`,
			ValidateFn: func(a *planArgs) error {
				if len(a.Steps) == 0 || len(a.Steps) > 12 {
					return errors.New("steps must contain 1-12 items")
				}
				for _, st := range a.Steps {
					switch st.Status {
					case "pending", "in_progress", "done", "blocked":
					default:
						return fmt.Errorf("invalid step status %q", st.Status)
					}
					if strings.TrimSpace(st.Step) == "" {
						return errors.New("empty step")
					}
				}
				return nil
			},
			Run: func(ctx context.Context, a planArgs) (tools.Result, error) {
				s.mu.Lock()
				s.Plan, s.Hypothesis = a.Steps, a.Hypothesis
				cb := s.OnPlan
				s.mu.Unlock()
				if cb != nil {
					cb(a.Steps, a.Hypothesis)
				}
				return tools.Result{Output: "plan recorded\n"}, nil
			},
		},
		&tools.Typed[finishArgs]{
			ToolName: "finish",
			Desc: "End the task with the final report. status: verified (a purpose=verify command succeeded after your last change), unverified (changes made, not verified), failed (goal not achieved), " +
				"not_needed (no changes were made; the answer is purely observational). The runtime checks this claim against recorded evidence and may downgrade it.",
			Schema: `{"type":"object","properties":{` +
				`"summary":{"type":"string","description":"What was found and done, plainly."},` +
				`"root_cause":{"type":"string","description":"Root cause if identified; say so if it is only a hypothesis."},` +
				`"changes":{"type":"array","items":{"type":"string"},"description":"Every change you made (file/command), including backups."},` +
				`"status":{"type":"string","enum":["verified","unverified","failed","not_needed"]},` +
				`"evidence":{"type":"string","description":"The observation that supports the status."},` +
				`"unresolved":{"type":"array","items":{"type":"string"},"description":"Open issues, blockers, anything needing the user."}},` +
				`"required":["summary","status"],"additionalProperties":false}`,
			ValidateFn: func(a *finishArgs) error {
				if strings.TrimSpace(a.Summary) == "" {
					return errors.New("summary is required")
				}
				if _, ok := verify.Parse(a.Status); !ok {
					return fmt.Errorf("status must be verified, unverified, failed or not_needed, got %q", a.Status)
				}
				return nil
			},
			Run: func(ctx context.Context, a finishArgs) (tools.Result, error) {
				claimed, _ := verify.Parse(a.Status)
				s.mu.Lock()
				defer s.mu.Unlock()
				first := s.attempts == 0
				s.attempts++
				as := s.Tracker.Assess(claimed, first)
				if as.Rejected {
					return tools.Result{Kind: tools.KindFailed, ExitCode: 1,
						Output: "finish rejected: " + as.Note + ". Fix this (run a verification command with purpose=verify, or call finish again with an honest status).\n"}, nil
				}
				r := &Report{Summary: a.Summary, RootCause: a.RootCause, Changes: a.Changes, Status: as.Status, Evidence: a.Evidence, Unresolved: a.Unresolved}
				if as.Evidence != "" {
					r.Evidence = as.Evidence
				}
				if as.Status != claimed {
					r.Notes = append(r.Notes, fmt.Sprintf("status downgraded from %q to %q: %s", claimed, as.Status, as.Note))
				}
				// The runtime's own record of changes is authoritative.
				if rec := s.Tracker.Changes(); len(rec) > 0 {
					r.Notes = append(r.Notes, fmt.Sprintf("%d change(s) recorded by the runtime", len(rec)))
				}
				s.report = r
				return tools.Result{Output: "report accepted\n"}, nil
			},
		},
	}
}
