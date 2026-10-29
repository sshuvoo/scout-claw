package cli

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"scoutclaw/internal/config"
	"scoutclaw/internal/executor"
	"scoutclaw/internal/policy"
	"scoutclaw/internal/redact"
)

func newTestTUI(t *testing.T, w, h int) *tuiModel {
	cfg := &config.Config{Model: "gpt-test", Approval: config.ApprovalAsk, Workspace: t.TempDir()}
	ui := &UI{Color: true, Redactor: redact.New()}
	m := newTUIModel(&App{}, context.Background(), cfg, ui)
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return m
}

func press(m *tuiModel, s string) {
	switch s {
	case "enter":
		m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	case "up":
		m.Update(tea.KeyMsg{Type: tea.KeyUp})
	case "down":
		m.Update(tea.KeyMsg{Type: tea.KeyDown})
	case "tab":
		m.Update(tea.KeyMsg{Type: tea.KeyTab})
	case "ctrl+c":
		m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	default:
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
	}
}

func lines(m *tuiModel) []string { return strings.Split(ansi.Strip(m.View()), "\n") }

func TestInputBoxAndStatusStickToBottom(t *testing.T) {
	m := newTestTUI(t, 80, 24)
	for i := 0; i < 200; i++ { // far more output than fits on screen
		m.appendRaw("line of output\n")
	}
	ls := lines(m)
	if len(ls) != 24 {
		t.Fatalf("view must fill exactly the terminal: %d lines", len(ls))
	}
	if !strings.HasPrefix(ls[len(ls)-2], "╰") || !strings.HasPrefix(ls[len(ls)-4], "╭") {
		t.Fatalf("input box must be pinned just above the status bar:\n%s", strings.Join(ls[len(ls)-5:], "\n"))
	}
	if !strings.Contains(ls[len(ls)-1], "ready") || !strings.Contains(ls[len(ls)-1], "gpt-test") {
		t.Fatalf("status bar: %q", ls[len(ls)-1])
	}
	if !strings.Contains(ls[0], "ScoutClaw") {
		t.Fatalf("header: %q", ls[0])
	}
}

func TestTranscriptAutoScrollsUnlessUserScrolledUp(t *testing.T) {
	m := newTestTUI(t, 80, 24)
	for i := 0; i < 100; i++ {
		m.appendRaw("row\n")
	}
	if !m.vp.AtBottom() {
		t.Fatal("must follow new output")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	if m.vp.AtBottom() {
		t.Fatal("PgUp must scroll")
	}
	pos := m.vp.YOffset
	m.appendRaw("new output while reading history\n")
	if m.vp.YOffset != pos {
		t.Fatal("must not yank the view while the user is reading older output")
	}
}

func TestLayoutSurvivesResizeAndLongLines(t *testing.T) {
	m := newTestTUI(t, 100, 30)
	m.appendRaw(strings.Repeat("word ", 200) + "\n")
	for _, sz := range [][2]int{{60, 20}, {120, 40}, {40, 12}} {
		m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		ls := lines(m)
		if len(ls) != sz[1] {
			t.Fatalf("%v: %d lines", sz, len(ls))
		}
		for _, l := range ls {
			if w := len([]rune(l)); w > sz[0] {
				t.Fatalf("%v: line wider than terminal (%d): %q", sz, w, l)
			}
		}
	}
}

func TestSlashSuggestionsPopupAndTabComplete(t *testing.T) {
	m := newTestTUI(t, 80, 24)
	press(m, "/mo")
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "/model") || strings.Contains(v, "/approval") {
		t.Fatalf("popup should list only matching commands:\n%s", v)
	}
	if len(strings.Split(v, "\n")) != 24 {
		t.Fatal("popup must not push the layout out of the screen")
	}
	press(m, "tab")
	if m.ta.Value() != "/model " {
		t.Fatalf("tab completion: %q", m.ta.Value())
	}
	m2 := newTestTUI(t, 80, 24)
	press(m2, "/")
	if got := len(m2.suggestions()); got != len(slashCommands) {
		t.Fatalf("bare / lists all commands, got %d", got)
	}
}

func TestPromptHistoryWithArrowKeys(t *testing.T) {
	m := newTestTUI(t, 80, 24)
	m.history = []string{"first", "second"}
	m.histIdx = 2
	press(m, "draft")
	press(m, "up")
	if m.ta.Value() != "second" {
		t.Fatalf("%q", m.ta.Value())
	}
	press(m, "up")
	if m.ta.Value() != "first" {
		t.Fatalf("%q", m.ta.Value())
	}
	press(m, "down")
	press(m, "down")
	if m.ta.Value() != "draft" {
		t.Fatalf("draft must be restored, got %q", m.ta.Value())
	}
}

func TestApprovalPanelCapturesKeysAndReplies(t *testing.T) {
	m := newTestTUI(t, 80, 24)
	req := executor.ApprovalRequest{Tool: "shell_exec", Summary: "systemctl restart docker",
		Decision: policy.Decision{Risk: policy.High, Reason: "changes service state"}}
	reply := make(chan executor.ApprovalResponse, 1)
	m.Update(approvalMsg{req: req, reply: reply})
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "HIGH RISK") || !strings.Contains(v, "systemctl restart docker") || !strings.Contains(v, "[y] approve") {
		t.Fatalf("approval panel not shown:\n%s", v)
	}
	if strings.Contains(v, "[a] always") {
		t.Fatal("high risk must not offer session-wide approval")
	}
	if len(strings.Split(v, "\n")) != 24 {
		t.Fatal("layout must stay fixed during approval")
	}
	press(m, "x") // unrelated keys do nothing
	select {
	case <-reply:
		t.Fatal("must wait for an explicit answer")
	default:
	}
	press(m, "n")
	if r := <-reply; r.Approved {
		t.Fatal("n must decline")
	}
	if m.approval != nil {
		t.Fatal("panel must close")
	}

	reply2 := make(chan executor.ApprovalResponse, 1)
	med := executor.ApprovalRequest{Summary: "go test ./...", Decision: policy.Decision{Risk: policy.Medium, Key: "shell_exec:go test"}}
	m.Update(approvalMsg{req: med, reply: reply2})
	press(m, "a")
	if r := <-reply2; !r.Approved || !r.Remember {
		t.Fatalf("a = approve and remember: %+v", r)
	}
}

func TestCtrlCDoubleTapQuitsWhenIdle(t *testing.T) {
	m := newTestTUI(t, 80, 24)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil || !strings.Contains(ansi.Strip(m.View()), "press Ctrl+C again") {
		t.Fatal("first Ctrl+C should arm quit and say so")
	}
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("second Ctrl+C must quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("expected quit")
	}
}

func TestEnterIgnoredWhileTaskRunningAndEmptyInputIgnored(t *testing.T) {
	m := newTestTUI(t, 80, 24)
	press(m, "enter") // empty
	if len(m.history) != 0 {
		t.Fatal("empty input must not be submitted")
	}
	m.running = true
	press(m, "do something")
	press(m, "enter")
	if !strings.Contains(m.transcript, "a task is running") {
		t.Fatalf("user must be told why nothing happened: %q", m.transcript)
	}
}

func TestKeyModeMasksInput(t *testing.T) {
	m := newTestTUI(t, 80, 24)
	m.enterKeyMode("")
	press(m, "sk-secret-value-123")
	v := ansi.Strip(m.View())
	if strings.Contains(v, "secret-value") || !strings.Contains(v, "•") {
		t.Fatalf("API key must be masked:\n%s", v)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.mode != modeChat || m.ki.Value() != "" {
		t.Fatal("Esc must cancel and clear the key")
	}
}
