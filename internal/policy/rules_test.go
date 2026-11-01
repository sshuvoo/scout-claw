package policy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"scoutclaw/internal/config"
)

func mustRule(t *testing.T, text string, action RuleAction, personal bool) Rule {
	t.Helper()
	r, err := ParseRule(text, action, "test.json", personal)
	if err != nil {
		t.Fatalf("ParseRule(%q): %v", text, err)
	}
	return r
}

func jsonIn(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

func TestParseRule(t *testing.T) {
	tests := []struct {
		text    string
		action  RuleAction
		wantErr string
	}{
		{"fs_write", RuleAllow, ""},
		{"fs_*", RuleDeny, ""},
		{"fs_read(**/.env*)", RuleDeny, ""},
		{"fs_write(/etc/**)", RuleAsk, ""},
		{"fs_write(~/notes/**)", RuleAllow, ""},
		{"shell_exec(go test *)", RuleAllow, ""},
		{"proc_start(npm run *)", RuleAllow, ""},
		{"net_http(domain:*.github.com)", RuleAllow, ""},
		{"shell_script", RuleDeny, ""},
		{"shell_powershell", RuleAsk, ""},
		{"update_plan", RuleAllow, ""},
		{"shell_script", RuleAllow, "never allowed"},
		{"shell_*", RuleAllow, "never allowed"},
		{"*", RuleAllow, "never allowed"},
		{"shell_script(rm *)", RuleDeny, "takes no pattern"},
		{"fs_write()", RuleAllow, "empty pattern"},
		{"shell_exec( * )", RuleAllow, "name the command"},
		{"net_http(github.com)", RuleAllow, "domain:host"},
		{"sys_info(cpu)", RuleAllow, "take no pattern"},
		{"Shell_Exec(go)", RuleAllow, "want Tool or Tool(spec)"},
		{"fs_write(src/**", RuleAllow, "want Tool or Tool(spec)"},
	}
	for _, tt := range tests {
		_, err := ParseRule(tt.text, tt.action, "f", false)
		if tt.wantErr == "" && err != nil {
			t.Errorf("%q: unexpected error %v", tt.text, err)
		}
		if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
			t.Errorf("%q: err = %v, want %q", tt.text, err, tt.wantErr)
		}
	}
	if _, err := ParseRuleAction("permit"); err == nil {
		t.Error("unknown action must fail")
	}
}

func TestRuleMatching(t *testing.T) {
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	scope := NewScope(ws)
	home, _ := os.UserHomeDir()
	absDir, _ := filepath.EvalSymlinks(t.TempDir())
	absPattern := "fs_write(" + filepath.ToSlash(absDir) + "/**)"
	tests := []struct {
		name   string
		rule   string
		action RuleAction
		tool   string
		in     any
		want   bool
	}{
		{"whole tool", "fs_write", RuleAllow, "fs_write", map[string]any{"path": "a.go"}, true},
		{"tool wildcard", "fs_*", RuleDeny, "fs_patch", map[string]any{"path": "a.go"}, true},
		{"tool wildcard does not cross names", "fs_*", RuleDeny, "shell_exec", map[string]any{"command": "ls"}, false},
		{"base name at any depth", "fs_read(**/.env*)", RuleDeny, "fs_read", map[string]any{"path": "svc/api/.env.local"}, true},
		{"glob without slash matches base names", "fs_write(*.md)", RuleAllow, "fs_write", map[string]any{"path": "docs/x.md"}, true},
		{"anchored path", "fs_write(src/**)", RuleAllow, "fs_write", map[string]any{"path": "src/a/b.go"}, true},
		{"anchored path misses", "fs_write(src/**)", RuleAllow, "fs_write", map[string]any{"path": "test/b.go"}, false},
		{"relative pattern never matches outside the project", "fs_write(**)", RuleAllow, "fs_write", map[string]any{"path": "../x.go"}, false},
		{"absolute pattern", absPattern, RuleAsk, "fs_write", map[string]any{"path": filepath.Join(absDir, "a")}, true},
		{"absolute pattern misses other dirs", absPattern, RuleAsk, "fs_write", map[string]any{"path": filepath.Join(ws, "a")}, false},
		{"home pattern", "fs_read(~/notes/**)", RuleAllow, "fs_read", map[string]any{"path": filepath.Join(home, "notes", "a.md")}, true},
		{"deny copy if either side matches", "fs_copy(secret/**)", RuleDeny, "fs_copy", map[string]any{"src": "secret/k", "dest": "pub/k"}, true},
		{"allow copy only if both sides match", "fs_copy(pub/**)", RuleAllow, "fs_copy", map[string]any{"src": "secret/k", "dest": "pub/k"}, false},
		{"allow copy when both match", "fs_copy(pub/**)", RuleAllow, "fs_copy", map[string]any{"src": "pub/a", "dest": "pub/b"}, true},
		{"allow patch only if every file matches", "fs_apply_patch(src/**)", RuleAllow, "fs_apply_patch",
			map[string]any{"patch": "*** Begin Patch\n*** Update File: src/a.go\n-a\n+b\n*** Add File: Makefile\n+x\n*** End Patch"}, false},
		{"deny patch if any file matches", "fs_apply_patch(Makefile)", RuleDeny, "fs_apply_patch",
			map[string]any{"patch": "*** Begin Patch\n*** Update File: src/a.go\n-a\n+b\n*** Add File: Makefile\n+x\n*** End Patch"}, true},
		{"read many: allow needs all", "fs_read_many(docs/**)", RuleAllow, "fs_read_many", map[string]any{"files": []map[string]any{{"path": "docs/a"}, {"path": "src/b"}}}, false},
		{"empty path means the project root", "fs_grep(**)", RuleAllow, "fs_grep", map[string]any{"pattern": "x"}, true},
		{"empty path is not under a subdirectory", "fs_grep(src/**)", RuleAllow, "fs_grep", map[string]any{"pattern": "x"}, false},
		{"command prefix", "shell_exec(go test *)", RuleAllow, "shell_exec", map[string]any{"command": "go", "args": []string{"test", "./..."}}, true},
		{"command prefix with no extra args", "shell_exec(go test *)", RuleAllow, "shell_exec", map[string]any{"command": "go", "args": []string{"test"}}, true},
		{"command prefix differs", "shell_exec(go test *)", RuleAllow, "shell_exec", map[string]any{"command": "go", "args": []string{"tool", "test"}}, false},
		{"exact command", "shell_exec(git status)", RuleAllow, "shell_exec", map[string]any{"command": "git", "args": []string{"status"}}, true},
		{"exact command rejects extra args", "shell_exec(git status)", RuleAllow, "shell_exec", map[string]any{"command": "git", "args": []string{"status", "-s"}}, false},
		{"command path is reduced to its name", "shell_exec(go test *)", RuleAllow, "shell_exec", map[string]any{"command": "/usr/local/go/bin/go", "args": []string{"test"}}, true},
		{"proc_start uses commands too", "proc_start(npm run *)", RuleAllow, "proc_start", map[string]any{"command": "npm", "args": []string{"run", "dev"}}, true},
		{"domain wildcard", "net_http(domain:*.github.com)", RuleAllow, "net_http", map[string]any{"url": "https://api.github.com/x"}, true},
		{"domain is case-insensitive", "net_http(domain:*.github.com)", RuleAllow, "net_http", map[string]any{"url": "https://API.GitHub.com"}, true},
		{"domain mismatch", "net_http(domain:*.github.com)", RuleAllow, "net_http", map[string]any{"url": "https://github.com.evil.io"}, false},
		{"domain for dns", "net_dns(domain:example.com)", RuleAllow, "net_dns", map[string]any{"host": "example.com"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := mustRule(t, tt.rule, tt.action, true)
			rs := RuleSet{Rules: []Rule{r}, Base: ws}
			_, got := rs.match(tt.action, tt.tool, jsonIn(tt.in), scope)
			if got != tt.want {
				t.Fatalf("match = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDecideAppliesRulesInOrder(t *testing.T) {
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	rule := func(text string, a RuleAction, personal bool) Rule {
		r, err := ParseRule(text, a, map[bool]string{true: "local.json", false: "project.json"}[personal], personal)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	tests := []struct {
		name   string
		mode   config.Approval
		rules  []Rule
		tool   string
		in     json.RawMessage
		action Action
		rule   string
	}{
		{"allow skips the prompt", config.ApprovalAsk, []Rule{rule("shell_exec(go test *)", RuleAllow, false)},
			"shell_exec", exec("go", "test", "./..."), Allow, "allow shell_exec(go test *) (project.json)"},
		{"deny beats allow", config.ApprovalAsk, []Rule{rule("fs_write", RuleAllow, true), rule("fs_write(secrets/**)", RuleDeny, false)},
			"fs_write", jsonIn(map[string]any{"path": "secrets/a"}), Deny, "deny fs_write(secrets/**) (project.json)"},
		{"ask beats allow", config.ApprovalAsk, []Rule{rule("shell_exec(git *)", RuleAllow, true), rule("shell_exec(git push *)", RuleAsk, false)},
			"shell_exec", exec("git", "push"), Ask, "ask shell_exec(git push *) (project.json)"},
		{"ask beats auto mode", config.ApprovalAuto, []Rule{rule("shell_exec(npm install *)", RuleAsk, false)},
			"shell_exec", exec("npm", "install"), Ask, "ask shell_exec(npm install *) (project.json)"},
		{"ask applies to low risk too", config.ApprovalAuto, []Rule{rule("fs_read(**/*.pem)", RuleAsk, false)},
			"fs_read", jsonIn(map[string]any{"path": "certs/a.pem"}), Ask, "ask fs_read(**/*.pem) (project.json)"},
		{"deny applies to low risk", config.ApprovalAsk, []Rule{rule("fs_read(**/.env*)", RuleDeny, false)},
			"fs_read", jsonIn(map[string]any{"path": ".env"}), Deny, "deny fs_read(**/.env*) (project.json)"},
		{"shared rules never allow high risk", config.ApprovalAsk, []Rule{rule("fs_write", RuleAllow, false)},
			"fs_write", jsonIn(map[string]any{"path": "../outside.txt"}), Ask, ""},
		{"personal rules may allow high risk", config.ApprovalAsk, []Rule{rule("shell_exec(git push *)", RuleAllow, true)},
			"shell_exec", exec("git", "push", "origin", "main"), Allow, "allow shell_exec(git push *) (local.json)"},
		{"no rule can lift a built-in refusal", config.ApprovalAuto, []Rule{rule("shell_exec(rm *)", RuleAllow, true)},
			"shell_exec", exec("rm", "-rf", "/"), Deny, ""},
		{"readonly ignores allow rules", config.ApprovalReadOnly, []Rule{rule("fs_write", RuleAllow, true)},
			"fs_write", jsonIn(map[string]any{"path": "a.go"}), Deny, ""},
		{"readonly turns an ask rule on a change into a denial", config.ApprovalReadOnly, []Rule{rule("fs_write", RuleAsk, true)},
			"fs_write", jsonIn(map[string]any{"path": "a.go"}), Deny, "ask fs_write (local.json)"},
		{"no matching rule falls back to the mode", config.ApprovalAsk, []Rule{rule("shell_exec(go test *)", RuleAllow, true)},
			"shell_exec", exec("npm", "test"), Ask, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := New(NewScope(ws), tt.mode)
			e.Rules = RuleSet{Rules: tt.rules, Base: ws}
			d := e.Decide(tt.tool, tt.in)
			if d.Action != tt.action || d.Rule != tt.rule {
				t.Fatalf("action=%s rule=%q (%s); want action=%s rule=%q", d.Action, d.Rule, d.Reason, tt.action, tt.rule)
			}
		})
	}
	e := New(NewScope(ws), config.ApprovalAsk)
	e.Rules = RuleSet{Rules: []Rule{rule("shell_exec(npm install *)", RuleAsk, true)}, Base: ws}
	if d := e.Decide("shell_exec", exec("npm", "install")); d.Key != "" {
		t.Fatalf("an ask rule must ask every time, so no session key: %q", d.Key)
	}
}

func TestSuggestRule(t *testing.T) {
	tests := []struct {
		tool string
		in   json.RawMessage
		d    Decision
		want string
	}{
		{"shell_exec", exec("go", "test", "./..."), Decision{Risk: Medium}, "shell_exec(go test *)"},
		{"shell_exec", exec("npm", "--prefix", "web", "run", "build"), Decision{Risk: Medium}, "shell_exec(npm web *)"},
		{"shell_exec", exec("make"), Decision{Risk: Medium}, "shell_exec(make *)"},
		{"proc_start", exec("npm", "run", "dev"), Decision{Risk: Medium}, "proc_start(npm run *)"},
		{"fs_patch", jsonIn(map[string]any{"path": "a.go"}), Decision{Risk: Medium}, "fs_patch"},
		{"shell_exec", exec("git", "push"), Decision{Risk: High}, ""},
		{"fs_write", jsonIn(map[string]any{"path": "../x"}), Decision{Risk: Medium, Outside: true}, ""},
		{"fs_apply_patch", jsonIn(map[string]any{"patch": "x"}), Decision{Risk: Medium, Deletes: true}, ""},
		{"net_http", jsonIn(map[string]any{"url": "https://x"}), Decision{Risk: Medium}, ""},
	}
	for _, tt := range tests {
		got := SuggestRule(tt.tool, tt.in, tt.d)
		if got != tt.want {
			t.Errorf("SuggestRule(%s %s) = %q, want %q", tt.tool, tt.in, got, tt.want)
		}
		if got != "" {
			if _, err := ParseRule(got, RuleAllow, "", true); err != nil {
				t.Errorf("suggested rule %q does not parse: %v", got, err)
			}
		}
	}
}

func TestScopeExtraDirectories(t *testing.T) {
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	extra, _ := filepath.EvalSymlinks(t.TempDir())
	s := NewScope(ws, extra)
	for path, want := range map[string]bool{filepath.Join(ws, "a"): true, filepath.Join(extra, "lib", "x.go"): true, extra: true, filepath.Dir(ws): false} {
		if got := s.Contains(path); got != want {
			t.Errorf("Contains(%s) = %v, want %v", path, got, want)
		}
	}
	e := New(s, config.ApprovalAsk)
	if d := e.Decide("fs_write", jsonIn(map[string]any{"path": filepath.Join(extra, "x.go")})); d.Risk != Medium || d.Outside {
		t.Fatalf("writes in an additional directory are workspace writes: %+v", d)
	}
}
