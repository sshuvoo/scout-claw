package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"scoutclaw/internal/agent"
	"scoutclaw/internal/executor"
	"scoutclaw/internal/policy"
	"scoutclaw/internal/redact"
	"scoutclaw/internal/tools"
	"scoutclaw/internal/tools/task"
	"scoutclaw/internal/verify"
)

const (
	cReset  = "\033[0m"
	cBold   = "\033[1m"
	cDim    = "\033[2m"
	cCyan   = "\033[36m"
	cGreen  = "\033[32m"
	cYellow = "\033[33m"
	cRed    = "\033[31m"
	cGray   = "\033[90m"
)

// UI writes progress and prompts. It implements agent.Reporter and executor.Approver.
type UI struct {
	Out         io.Writer
	Err         io.Writer
	In          *Input
	Color       bool
	Interactive bool // can the user answer approval prompts?
	Redactor    *redact.Redactor
	Verbose     bool
}

func (u *UI) c(code, s string) string {
	if !u.Color {
		return s
	}
	return code + s + cReset
}

func (u *UI) Thought(text string) {
	fmt.Fprintln(u.Out, u.c(cGray, indent(text, "  ")))
}

func (u *UI) Plan(steps []task.Step, hypothesis string) {
	fmt.Fprintln(u.Out, u.c(cCyan, "  plan:"))
	for _, s := range steps {
		mark := map[string]string{"pending": "○", "in_progress": "◐", "done": "●", "blocked": "✖"}[s.Status]
		fmt.Fprintf(u.Out, "    %s %s\n", mark, s.Step)
	}
	if hypothesis != "" {
		fmt.Fprintln(u.Out, u.c(cGray, "    hypothesis: "+hypothesis))
	}
}

func (u *UI) ToolStart(summary string) {
	fmt.Fprintln(u.Out, u.c(cBold, "$ "+u.Redactor.Redact(summary)))
}

func (u *UI) ToolEnd(o executor.Outcome) {
	if o.Call.Name == "update_plan" || o.Call.Name == "finish" && o.OK() {
		return
	}
	r := o.Result
	var badge string
	switch {
	case r.OK():
		badge = u.c(cGreen, "  ✓ ok")
	case r.Kind == tools.KindDenied:
		badge = u.c(cYellow, "  ⛔ denied")
	default:
		badge = u.c(cRed, fmt.Sprintf("  ✗ %s (exit %d)", orStr(string(r.Kind), "failed"), r.ExitCode))
	}
	if o.Decision.Risk != policy.Low && o.Approved {
		badge += u.c(cGray, " ["+o.Decision.Risk.String()+" risk, approved]")
	}
	fmt.Fprintf(u.Out, "%s %s\n", badge, u.c(cGray, o.Duration.Round(time.Millisecond).String()))
	out := strings.TrimRight(u.Redactor.Redact(r.Output), "\n")
	if out == "" {
		return
	}
	lines := strings.Split(out, "\n")
	limit := 8
	if u.Verbose {
		limit = 60
	}
	if len(lines) > limit {
		lines = append(lines[:limit], fmt.Sprintf("… (%d more lines)", len(lines)-limit))
	}
	prev := strings.Join(lines, "\n")
	if len(prev) > 1200 && !u.Verbose {
		prev = prev[:1200] + "…"
	}
	fmt.Fprintln(u.Out, u.c(cGray, indent(prev, "    ")))
}

func (u *UI) Notice(msg string) { fmt.Fprintln(u.Err, u.c(cYellow, "! "+msg)) }

// Approve implements executor.Approver.
func (u *UI) Approve(ctx context.Context, req executor.ApprovalRequest) (executor.ApprovalResponse, error) {
	d := req.Decision
	colour := cYellow
	if d.Risk == policy.High {
		colour = cRed
	}
	fmt.Fprintln(u.Out)
	fmt.Fprintln(u.Out, u.c(colour+cBold, fmt.Sprintf("⚠  Approval needed — %s risk", strings.ToUpper(d.Risk.String()))))
	fmt.Fprintf(u.Out, "   action  : %s\n   why     : %s\n", u.Redactor.Redact(req.Summary), d.Reason)
	if d.Effects != "" {
		fmt.Fprintf(u.Out, "   effects : %s\n", d.Effects)
	}
	if d.Recovery != "" {
		fmt.Fprintf(u.Out, "   recovery: %s\n", d.Recovery)
	}
	if req.Preview != "" {
		fmt.Fprintln(u.Out, "   change  :")
		fmt.Fprintln(u.Out, indent(clipLines(u.Redactor.Redact(req.Preview), 60), "     "))
	}
	if !u.Interactive {
		fmt.Fprintln(u.Out, u.c(cYellow, "   not approved: no interactive terminal. Use --approval auto to pre-approve medium-risk actions (high risk always needs a person)."))
		return executor.ApprovalResponse{}, nil
	}
	opts := "[y]es / [n]o"
	if d.Risk == policy.Medium && d.Key != "" {
		opts += " / [a]lways for this session"
	}
	fmt.Fprintf(u.Out, "   allow? %s: ", opts)
	line, err := u.In.ReadLine(ctx)
	if err != nil {
		return executor.ApprovalResponse{}, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return executor.ApprovalResponse{Approved: true}, nil
	case "a", "always":
		if d.Risk == policy.Medium && d.Key != "" {
			return executor.ApprovalResponse{Approved: true, Remember: true}, nil
		}
		return executor.ApprovalResponse{Approved: true}, nil
	}
	return executor.ApprovalResponse{}, nil
}

// PrintResult renders the final report.
func (u *UI) PrintResult(r agent.Result) {
	fmt.Fprintln(u.Out)
	line := u.c(cGray, strings.Repeat("─", 60))
	fmt.Fprintln(u.Out, line)
	defer func() {
		fmt.Fprintln(u.Out, u.c(cGray, fmt.Sprintf("%d steps · %s · tokens in/out %d/%d",
			r.Steps, r.Elapsed.Round(100*time.Millisecond), r.Usage.InputTokens, r.Usage.OutputTokens)))
	}()

	switch r.Status {
	case agent.StatusCancelled:
		fmt.Fprintln(u.Out, u.c(cYellow+cBold, "■ Cancelled. Any running commands were terminated."))
		return
	case agent.StatusError:
		fmt.Fprintln(u.Out, u.c(cRed+cBold, "✖ Stopped by an error: ")+errText(r.Err))
		return
	case agent.StatusLimit, agent.StatusStuck:
		label := "Stopped: budget exhausted"
		if r.Status == agent.StatusStuck {
			label = "Stopped: repeated failures without progress"
		}
		fmt.Fprintln(u.Out, u.c(cYellow+cBold, "⚠ "+label)+" — "+errText(r.Err))
		if r.Text != "" {
			fmt.Fprintln(u.Out, "\n"+r.Text)
		}
		fmt.Fprintln(u.Out, u.c(cYellow, "Outcome is not verified."))
		return
	}

	rep := r.Report
	if rep == nil {
		fmt.Fprintln(u.Out, u.c(cBold+cGreen, r.Text))
		if r.Verify == verify.Unverified {
			fmt.Fprintln(u.Out, u.c(cYellow, "\n⚠ Changes were made during this task but no verification step succeeded afterwards."))
		}
		return
	}
	switch rep.Status {
	case verify.Verified:
		fmt.Fprintln(u.Out, u.c(cGreen+cBold, "✔ Verified"))
	case verify.Unverified:
		fmt.Fprintln(u.Out, u.c(cYellow+cBold, "⚠ Unverified — changes were made but not confirmed to work"))
	case verify.Failed:
		fmt.Fprintln(u.Out, u.c(cRed+cBold, "✖ Not resolved"))
	default:
		fmt.Fprintln(u.Out, u.c(cCyan+cBold, "ℹ Findings (no changes made)"))
	}
	fmt.Fprintln(u.Out, "\n"+rep.Summary)
	if rep.RootCause != "" {
		fmt.Fprintln(u.Out, u.c(cBold, "\nRoot cause: ")+rep.RootCause)
	}
	section := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintln(u.Out, u.c(cBold, "\n"+title))
		for _, i := range items {
			fmt.Fprintln(u.Out, "  • "+i)
		}
	}
	section("Changes made:", rep.Changes)
	if rep.Evidence != "" {
		fmt.Fprintln(u.Out, u.c(cBold, "\nEvidence: ")+rep.Evidence)
	}
	section("Unresolved:", rep.Unresolved)
	for _, n := range rep.Notes {
		fmt.Fprintln(u.Out, u.c(cGray, "note: "+n))
	}
}

func errText(err error) string {
	if err == nil {
		return "unknown"
	}
	return err.Error()
}

func orStr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

func clipLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[:n], "\n") + fmt.Sprintf("\n… (%d more lines)", len(lines)-n)
}

// Input reads lines from a reader on a single goroutine so that a blocked read
// never prevents cancellation and prompts never race each other.
type Input struct {
	lines chan string
	done  chan struct{}
}

func NewInput(r io.Reader) *Input {
	in := &Input{lines: make(chan string), done: make(chan struct{})}
	go func() {
		defer close(in.done)
		br := bufio.NewReader(r)
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				in.lines <- strings.TrimRight(line, "\r\n")
			}
			if err != nil {
				return
			}
		}
	}()
	return in
}

var errEOF = errors.New("end of input")

func (in *Input) ReadLine(ctx context.Context) (string, error) {
	select {
	case l := <-in.lines:
		return l, nil
	case <-in.done:
		return "", errEOF
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
