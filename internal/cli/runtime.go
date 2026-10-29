package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"scoutclaw/internal/agent"
	"scoutclaw/internal/audit"
	"scoutclaw/internal/config"
	"scoutclaw/internal/executor"
	"scoutclaw/internal/llm"
	"scoutclaw/internal/platform"
	"scoutclaw/internal/policy"
	"scoutclaw/internal/redact"
	"scoutclaw/internal/session"
	"scoutclaw/internal/tools/builtin"
	"scoutclaw/internal/verify"
)

// providerFactory is replaced in tests so no live API is needed.
var providerFactory = func(cfg *config.Config, ui *UI) (llm.Provider, error) {
	if err := cfg.RequireAPIKey(); err != nil {
		return nil, err
	}
	p, err := llm.NewOpenAI(cfg.APIKey, cfg.BaseURL, cfg.Model)
	if err != nil {
		return nil, err
	}
	pol := llm.DefaultRetry()
	pol.OnRetry = func(attempt int, wait time.Duration, err error) {
		ui.Notice(fmt.Sprintf("provider error (%v); retry %d in %s", err, attempt, wait.Round(time.Millisecond)))
	}
	return llm.WithRetry(p, pol), nil
}

type Runtime struct {
	Cfg     *config.Config
	UI      *UI
	Agent   *agent.Agent
	Session *session.Session
	audit   *audit.Logger
}

func newRedactor(cfg *config.Config) *redact.Redactor {
	r := redact.New(cfg.APIKey)
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if redact.LooksLikeSecretValue(k, v) {
			r.AddSecret(v)
		}
	}
	return r
}

func newRuntime(cfg *config.Config, ui *UI, sess *session.Session) (*Runtime, error) {
	provider, err := providerFactory(cfg, ui)
	if err != nil {
		return nil, err
	}
	red := newRedactor(cfg)
	ui.Redactor = red
	lg, err := audit.Open(cfg.AuditPath(), red)
	if err != nil {
		// Auditing is a safety feature: do not run actions we cannot record.
		return nil, fmt.Errorf("cannot open audit log %s: %w", cfg.AuditPath(), err)
	}
	resumed := sess != nil
	if sess == nil {
		sess = session.New(cfg.SessionsDir(), cfg.Workspace, cfg.Model)
	}
	scope := policy.NewScope(cfg.Workspace)
	tracker := &verify.Tracker{}
	reg, state := builtin.New(builtin.Options{
		Scope: scope, BackupDir: cfg.BackupsDir(), SessionID: sess.ID, MaxOutput: cfg.MaxOutput,
		ToolTimeout: cfg.ToolTimeout, StripEnv: []string{config.EnvAPIKey}, Tracker: tracker,
	})
	exec := &executor.Executor{
		Registry: reg, Policy: policy.New(scope, cfg.Approval), Approver: ui, Audit: lg, Redactor: red,
		Tracker: tracker, SessionID: sess.ID, ToolTimeout: cfg.ToolTimeout, MaxOutput: cfg.MaxOutput,
	}
	plat := platform.Detect()
	ag := &agent.Agent{
		LLM: provider, Exec: exec, Registry: reg, State: state, Audit: lg, Reporter: ui, SessionID: sess.ID,
		Limits:   agent.Limits{MaxSteps: cfg.MaxSteps, TaskTimeout: cfg.TaskTimeout},
		Preamble: func(prompt string) string { return agent.TaskContext(cfg, plat, prompt) },
	}
	if resumed {
		ag.PreviousID = sess.ResponseID
		for _, p := range sess.Pending {
			ag.Carry = append(ag.Carry, llm.ToolOutput{CallID: p.CallID, Output: p.Output})
		}
		if sess.Model != cfg.Model {
			ui.Notice(fmt.Sprintf("session was created with model %s; continuing with %s", sess.Model, cfg.Model))
		}
	}
	return &Runtime{Cfg: cfg, UI: ui, Agent: ag, Session: sess, audit: lg}, nil
}

func (r *Runtime) Close() { _ = r.audit.Close() }

// RunTask runs one task. Ctrl-C (SIGINT/SIGTERM) cancels it and terminates any
// child processes; a second Ctrl-C exits immediately.
func (r *Runtime) RunTask(ctx context.Context, prompt string) agent.Result {
	cctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	go func() { <-cctx.Done(); stop() }()
	defer stop()

	started := time.Now().UTC()
	res := r.Agent.Run(cctx, prompt)
	r.UI.PrintResult(res)
	r.record(prompt, started, res)
	return res
}

func (r *Runtime) record(prompt string, started time.Time, res agent.Result) {
	rec := session.TaskRecord{
		Prompt: prompt, Status: string(res.Status), Verified: string(res.Verify), Started: started,
		Finished: time.Now().UTC(), Steps: res.Steps, TokensIn: res.Usage.InputTokens, TokensOut: res.Usage.OutputTokens,
	}
	switch {
	case res.Report != nil:
		rec.Summary, rec.Changes = r.UI.Redactor.Redact(res.Report.Summary), res.Report.Changes
	default:
		rec.Summary = r.UI.Redactor.Redact(firstLine(res.Text))
	}
	r.Session.Tasks = append(r.Session.Tasks, rec)
	r.Session.ResponseID = r.Agent.PreviousID
	r.Session.Pending = nil
	for _, c := range r.Agent.Carry {
		r.Session.Pending = append(r.Session.Pending, session.Pending{CallID: c.CallID, Output: c.Output})
	}
	if err := r.Session.Save(); err != nil {
		r.UI.Notice("could not save session: " + err.Error())
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
