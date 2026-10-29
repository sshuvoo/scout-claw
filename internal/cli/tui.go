package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"

	"scoutclaw/internal/agent"
	"scoutclaw/internal/config"
	"scoutclaw/internal/executor"
	"scoutclaw/internal/policy"
	"scoutclaw/internal/session"
)

// useTUI reports whether the full-screen chat can run: both ends must be a
// real terminal, and the user must not have asked for the plain mode.
func (a *App) useTUI() bool {
	return !a.fl.plain && a.stdinT && a.Out == os.Stdout && isTerminal(os.Stdout) && os.Getenv("TERM") != "dumb"
}

// ---- messages ---------------------------------------------------------------

type (
	outMsg     string
	statusMsg  string
	rtReadyMsg struct {
		rt  *Runtime
		err error
	}
	taskDoneMsg struct{ res agent.Result }
	slashDone   struct {
		out   string
		quit  bool
		newRT *Runtime
	}
	approvalMsg struct {
		req   executor.ApprovalRequest
		reply chan executor.ApprovalResponse
	}
	clearQuitMsg struct{}
)

type tuiWriter struct{ p *tea.Program }

func (w *tuiWriter) Write(b []byte) (int, error) {
	if w.p != nil {
		w.p.Send(outMsg(string(b)))
	}
	return len(b), nil
}

// ---- styles -----------------------------------------------------------------

// Fixed mid-tone colours that read on both dark and light themes.
var (
	accent   = lipgloss.Color("#8B7BFF")
	subtle   = lipgloss.Color("244")
	warnCol  = lipgloss.Color("#E0A030")
	errCol   = lipgloss.Color("#E5584F")
	okCol    = lipgloss.Color("#3DB468")
	headerSt = lipgloss.NewStyle().Bold(true).Foreground(accent)
	dimSt    = lipgloss.NewStyle().Foreground(subtle)
	userSt   = lipgloss.NewStyle().Bold(true).Foreground(accent)
)

type tuiMode int

const (
	modeChat tuiMode = iota
	modeKey
)

type pendingApproval struct {
	req   executor.ApprovalRequest
	reply chan executor.ApprovalResponse
}

type tuiModel struct {
	app *App
	cfg *config.Config
	ui  *UI
	rt  *Runtime
	ctx context.Context

	vp   viewport.Model
	ta   textarea.Model
	ki   textinput.Model
	spin spinner.Model

	transcript string // raw text with ANSI codes, wrapped at render time
	w, h       int
	mode       tuiMode

	running   bool
	started   time.Time
	cancel    context.CancelFunc
	activity  string
	approval  *pendingApproval
	quitArmed bool

	history  []string
	histIdx  int // len(history) means "current draft"
	draft    string
	sugIdx   int
	tokensIn int64
	tokensOu int64
	firstKey bool   // no API key at start
	hadTask  bool   // a task has been started: leave the landing screen for good
	note     string // message shown on the landing screen (e.g. first-run key prompt)
	build    func() tea.Msg
}

func (a *App) runTUI(cmd *cobra.Command, cfg *config.Config, sess *session.Session) error {
	// No Input here: a goroutine reading stdin would steal keystrokes from Bubble Tea.
	w := &tuiWriter{}
	ui := &UI{Out: w, Err: w, Color: true, Interactive: true, Verbose: a.fl.verbose}

	m := newTUIModel(a, cmd.Context(), cfg, ui)
	m.firstKey = cfg.APIKey == ""
	prog := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithContext(cmd.Context()))
	w.p = prog

	ui.approveFn = func(ctx context.Context, req executor.ApprovalRequest) (executor.ApprovalResponse, error) {
		reply := make(chan executor.ApprovalResponse, 1)
		prog.Send(approvalMsg{req: req, reply: reply})
		select {
		case r := <-reply:
			return r, nil
		case <-ctx.Done():
			return executor.ApprovalResponse{}, ctx.Err()
		}
	}
	ui.onStatus = func(s string) { prog.Send(statusMsg(s)) }
	m.build = func() tea.Msg {
		rt, err := newRuntime(cfg, ui, sess)
		return rtReadyMsg{rt, err}
	}

	final, err := prog.Run()
	fm, _ := final.(*tuiModel)
	if fm != nil && fm.rt != nil {
		fm.rt.syncSession()
		_ = fm.rt.Session.Save()
		fmt.Fprintf(a.Out, "Session %s saved. Resume with: scout --resume %s\n", fm.rt.Session.ID, fm.rt.Session.ID)
		fm.rt.Close()
	}
	return err
}

func newTUIModel(a *App, ctx context.Context, cfg *config.Config, ui *UI) *tuiModel {
	ta := textarea.New()
	ta.Prompt = "❯ "
	ta.Placeholder = "Describe a problem, or type / for commands…"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.SetHeight(1)
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.FocusedStyle.Prompt = lipgloss.NewStyle().Foreground(accent).Bold(true)
	ta.BlurredStyle.Prompt = dimSt
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("ctrl+j", "alt+enter"))
	ta.Focus()

	ki := textinput.New()
	ki.Prompt = "API key ❯ "
	ki.EchoMode = textinput.EchoPassword
	ki.EchoCharacter = '•'
	ki.Placeholder = "paste your OpenAI API key, then Enter"

	sp := spinner.New(spinner.WithSpinner(spinner.Dot))
	sp.Style = lipgloss.NewStyle().Foreground(accent)

	m := &tuiModel{app: a, cfg: cfg, ui: ui, ctx: ctx, ta: ta, ki: ki, spin: sp}
	m.vp = viewport.New(0, 0)
	m.histIdx = 0
	return m
}

// build is set by runTUI; kept as a field so tests can stub it.
func (m *tuiModel) buildCmd() tea.Cmd { return m.build }

// ---- tea.Model --------------------------------------------------------------

func (m *tuiModel) Init() tea.Cmd {
	if m.firstKey {
		m.enterKeyMode("No OpenAI API key found. Paste it below (input is masked, saved to ~/.scoutclaw.env with mode 600).")
		return textarea.Blink
	}
	return tea.Batch(textarea.Blink, m.buildCmd())
}

// landing reports whether to show the centered start screen: nothing has been
// said yet and no task or approval is in progress.
func (m *tuiModel) landing() bool {
	return !m.hadTask && m.transcript == "" && !m.running && m.approval == nil
}

func (m *tuiModel) boxW() int {
	if m.landing() {
		return min(m.w, 84)
	}
	return m.w
}

func (m *tuiModel) landingView() string {
	bw := m.boxW()
	fit := func(t string) string { return ansi.Truncate(t, m.w, "…") }
	var parts []string
	if m.w >= 56 && m.h >= 22 { // the banner needs room; Render pads the art into a rectangle so it centers as one block
		parts = append(parts, headerSt.Render(strings.TrimLeft(banner, "\n")), "")
	} else {
		parts = append(parts, headerSt.Render("ScoutClaw"))
	}
	parts = append(parts,
		fit(dimSt.Render("Autonomous on-device diagnostic agent")),
		fit(dimSt.Render("Describe a problem in plain language. Risky actions always ask first.")),
		"",
		m.inputBox(),
	)
	if m.note != "" {
		parts = append(parts, lipgloss.NewStyle().Foreground(warnCol).Width(min(bw, m.w)).Render(m.note))
	}
	if pop := m.popup(); pop != "" {
		parts = append(parts, lipgloss.NewStyle().Width(bw).Render(pop))
	} else {
		parts = append(parts, fit(dimSt.Render("Enter send · Ctrl+J newline · / commands · Ctrl+C quit")))
	}
	block := lipgloss.JoinVertical(lipgloss.Center, parts...)
	return lipgloss.Place(m.w, m.h-1, lipgloss.Center, lipgloss.Center, block) + "\n" + m.statusBar()
}

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.layout()
		m.refresh(false)
		return m, nil

	case outMsg:
		m.appendRaw(string(msg))
		return m, nil

	case statusMsg:
		m.activity = string(msg)
		return m, nil

	case rtReadyMsg:
		if msg.err != nil {
			m.appendRaw(m.ui.c(cRed, "✖ "+msg.err.Error()) + "\n")
			if strings.Contains(msg.err.Error(), "OPENAI_API_KEY") {
				m.enterKeyMode("")
			}
			return m, nil
		}
		m.rt = msg.rt
		return m, nil

	case approvalMsg:
		m.approval = &pendingApproval{req: msg.req, reply: msg.reply}
		m.layout()
		return m, nil

	case taskDoneMsg:
		m.running, m.activity, m.cancel = false, "", nil
		m.tokensIn += msg.res.Usage.InputTokens
		m.tokensOu += msg.res.Usage.OutputTokens
		m.approval = nil
		m.layout()
		m.refresh(false)
		return m, nil

	case slashDone:
		if msg.out != "" {
			m.appendRaw(msg.out)
		}
		if msg.newRT != nil {
			m.rt = msg.newRT
		}
		if msg.quit {
			return m, tea.Quit
		}
		return m, nil

	case clearQuitMsg:
		m.quitArmed = false
		return m, nil

	case spinner.TickMsg:
		if m.running {
			var c tea.Cmd
			m.spin, c = m.spin.Update(msg)
			return m, c
		}
		return m, nil

	case tea.MouseMsg:
		var c tea.Cmd
		m.vp, c = m.vp.Update(msg)
		return m, c

	case tea.KeyMsg:
		return m.onKey(msg)
	}

	// default: forward to the focused input (cursor blink etc.)
	var c tea.Cmd
	if m.mode == modeKey {
		m.ki, c = m.ki.Update(msg)
	} else {
		m.ta, c = m.ta.Update(msg)
	}
	cmds = append(cmds, c)
	return m, tea.Batch(cmds...)
}

func (m *tuiModel) onKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()

	if m.approval != nil { // approval panel owns the keyboard
		switch strings.ToLower(k) {
		case "y", "enter":
			m.answer(executor.ApprovalResponse{Approved: true}, "approved")
		case "a":
			d := m.approval.req.Decision
			m.answer(executor.ApprovalResponse{Approved: true, Remember: d.Risk == policy.Medium && d.Key != ""}, "approved")
		case "n", "esc":
			m.answer(executor.ApprovalResponse{}, "declined")
		case "ctrl+c":
			m.answer(executor.ApprovalResponse{}, "declined")
			if m.cancel != nil {
				m.cancel()
			}
		}
		return m, nil
	}

	if m.mode == modeKey {
		switch k {
		case "enter":
			return m, m.saveKey(strings.TrimSpace(m.ki.Value()))
		case "esc", "ctrl+c":
			m.mode = modeChat
			m.ki.SetValue("")
			m.layout()
			m.appendRaw(dimSt.Render("key entry cancelled") + "\n")
			return m, nil
		}
		var c tea.Cmd
		m.ki, c = m.ki.Update(msg)
		return m, c
	}

	switch k {
	case "ctrl+c":
		if m.running && m.cancel != nil {
			m.cancel()
			m.appendRaw(m.ui.c(cYellow, "! cancelling… running commands are being terminated") + "\n")
			return m, nil
		}
		if m.quitArmed {
			return m, tea.Quit
		}
		m.quitArmed = true
		return m, tea.Tick(2*time.Second, func(time.Time) tea.Msg { return clearQuitMsg{} })
	case "ctrl+d":
		if strings.TrimSpace(m.ta.Value()) == "" {
			if m.cancel != nil {
				m.cancel()
			}
			return m, tea.Quit
		}
	case "ctrl+l":
		m.transcript = ""
		m.refresh(true)
		return m, nil
	case "pgup", "pgdown":
		var c tea.Cmd
		m.vp, c = m.vp.Update(msg)
		return m, c
	case "esc":
		m.sugIdx = 0
		return m, nil
	case "tab":
		if sug := m.suggestions(); len(sug) > 0 {
			m.ta.SetValue(sug[m.sugIdx%len(sug)].name + " ")
			m.ta.CursorEnd()
			m.sugIdx++
			return m, nil
		}
	case "up", "down":
		if sug := m.suggestions(); len(sug) > 0 {
			if k == "up" {
				m.sugIdx = (m.sugIdx + len(sug) - 1) % len(sug)
			} else {
				m.sugIdx = (m.sugIdx + 1) % len(sug)
			}
			return m, nil
		}
		if !strings.Contains(m.ta.Value(), "\n") {
			m.historyMove(k == "up")
			return m, nil
		}
	case "enter":
		if sug := m.suggestions(); len(sug) > 0 && m.ta.Value() != sug[m.sugIdx%len(sug)].name {
			if !strings.Contains(m.ta.Value(), " ") { // complete the highlighted command first
				m.ta.SetValue(sug[m.sugIdx%len(sug)].name)
				m.ta.CursorEnd()
				return m, nil
			}
		}
		return m, m.submit()
	}

	var c tea.Cmd
	m.ta, c = m.ta.Update(msg)
	m.sugIdx = clampIdx(m.sugIdx, len(m.suggestions()))
	m.layout() // input may have grown
	return m, c
}

func (m *tuiModel) answer(r executor.ApprovalResponse, label string) {
	m.approval.reply <- r
	m.appendRaw(dimSt.Render("   → "+label) + "\n")
	m.approval = nil
	m.layout()
}

func (m *tuiModel) submit() tea.Cmd {
	text := strings.TrimSpace(m.ta.Value())
	if text == "" {
		return nil
	}
	if m.running {
		m.appendRaw(m.ui.c(cYellow, "! a task is running — wait for it to finish or press Ctrl+C") + "\n")
		return nil
	}
	m.history = append(m.history, text)
	m.histIdx, m.draft, m.sugIdx = len(m.history), "", 0
	m.ta.Reset()
	m.layout()

	if strings.HasPrefix(text, "/") {
		return m.runSlash(text)
	}
	if m.rt == nil {
		m.appendRaw(m.ui.c(cYellow, "! not ready yet — enter your API key with /key") + "\n")
		return nil
	}
	m.appendRaw("\n" + userSt.Render("❯ "+strings.ReplaceAll(text, "\n", "\n  ")) + "\n\n")
	ctx, cancel := context.WithCancel(m.ctx)
	m.cancel, m.running, m.started, m.hadTask = cancel, true, time.Now(), true
	m.note = ""
	m.layout()
	rt := m.rt
	run := func() tea.Msg {
		defer cancel()
		return taskDoneMsg{rt.RunTask(ctx, text)}
	}
	return tea.Batch(run, m.spin.Tick)
}

func (m *tuiModel) runSlash(line string) tea.Cmd {
	name := strings.ToLower(strings.Fields(line)[0])
	switch name {
	case "/exit", "/quit", "/q":
		return tea.Quit
	case "/clear":
		m.transcript = ""
		m.refresh(true)
		return nil
	case "/key":
		m.enterKeyMode("")
		return nil
	case "/help":
		m.appendRaw(helpText + "\nKeys: Enter send · Ctrl+J newline · ↑/↓ prompt history · Tab complete · PgUp/PgDn scroll · Ctrl+L clear · Ctrl+C cancel · Ctrl+D quit\n\n")
		return nil
	}
	if m.rt == nil {
		m.appendRaw(m.ui.c(cYellow, "! not ready yet — enter your API key with /key") + "\n")
		return nil
	}
	app, cfg, ui, rt, ctx := m.app, m.cfg, m.ui, m.rt, m.ctx
	return func() tea.Msg {
		var buf bytes.Buffer
		quit, newRT := app.slash(ctx, cfg, ui, rt, line, &buf)
		return slashDone{out: buf.String() + "\n", quit: quit, newRT: newRT}
	}
}

func (m *tuiModel) enterKeyMode(note string) {
	m.mode = modeKey
	m.ki.SetValue("")
	m.ki.Focus()
	if note != "" {
		if m.landing() {
			m.note = note
		} else {
			m.appendRaw(m.ui.c(cYellow, note) + "\n")
		}
	}
	m.layout()
}

func (m *tuiModel) saveKey(key string) tea.Cmd {
	if key == "" {
		return nil
	}
	path, err := config.UserEnvPath()
	if err == nil {
		err = config.SetEnvFile(path, config.EnvAPIKey, key)
	}
	if err != nil {
		m.appendRaw(m.ui.c(cRed, "✖ could not save key: "+err.Error()) + "\n")
		return nil
	}
	os.Setenv(config.EnvAPIKey, key)
	m.cfg.APIKey = key
	m.mode = modeChat
	m.ki.SetValue("")
	m.layout()
	m.appendRaw(m.ui.c(cGreen, fmt.Sprintf("✓ API key saved (%s)", m.cfg.MaskedKey())) + "\n")
	old := m.rt
	if old != nil {
		old.syncSession()
	}
	cfg, ui := m.cfg, m.ui
	var sess *session.Session
	if old != nil {
		sess = old.Session
	}
	return func() tea.Msg {
		if old != nil {
			old.Close()
		}
		rt, err := newRuntime(cfg, ui, sess)
		return rtReadyMsg{rt, err}
	}
}

func (m *tuiModel) historyMove(up bool) {
	if len(m.history) == 0 {
		return
	}
	if m.histIdx == len(m.history) {
		m.draft = m.ta.Value()
	}
	if up && m.histIdx > 0 {
		m.histIdx--
	} else if !up && m.histIdx < len(m.history) {
		m.histIdx++
	}
	if m.histIdx == len(m.history) {
		m.ta.SetValue(m.draft)
	} else {
		m.ta.SetValue(m.history[m.histIdx])
	}
	m.ta.CursorEnd()
}

func (m *tuiModel) suggestions() []struct{ name, desc string } {
	v := m.ta.Value()
	if !strings.HasPrefix(v, "/") || strings.ContainsAny(v, " \n") {
		return nil
	}
	var out []struct{ name, desc string }
	for _, c := range slashCommands {
		if strings.HasPrefix(c.name, strings.ToLower(v)) {
			out = append(out, c)
		}
	}
	return out
}

func clampIdx(i, n int) int {
	if n == 0 || i >= n {
		return 0
	}
	return i
}

// ---- rendering --------------------------------------------------------------

func (m *tuiModel) appendRaw(s string) {
	m.transcript += s
	m.layout() // leaving the landing screen changes the input box width
	m.refresh(false)
}

// refresh re-wraps the transcript for the current width and keeps the view
// pinned to the bottom unless the user has scrolled up.
func (m *tuiModel) refresh(forceBottom bool) {
	if m.w == 0 {
		return
	}
	atBottom := m.vp.AtBottom() || forceBottom
	m.vp.SetContent(ansi.Wrap(m.transcript, max(m.w-1, 10), " "))
	if atBottom {
		m.vp.GotoBottom()
	}
}

func (m *tuiModel) inputHeight() int {
	if m.mode == modeKey {
		return 1
	}
	return min(max(m.ta.LineCount(), 1), 6)
}

func (m *tuiModel) popupHeight() int {
	if m.mode != modeChat || m.approval != nil {
		return 0
	}
	return min(len(m.suggestions()), 8)
}

func (m *tuiModel) layout() {
	if m.w == 0 {
		return
	}
	m.ta.SetWidth(m.boxW() - 4)
	m.ta.SetHeight(m.inputHeight())
	m.ki.Width = m.boxW() - 16
	box := 1 + 2 // content + border
	if m.approval != nil {
		box = m.approvalBoxHeight()
	} else {
		box = m.inputHeight() + 2
	}
	h := m.h - 1 - box - m.popupHeight() - 1 // header, input box, popup, status bar (chat layout)
	m.vp.Width, m.vp.Height = m.w, max(h, 3)
}

func (m *tuiModel) approvalBoxHeight() int { return 4 } // two content lines + border

func (m *tuiModel) View() string {
	if m.w == 0 {
		return ""
	}
	if m.landing() {
		return m.landingView()
	}
	header := headerSt.Render("ScoutClaw") + dimSt.Render("  ·  autonomous diagnostic agent")
	if m.rt != nil {
		header += dimSt.Render("  ·  session " + m.rt.Session.ID)
	}

	parts := []string{ansi.Truncate(header, m.w, "…"), m.vp.View()}
	if p := m.popup(); p != "" {
		parts = append(parts, p)
	}
	parts = append(parts, m.inputBox(), m.statusBar())
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

func (m *tuiModel) boxStyle(color lipgloss.Color) lipgloss.Style {
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(color).Width(m.boxW() - 2)
}

func (m *tuiModel) inputBox() string {
	switch {
	case m.approval != nil:
		d := m.approval.req.Decision
		col := warnCol
		if d.Risk == policy.High {
			col = errCol
		}
		opts := "[y] approve   [n] decline"
		if d.Risk == policy.Medium && d.Key != "" {
			opts += "   [a] always this session"
		}
		line := lipgloss.NewStyle().Bold(true).Foreground(col).Render(strings.ToUpper(d.Risk.String())+" RISK") +
			"  " + ansi.Truncate(m.ui.Redactor.Redact(m.approval.req.Summary), max(m.w-24, 10), "…") + "\n" + dimSt.Render(opts+"   (Esc = decline)")
		return m.boxStyle(col).Render(line)
	case m.mode == modeKey:
		return m.boxStyle(warnCol).Render(m.ki.View())
	case m.running:
		return m.boxStyle(subtle).Render(m.ta.View())
	}
	return m.boxStyle(accent).Render(m.ta.View())
}

func (m *tuiModel) popup() string {
	sug := m.suggestions()
	if len(sug) == 0 || m.mode != modeChat || m.approval != nil {
		return ""
	}
	sug = sug[:min(len(sug), 8)]
	sel := m.sugIdx % len(sug)
	var lines []string
	for i, c := range sug {
		l := fmt.Sprintf(" %-11s %s", c.name, c.desc)
		if i == sel {
			l = lipgloss.NewStyle().Foreground(accent).Bold(true).Render("▸" + l[1:])
		} else {
			l = dimSt.Render(l)
		}
		lines = append(lines, l)
	}
	return strings.Join(lines, "\n")
}

func (m *tuiModel) statusBar() string {
	var left string
	switch {
	case m.approval != nil:
		left = lipgloss.NewStyle().Foreground(warnCol).Render("⚠ waiting for your approval")
	case m.running:
		act := "thinking"
		if m.activity != "" {
			act = "$ " + m.activity
		}
		left = m.spin.View() + " " + ansi.Truncate(fmt.Sprintf("%s · %ds", act, int(time.Since(m.started).Seconds())), max(m.w/2, 10), "…") + dimSt.Render("  Ctrl+C cancels")
	case m.quitArmed:
		left = lipgloss.NewStyle().Foreground(warnCol).Render("press Ctrl+C again to quit")
	case m.mode == modeKey:
		left = dimSt.Render("Enter saves · Esc cancels")
	default:
		left = lipgloss.NewStyle().Foreground(okCol).Render("● ready")
	}
	// Right-hand details by priority: the workspace path is the first to go when space is short.
	tok := fmt.Sprintf("tokens %s/%s", human(m.tokensIn), human(m.tokensOu))
	var right string
	for _, cand := range []string{
		fmt.Sprintf("%s · %s · %s · %s", m.cfg.Model, m.cfg.Approval, shortPath(m.cfg.Workspace), tok),
		fmt.Sprintf("%s · %s · %s", m.cfg.Model, m.cfg.Approval, tok),
		fmt.Sprintf("%s · %s", m.cfg.Model, m.cfg.Approval),
		m.cfg.Model,
	} {
		right = dimSt.Render(cand)
		if m.w-lipgloss.Width(left)-lipgloss.Width(right) >= 3 {
			break
		}
		right = ""
	}
	gap := max(m.w-lipgloss.Width(left)-lipgloss.Width(right), 0)
	return left + strings.Repeat(" ", gap) + right
}

func shortPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

func human(n int64) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprint(n)
}
