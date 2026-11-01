package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"scoutclaw/internal/config"
	"scoutclaw/internal/llm"
)

func wsPath(t *testing.T, rel string) string {
	t.Helper()
	ws, _ := filepath.EvalSymlinks(os.Getenv(config.EnvWorkspace))
	return filepath.Join(ws, filepath.FromSlash(rel))
}

func writeWS(t *testing.T, rel, content string) {
	t.Helper()
	p := wsPath(t, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTrustPrompt(t *testing.T) {
	tests := []struct {
		name         string
		stdin        string
		interactive  bool
		flags        []string
		wantApproval string
		wantOut      []string
		wantErr      []string
		trusted      bool
	}{
		{name: "yes trusts and applies", stdin: "y\n", interactive: true, wantApproval: "approval auto",
			wantOut: []string{"would loosen", `approval "auto"`, "Trusted."}, trusted: true},
		{name: "no keeps it ignored", stdin: "n\n", interactive: true, wantApproval: "approval ask",
			wantOut: []string{"Not trusted"}},
		{name: "no terminal ignores it", interactive: false, wantApproval: "approval ask",
			wantErr: []string{"not trusted: these settings are ignored"}},
		{name: "flag applies it once", interactive: false, flags: []string{"--trust-workspace"}, wantApproval: "approval auto"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, tt.stdin, llm.Reply("ok"))
			writeWS(t, ".scout/config.json", `{"approval":"auto"}`)
			h.app.stdinT = tt.interactive
			args := append([]string{"run"}, tt.flags...)
			if code := h.run(append(args, "hello")...); code != 0 {
				t.Fatalf("exit %d: %s", code, h.errb.String())
			}
			if !strings.Contains(h.out.String(), tt.wantApproval) {
				t.Fatalf("want %q in:\n%s", tt.wantApproval, h.out.String())
			}
			for _, w := range tt.wantOut {
				if !strings.Contains(h.out.String(), w) {
					t.Errorf("stdout missing %q:\n%s", w, h.out.String())
				}
			}
			for _, w := range tt.wantErr {
				if !strings.Contains(h.errb.String(), w) {
					t.Errorf("stderr missing %q:\n%s", w, h.errb.String())
				}
			}
			_, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".scout", "trusted.json"))
			if (err == nil) != tt.trusted {
				t.Fatalf("trust store exists = %v, want %v", err == nil, tt.trusted)
			}
		})
	}
}

func TestPermissionsCommands(t *testing.T) {
	h := newHarness(t, "")
	steps := []struct {
		args    []string
		code    int
		wantOut string
	}{
		{[]string{"permissions"}, 0, "no permission rules"},
		{[]string{"permissions", "add", "allow", "shell_exec(go test *)"}, 0, "added allow shell_exec(go test *) to " + filepath.Join(".scout", "config.local.json")},
		{[]string{"permissions", "add", "allow", "shell_exec(go test *)"}, 0, "already in"},
		{[]string{"permissions", "add", "deny", "fs_read(**/.env*)", "--project"}, 0, filepath.Join(".scout", "config.json")},
		{[]string{"permissions", "add", "allow", "fs_write", "--project"}, 0, "apply only after you trust"},
		{[]string{"permissions", "add", "allow", "shell_script"}, 1, ""},
		{[]string{"permissions", "add", "permit", "fs_write"}, 1, ""},
		{[]string{"permissions", "add", "allow", "fs_write", "--global", "--local"}, 1, ""},
		{[]string{"permissions"}, 0, "deny  fs_read(**/.env*)"},
		{[]string{"permissions", "remove", "shell_exec(go test *)"}, 0, "removed shell_exec(go test *)"},
		{[]string{"permissions", "remove", "shell_exec(go test *)"}, 1, ""},
	}
	for _, s := range steps {
		h.out.Reset()
		h.errb.Reset()
		if code := h.run(s.args...); code != s.code {
			t.Fatalf("%v: exit %d, want %d (%s)", s.args, code, s.code, h.errb.String())
		}
		if s.wantOut != "" && !strings.Contains(h.out.String(), s.wantOut) {
			t.Fatalf("%v: output missing %q:\n%s", s.args, s.wantOut, h.out.String())
		}
	}
	h.out.Reset()
	h.run("permissions")
	if !strings.Contains(h.out.String(), "loosening setting(s)") {
		t.Fatalf("an untrusted shared allow rule should be reported:\n%s", h.out.String())
	}
}

func TestConfigSetGetUnset(t *testing.T) {
	h := newHarness(t, "")
	steps := []struct {
		args    []string
		code    int
		wantOut string
	}{
		{[]string{"config", "set", "approval", "edits"}, 0, "approval = edits in " + filepath.Join(".scout", "config.local.json")},
		{[]string{"config", "get", "approval"}, 0, "edits\t(" + filepath.Join(".scout", "config.local.json") + ")"},
		{[]string{"config", "set", "maxSteps", "150", "--project"}, 0, "maxSteps = 150"},
		{[]string{"config", "get", "maxSteps"}, 0, "150\t(" + filepath.Join(".scout", "config.json") + ")"},
		{[]string{"config", "set", "model", "gpt-custom", "--global"}, 0, filepath.Join("~/.scout", "config.json")},
		{[]string{"config", "get", "model"}, 0, "gpt-custom"},
		{[]string{"config", "set", "baseUrl", "https://x"}, 1, ""},
		{[]string{"config", "set", "approval", "yolo"}, 1, ""},
		{[]string{"config", "get", "colour"}, 1, ""},
		{[]string{"config"}, 0, "[" + filepath.Join(".scout", "config.local.json") + "]"},
		{[]string{"config", "unset", "approval"}, 0, "removed approval"},
		{[]string{"config", "unset", "approval"}, 0, "is not set"},
		{[]string{"config", "get", "approval"}, 0, "ask\t(default)"},
	}
	for _, s := range steps {
		h.out.Reset()
		h.errb.Reset()
		if code := h.run(s.args...); code != s.code {
			t.Fatalf("%v: exit %d, want %d (%s)", s.args, code, s.code, h.errb.String())
		}
		if s.wantOut != "" && !strings.Contains(h.out.String(), s.wantOut) {
			t.Fatalf("%v: output missing %q:\n%s", s.args, s.wantOut, h.out.String())
		}
	}
	t.Setenv(config.EnvModel, "from-env")
	h.out.Reset()
	h.run("config", "set", "model", "x")
	if !strings.Contains(h.out.String(), "SCOUTCLAW_MODEL overrides config files") {
		t.Fatalf("setting a value the environment overrides should say so:\n%s", h.out.String())
	}
}

func TestInitAndTrustCommands(t *testing.T) {
	h := newHarness(t, "")
	if code := h.run("init"); code != 0 || !strings.Contains(h.out.String(), "created") {
		t.Fatalf("init: %d %s", code, h.out.String())
	}
	h.out.Reset()
	h.run("init")
	if !strings.Contains(h.out.String(), "already has .scout/config.json") {
		t.Fatalf("second init: %s", h.out.String())
	}
	h.out.Reset()
	h.run("trust", "--revoke")
	if !strings.Contains(h.out.String(), "was not trusted") {
		t.Fatalf("revoke before trust: %s", h.out.String())
	}
	writeWS(t, ".scout/config.json", `{"permissions":{"allow":["fs_write"]}}`)
	h.out.Reset()
	if code := h.run("trust"); code != 0 || !strings.Contains(h.out.String(), "allow rule fs_write") || !strings.Contains(h.out.String(), "trusted") {
		t.Fatalf("trust: %d %s", code, h.out.String())
	}
	h.out.Reset()
	h.run("permissions")
	if strings.Contains(h.out.String(), "loosening setting(s)") {
		t.Fatalf("trusted project should not report ignored settings:\n%s", h.out.String())
	}
	h.out.Reset()
	h.run("trust", "--revoke")
	if !strings.Contains(h.out.String(), "no longer trusting") {
		t.Fatalf("revoke: %s", h.out.String())
	}
}

func TestAlwaysInProjectSavesRuleAndAppliesIt(t *testing.T) {
	h := newHarness(t, "write a\np\nwrite b\n/permissions\n/exit\n",
		llm.Calls(llm.ToolCall{ID: "1", Name: "fs_write", Arguments: `{"path":"a.txt","content":"a"}`}),
		llm.Reply("wrote a"),
		llm.Calls(llm.ToolCall{ID: "2", Name: "fs_write", Arguments: `{"path":"b.txt","content":"b"}`}),
		llm.Reply("wrote b"),
	)
	h.app.stdinT = true
	if code := h.run("chat"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	out := h.out.String()
	if n := strings.Count(out, "allow? "); n != 1 {
		t.Fatalf("want exactly one approval prompt, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "[p] saves the rule fs_write to .scout/config.local.json") || !strings.Contains(out, "allow   fs_write") && !strings.Contains(out, "allow fs_write") {
		t.Fatalf("prompt or /permissions output missing:\n%s", out)
	}
	b, err := os.ReadFile(wsPath(t, ".scout/config.local.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Permissions struct{ Allow []string } `json:"permissions"`
	}
	if json.Unmarshal(b, &m) != nil || len(m.Permissions.Allow) != 1 || m.Permissions.Allow[0] != "fs_write" {
		t.Fatalf("saved file: %s", b)
	}
	for _, f := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(wsPath(t, f)); err != nil {
			t.Errorf("%s was not written", f)
		}
	}
}

func TestConfigWarningsAndBadRules(t *testing.T) {
	h := newHarness(t, "", llm.Reply("ok"))
	writeWS(t, ".scout/config.local.json", `{"colour":"red","permissions":{"allow":["shell_script"]}}`)
	if code := h.run("run", "hi"); code == 0 {
		t.Fatal("an invalid rule must stop startup")
	}
	if !strings.Contains(h.errb.String(), `unknown setting "colour"`) || !strings.Contains(h.errb.String(), "never allowed") || !strings.Contains(h.errb.String(), "config.local.json") {
		t.Fatalf("stderr: %s", h.errb.String())
	}
}

func TestDenyRuleFromConfigReachesTheModel(t *testing.T) {
	h := newHarness(t, "",
		llm.Calls(llm.ToolCall{ID: "1", Name: "fs_read", Arguments: `{"path":".env"}`}),
		func(req llm.Request) (*llm.Response, error) {
			if !strings.Contains(req.ToolOutputs[0].Output, "denied by rule deny fs_read(**/.env*)") {
				t.Errorf("model should see which rule denied it: %q", req.ToolOutputs[0].Output)
			}
			return &llm.Response{Text: "blocked"}, nil
		})
	writeWS(t, ".env", "SECRET=1\n")
	writeWS(t, ".scout/config.json", `{"permissions":{"deny":["fs_read(**/.env*)"]}}`)
	if code := h.run("run", "read .env"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
}

func TestResultLineNamesTheAllowingRule(t *testing.T) {
	h := newHarness(t, "",
		llm.Calls(llm.ToolCall{ID: "1", Name: "fs_write", Arguments: `{"path":"n.txt","content":"x"}`}),
		llm.Reply("done"))
	writeWS(t, ".scout/config.local.json", `{"permissions":{"allow":["fs_write"]}}`)
	if code := h.run("run", "write"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if !strings.Contains(h.out.String(), "[medium risk, allowed by rule fs_write]") || strings.Contains(h.out.String(), "allow? ") {
		t.Fatalf("output:\n%s", h.out.String())
	}
}
