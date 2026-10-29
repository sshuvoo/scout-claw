package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"scoutclaw/internal/config"
	"scoutclaw/internal/llm"
)

type harness struct {
	out, errb bytes.Buffer
	app       *App
	prov      *llm.Scripted
}

func newHarness(t *testing.T, stdin string, steps ...func(llm.Request) (*llm.Response, error)) *harness {
	ws := t.TempDir()
	t.Setenv(config.EnvStateDir, t.TempDir())
	t.Setenv(config.EnvWorkspace, ws)
	t.Setenv(config.EnvAPIKey, "sk-test-key-not-real-0000000000")
	for _, k := range []string{config.EnvApproval, config.EnvModel, config.EnvMaxSteps, config.EnvBaseURL} {
		t.Setenv(k, "")
	}
	h := &harness{prov: &llm.Scripted{Steps: steps}}
	h.app = &App{In: strings.NewReader(stdin), Out: &h.out, Err: &h.errb}
	old := providerFactory
	providerFactory = func(*config.Config, *UI) (llm.Provider, error) { return h.prov, nil }
	t.Cleanup(func() { providerFactory = old })
	return h
}

func (h *harness) run(args ...string) int {
	return h.app.Run(context.Background(), args)
}

func TestHelpListsCommandsAndExamples(t *testing.T) {
	h := newHarness(t, "")
	if code := h.run("--help"); code != 0 {
		t.Fatal(code)
	}
	for _, want := range []string{"run", "chat", "doctor", "status", "config", "history", "version", "--approval", "Examples:"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("help is missing %q", want)
		}
	}
}

func TestVersion(t *testing.T) {
	h := newHarness(t, "")
	h.run("version")
	if !strings.HasPrefix(h.out.String(), "scout ") {
		t.Fatal(h.out.String())
	}
}

func TestConfigNeverPrintsTheKey(t *testing.T) {
	h := newHarness(t, "")
	if h.run("config") != 0 || h.run("status") != 0 {
		t.Fatal(h.errb.String())
	}
	if strings.Contains(h.out.String(), "not-real-0000") {
		t.Fatalf("api key leaked:\n%s", h.out.String())
	}
	if !strings.Contains(h.out.String(), "gpt-5.5") {
		t.Fatalf("existing default model must be preserved:\n%s", h.out.String())
	}
}

func TestInvalidConfigFailsSafely(t *testing.T) {
	h := newHarness(t, "")
	t.Setenv(config.EnvApproval, "yolo")
	if code := h.run("status"); code == 0 || !strings.Contains(h.errb.String(), "approval") {
		t.Fatalf("code=%d err=%s", code, h.errb.String())
	}
}

func TestMissingKeyIsExplained(t *testing.T) {
	h := newHarness(t, "")
	t.Setenv(config.EnvAPIKey, "")
	providerFactory = func(cfg *config.Config, _ *UI) (llm.Provider, error) { return nil, cfg.RequireAPIKey() }
	if code := h.run("run", "hello"); code == 0 || !strings.Contains(h.errb.String(), "OPENAI_API_KEY") {
		t.Fatalf("code=%d err=%s", code, h.errb.String())
	}
	if code := h.run("doctor"); code == 0 || !strings.Contains(h.out.String(), "api key") {
		t.Fatalf("doctor should fail without a key: %s", h.out.String())
	}
}

func finishCall(status string) func(llm.Request) (*llm.Response, error) {
	args, _ := json.Marshal(map[string]any{"summary": "disk is fine", "status": status})
	return llm.Calls(llm.ToolCall{ID: "f1", Name: "finish", Arguments: string(args)})
}

func TestRunTaskEndToEndAndHistory(t *testing.T) {
	h := newHarness(t, "",
		llm.Calls(llm.ToolCall{ID: "1", Name: "shell_exec", Arguments: `{"command":"echo","args":["hello-from-tool"]}`}),
		finishCall("not_needed"),
	)
	if code := h.run("run", "check the disk"); code != 0 {
		t.Fatalf("code=%d\nout=%s\nerr=%s", code, h.out.String(), h.errb.String())
	}
	out := h.out.String()
	for _, want := range []string{"$ echo hello-from-tool", "hello-from-tool", "disk is fine", "Findings"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	h.out.Reset()
	if h.run("history") != 0 || !strings.Contains(h.out.String(), "check the disk") || !strings.Contains(h.out.String(), "1 task(s)") {
		t.Fatalf("history: %s", h.out.String())
	}
	id := strings.Fields(h.out.String())[0]
	h.out.Reset()
	if h.run("history", id) != 0 || !strings.Contains(h.out.String(), "shell_exec") || !strings.Contains(h.out.String(), "policy: low risk") {
		t.Fatalf("history show: %s", h.out.String())
	}
}

func TestNonInteractiveApprovalIsDeniedNotAssumed(t *testing.T) {
	h := newHarness(t, "",
		llm.Calls(llm.ToolCall{ID: "1", Name: "fs_write", Arguments: `{"path":"new.txt","content":"x"}`}),
		func(req llm.Request) (*llm.Response, error) {
			if !strings.Contains(req.ToolOutputs[0].Output, "declined") {
				t.Errorf("%q", req.ToolOutputs[0].Output)
			}
			return &llm.Response{Text: "could not write"}, nil
		},
	)
	h.app.stdinT = false
	h.run("run", "write a file")
	if !strings.Contains(h.out.String(), "Approval needed") || !strings.Contains(h.out.String(), "not approved") {
		t.Fatalf("%s", h.out.String())
	}
}

func TestInteractiveApprovalYes(t *testing.T) {
	h := newHarness(t, "y\n",
		llm.Calls(llm.ToolCall{ID: "1", Name: "fs_write", Arguments: `{"path":"new.txt","content":"x"}`}),
		llm.Reply("written"),
	)
	h.app.stdinT = true
	h.run("run", "write a file")
	if !strings.Contains(h.out.String(), "allow?") || !strings.Contains(h.out.String(), "✓ ok") || !strings.Contains(h.out.String(), "Changes were made") {
		t.Fatalf("%s", h.out.String())
	}
}

func TestBareArgumentRunsATaskForBackwardCompatibility(t *testing.T) {
	h := newHarness(t, "", llm.Reply("answer: 42"))
	if code := h.run("what", "is", "the", "answer"); code != 0 {
		t.Fatal(h.errb.String())
	}
	if !strings.Contains(h.out.String(), "answer: 42") || !strings.Contains(h.prov.Requests[0].UserText, "what is the answer") {
		t.Fatalf("%s", h.out.String())
	}
}

func TestChatKeepsSessionAcrossTasksAndSupportsReset(t *testing.T) {
	h := newHarness(t, "first question\nsecond question\nreset\nthird\nexit\n",
		llm.Reply("a1"), llm.Reply("a2"), llm.Reply("a3"))
	if code := h.run("chat"); code != 0 {
		t.Fatal(h.errb.String())
	}
	r := h.prov.Requests
	if len(r) != 3 || r[0].PreviousID != "" || r[1].PreviousID == "" || r[2].PreviousID != "" {
		t.Fatalf("conversation continuity wrong: %+v", r)
	}
	if !strings.Contains(h.out.String(), "Context cleared") {
		t.Fatal(h.out.String())
	}
}

func TestResumeLastSession(t *testing.T) {
	h := newHarness(t, "", llm.Reply("one"), llm.Reply("two"))
	h.run("run", "first")
	h.out.Reset()
	if code := h.run("run", "--resume", "last", "second"); code != 0 {
		t.Fatal(h.errb.String())
	}
	if h.prov.Requests[1].PreviousID == "" || h.prov.Requests[1].UserText != "second" {
		t.Fatalf("resume should continue the provider conversation: %+v", h.prov.Requests[1])
	}
	if code := h.run("run", "--resume", "no-such-session", "x"); code == 0 {
		t.Fatal("unknown session must fail")
	}
}

func TestFlagsOverrideEnvironment(t *testing.T) {
	h := newHarness(t, "", llm.Reply("ok"))
	t.Setenv(config.EnvApproval, "auto")
	h.run("--approval", "readonly", "config")
	if !strings.Contains(h.out.String(), "readonly") {
		t.Fatal(h.out.String())
	}
}

func TestSlashCommandsInChat(t *testing.T) {
	h := newHarness(t, "/help\n/approval readonly\n/approval bogus\n/model gpt-test\n/model\n/config\n/status\n/nope\n/key\n/exit\n")
	if code := h.run("chat"); code != 0 {
		t.Fatal(h.errb.String())
	}
	out := h.out.String()
	for _, want := range []string{"/approval [mode]", "approval is now readonly", "unknown mode", "model is now gpt-test", "model: gpt-test",
		"SCOUTCLAW_MODEL", "unknown command /nope", "/key needs an interactive terminal", "Bye from ScoutClaw"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "not-real-0000") {
		t.Fatal("key leaked")
	}
}

func TestApprovalSwitchAppliesToPolicy(t *testing.T) {
	h := newHarness(t, "/approval readonly\nwrite a file\n/exit\n",
		llm.Calls(llm.ToolCall{ID: "1", Name: "fs_write", Arguments: `{"path":"n.txt","content":"x"}`}),
		func(req llm.Request) (*llm.Response, error) {
			if !strings.Contains(req.ToolOutputs[0].Output, "approval policy is readonly") {
				t.Errorf("policy mode not applied: %q", req.ToolOutputs[0].Output)
			}
			return &llm.Response{Text: "denied"}, nil
		})
	h.run("chat")
}
