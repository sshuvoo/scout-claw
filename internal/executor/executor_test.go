package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"scoutclaw/internal/audit"
	"scoutclaw/internal/config"
	"scoutclaw/internal/policy"
	"scoutclaw/internal/redact"
	"scoutclaw/internal/tools"
	"scoutclaw/internal/tools/builtin"
	"scoutclaw/internal/verify"
)

type fakeApprover struct {
	answers []ApprovalResponse
	asked   []ApprovalRequest
}

func (f *fakeApprover) Approve(_ context.Context, r ApprovalRequest) (ApprovalResponse, error) {
	f.asked = append(f.asked, r)
	if len(f.answers) == 0 {
		return ApprovalResponse{}, nil
	}
	a := f.answers[0]
	f.answers = f.answers[1:]
	return a, nil
}

type env struct {
	x     *Executor
	ws    string
	state string
	ap    *fakeApprover
	tr    *verify.Tracker
}

func newEnv(t *testing.T, mode config.Approval, answers ...ApprovalResponse) *env {
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	state := t.TempDir()
	scope := policy.NewScope(ws)
	tr := &verify.Tracker{}
	reg, _ := builtin.New(builtin.Options{Scope: scope, BackupDir: filepath.Join(state, "bk"), SessionID: "t", MaxOutput: 4096, ToolTimeout: 10 * time.Second, Tracker: tr})
	red := redact.New("my-super-secret-api-key")
	lg, err := audit.Open(filepath.Join(state, "audit.jsonl"), red)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lg.Close() })
	ap := &fakeApprover{answers: answers}
	return &env{ws: ws, state: state, ap: ap, tr: tr, x: &Executor{Registry: reg, Policy: policy.New(scope, mode), Approver: ap, Audit: lg, Redactor: red,
		Tracker: tr, SessionID: "t", ToolTimeout: 10 * time.Second, MaxOutput: 4096}}
}

func call(name string, in any) Call {
	b, _ := json.Marshal(in)
	return Call{ID: "c1", Name: name, Input: b}
}

func TestLowRiskRunsWithoutPrompt(t *testing.T) {
	e := newEnv(t, config.ApprovalAsk)
	o := e.x.Run(context.Background(), call("shell_exec", map[string]any{"command": "echo", "args": []string{"hi"}}))
	if !o.OK() || len(e.ap.asked) != 0 || !strings.Contains(o.Text, "hi") || !strings.HasPrefix(o.Text, "status: ok") {
		t.Fatalf("%+v asked=%d", o, len(e.ap.asked))
	}
}

func TestUnknownToolAndBadArgsAreRecoverable(t *testing.T) {
	e := newEnv(t, config.ApprovalAsk)
	o := e.x.Run(context.Background(), call("rm_everything", map[string]any{}))
	if o.Result.Kind != tools.KindInvalidInput || !strings.Contains(o.Text, "Available tools") {
		t.Fatalf("%+v", o)
	}
	o = e.x.Run(context.Background(), Call{ID: "c", Name: "shell_exec", Input: json.RawMessage(`{"command":`)})
	if o.Result.Kind != tools.KindInvalidInput || !strings.Contains(o.Text, "Expected schema") {
		t.Fatalf("%+v", o)
	}
}

func TestMediumRiskNeedsApprovalAndDeclineBlocksExecution(t *testing.T) {
	e := newEnv(t, config.ApprovalAsk) // approver says no by default
	target := filepath.Join(e.ws, "f.txt")
	o := e.x.Run(context.Background(), call("fs_write", map[string]any{"path": "f.txt", "content": "x"}))
	if o.Result.Kind != tools.KindDenied || len(e.ap.asked) != 1 {
		t.Fatalf("%+v", o)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("declined write was performed")
	}
	if !strings.Contains(e.ap.asked[0].Preview, "+x") {
		t.Fatalf("approval must include a diff preview, got %q", e.ap.asked[0].Preview)
	}
}

func TestApprovedWriteRunsAndRememberSkipsNextPrompt(t *testing.T) {
	e := newEnv(t, config.ApprovalAsk, ApprovalResponse{Approved: true, Remember: true})
	o := e.x.Run(context.Background(), call("fs_write", map[string]any{"path": "a.txt", "content": "1"}))
	if !o.OK() || !o.Result.Mutated {
		t.Fatalf("%+v", o)
	}
	o = e.x.Run(context.Background(), call("fs_write", map[string]any{"path": "b.txt", "content": "2"}))
	if !o.OK() || len(e.ap.asked) != 1 {
		t.Fatalf("second write should not prompt: asked=%d %+v", len(e.ap.asked), o)
	}
	if len(e.tr.Changes()) != 2 {
		t.Fatalf("changes must be recorded: %v", e.tr.Changes())
	}
}

func TestHighRiskNeverAutoApprovedAndOutsideNeedsApproval(t *testing.T) {
	e := newEnv(t, config.ApprovalAuto)
	outside := filepath.Join(t.TempDir(), "o.txt")
	o := e.x.Run(context.Background(), call("fs_write", map[string]any{"path": outside, "content": "x"}))
	if o.Result.Kind != tools.KindDenied || len(e.ap.asked) != 1 || e.ap.asked[0].Decision.Risk != policy.High {
		t.Fatalf("high-risk write must prompt even in auto mode: %+v", o)
	}
	e.ap.answers = []ApprovalResponse{{Approved: true}}
	o = e.x.Run(context.Background(), call("fs_write", map[string]any{"path": outside, "content": "x"}))
	if !o.OK() {
		t.Fatalf("approved outside write should work: %+v", o)
	}
}

func TestReadOnlyModeDeniesWithoutPrompt(t *testing.T) {
	e := newEnv(t, config.ApprovalReadOnly)
	o := e.x.Run(context.Background(), call("fs_write", map[string]any{"path": "x", "content": "y"}))
	if o.Result.Kind != tools.KindDenied || len(e.ap.asked) != 0 {
		t.Fatalf("%+v", o)
	}
}

func TestForbiddenIsDeniedWithoutPrompt(t *testing.T) {
	e := newEnv(t, config.ApprovalAuto, ApprovalResponse{Approved: true})
	o := e.x.Run(context.Background(), call("shell_exec", map[string]any{"command": "rm", "args": []string{"-rf", "/"}}))
	if o.Result.Kind != tools.KindDenied || len(e.ap.asked) != 0 || !strings.Contains(o.Text, "denied by policy") {
		t.Fatalf("%+v", o)
	}
	o = e.x.Run(context.Background(), call("fs_read", map[string]any{"path": "/etc/shadow"}))
	if o.Result.Kind != tools.KindDenied {
		t.Fatalf("%+v", o)
	}
}

func TestSecretsRedactedInObservationAndAudit(t *testing.T) {
	e := newEnv(t, config.ApprovalAsk)
	os.WriteFile(filepath.Join(e.ws, ".env"), []byte("DB_PASSWORD=hunter2hunter2\nOTHER=my-super-secret-api-key\nPORT=80\n"), 0o600)
	o := e.x.Run(context.Background(), call("fs_read", map[string]any{"path": ".env"}))
	if strings.Contains(o.Text, "hunter2") || strings.Contains(o.Text, "my-super-secret") || !strings.Contains(o.Text, "PORT=80") {
		t.Fatalf("observation leaks or over-redacts: %q", o.Text)
	}
	e.x.Run(context.Background(), call("shell_exec", map[string]any{"command": "echo", "args": []string{"token=abcdef123456789"}}))
	b, _ := os.ReadFile(filepath.Join(e.state, "audit.jsonl"))
	if strings.Contains(string(b), "hunter2") || strings.Contains(string(b), "abcdef123456789") || strings.Contains(string(b), "my-super-secret") {
		t.Fatalf("audit log leaks secrets:\n%s", b)
	}
	if !strings.Contains(string(b), `"kind":"tool_result"`) || !strings.Contains(string(b), `"kind":"decision"`) {
		t.Fatalf("audit log is missing events:\n%s", b)
	}
}

func TestOutputIsBoundedForTheModel(t *testing.T) {
	e := newEnv(t, config.ApprovalAsk)
	e.x.MaxOutput = 1000
	o := e.x.Run(context.Background(), call("shell_exec", map[string]any{"command": "seq", "args": []string{"1", "5000"}}))
	if len(o.Text) > 1300 || !strings.Contains(o.Text, "omitted") {
		t.Fatalf("observation not bounded: %d bytes", len(o.Text))
	}
}

func TestIdenticalFailuresAreNotRepeated(t *testing.T) {
	e := newEnv(t, config.ApprovalAsk)
	c := call("shell_exec", map[string]any{"command": "false"})
	for i := 0; i < 2; i++ {
		if o := e.x.Run(context.Background(), c); o.Result.ExitCode != 1 {
			t.Fatalf("run %d: %+v", i, o)
		}
	}
	o := e.x.Run(context.Background(), c)
	if !strings.Contains(o.Text, "not executed") || o.OK() {
		t.Fatalf("third identical failure should be short-circuited: %q", o.Text)
	}
	// Different arguments are still allowed, and a new task resets the counter.
	if o := e.x.Run(context.Background(), call("shell_exec", map[string]any{"command": "true"})); !o.OK() {
		t.Fatalf("%+v", o)
	}
	e.x.ResetTask()
	if o := e.x.Run(context.Background(), c); strings.Contains(o.Text, "not executed") {
		t.Fatal("ResetTask should clear failure counts")
	}
}

func TestVerificationEvidenceRecorded(t *testing.T) {
	e := newEnv(t, config.ApprovalAuto)
	e.x.Run(context.Background(), call("shell_exec", map[string]any{"command": "touch", "args": []string{"made"}, "purpose": "change"}))
	if len(e.tr.Changes()) != 1 {
		t.Fatalf("change not recorded: %v", e.tr.Changes())
	}
	if a := e.tr.Assess(verify.Verified, true); a.Status != verify.Unverified || !a.Rejected {
		t.Fatalf("verified claim without evidence must be rejected: %+v", a)
	}
	e.x.Run(context.Background(), call("shell_exec", map[string]any{"command": "test", "args": []string{"-f", "made"}, "purpose": "verify"}))
	if a := e.tr.Assess(verify.Verified, true); a.Status != verify.Verified {
		t.Fatalf("%+v", a)
	}
	// a failing verification after the change flips it
	e.x.Run(context.Background(), call("shell_exec", map[string]any{"command": "test", "args": []string{"-f", "absent"}, "purpose": "verify"}))
	if a := e.tr.Assess(verify.Verified, false); a.Status != verify.Failed {
		t.Fatalf("%+v", a)
	}
}

func TestCancelledContextStopsToolAndDoesNotCountAsFailure(t *testing.T) {
	e := newEnv(t, config.ApprovalAsk)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	o := e.x.Run(ctx, call("shell_exec", map[string]any{"command": "sleep", "args": []string{"20"}}))
	if o.Result.Kind != tools.KindCancelled {
		t.Fatalf("%+v", o)
	}
	if e.x.failureCount(fingerprint(call("shell_exec", map[string]any{"command": "sleep", "args": []string{"20"}}))) != 0 {
		t.Fatal("cancellation must not count as a failure")
	}
}

func TestOutputLimitRaisesTheCapPerResult(t *testing.T) {
	e := newEnv(t, config.ApprovalAsk)
	big := strings.Repeat("x", 10000)
	tests := []struct {
		name      string
		res       tools.Result
		truncated bool
	}{
		{"default cap applies", tools.Result{Output: big}, true},
		{"larger per-result limit keeps everything", tools.Result{Output: big, OutputLimit: 20000}, false},
		{"smaller per-result limit cannot lower the cap", tools.Result{Output: strings.Repeat("x", 3000), OutputLimit: 100}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := strings.Contains(e.x.render(tt.res), "omitted by ScoutClaw")
			if got != tt.truncated {
				t.Fatalf("truncated = %v, want %v", got, tt.truncated)
			}
		})
	}
}

func TestEditsModeThroughExecutor(t *testing.T) {
	e := newEnv(t, config.ApprovalEdits, ApprovalResponse{Approved: false})
	ctx := context.Background()
	if o := e.x.Run(ctx, call("fs_write", map[string]any{"path": "a.txt", "content": "x"})); !o.OK() || len(e.ap.asked) != 0 {
		t.Fatalf("workspace write should run without a prompt: %+v asked=%d", o, len(e.ap.asked))
	}
	if o := e.x.Run(ctx, call("fs_delete", map[string]any{"path": "a.txt"})); o.OK() || len(e.ap.asked) != 1 {
		t.Fatalf("delete must ask (and was declined): %+v asked=%d", o, len(e.ap.asked))
	}
	if _, err := os.Stat(filepath.Join(e.ws, "a.txt")); err != nil {
		t.Fatal("declined delete removed the file")
	}
}

func TestPreviewFailureStopsBeforeApproval(t *testing.T) {
	e := newEnv(t, config.ApprovalAsk, ApprovalResponse{Approved: true})
	os.WriteFile(filepath.Join(e.ws, "unread.txt"), []byte("a\n"), 0o644)
	o := e.x.Run(context.Background(), call("fs_patch", map[string]any{"path": "unread.txt", "old_string": "a", "new_string": "b"}))
	if o.OK() || len(e.ap.asked) != 0 || !strings.Contains(o.Text, "has not been read") {
		t.Fatalf("a doomed edit must fail without asking: %+v asked=%d", o, len(e.ap.asked))
	}
}

func TestSummarizeFileTools(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"fs_lines", map[string]any{"path": "a.go"}, "fs_lines a.go"},
		{"fs_mkdir", map[string]any{"path": "d"}, "fs_mkdir d"},
		{"fs_changes", map[string]any{}, "fs_changes"},
		{"fs_copy", map[string]any{"src": "a", "dest": "b"}, "fs_copy a -> b"},
		{"fs_move", map[string]any{"src": "a", "dest": "b"}, "fs_move a -> b"},
		{"fs_delete", map[string]any{"path": "x"}, "fs_delete x"},
		{"fs_delete", map[string]any{"path": "x", "recursive": true}, "fs_delete -r x"},
		{"fs_undo", map[string]any{}, "fs_undo 1"},
		{"fs_undo", map[string]any{"steps": 3, "path": "src"}, "fs_undo 3 src"},
		{"fs_glob", map[string]any{"pattern": "*.go"}, "fs_glob *.go"},
		{"fs_apply_patch", map[string]any{"patch": "*** Begin Patch\n*** Update File: a.go\n-x\n+y\n*** Add File: b.go\n+z\n*** End Patch"}, "fs_apply_patch a.go b.go"},
		{"fs_apply_patch", map[string]any{"patch": "--- a/c.go\n+++ b/c.go\n"}, "fs_apply_patch c.go"},
		{"project_info", map[string]any{}, "project_info"},
		{"proc_start", map[string]any{"command": "npm", "args": []string{"run", "dev"}}, "proc_start npm run dev"},
		{"proc_output", map[string]any{"id": "p1"}, "proc_output p1"},
		{"proc_stop", map[string]any{"id": "p2"}, "proc_stop p2"},
		{"fs_grep", map[string]any{"pattern": "TODO", "path": "src"}, `fs_grep "TODO" src`},
		{"fs_read_many", map[string]any{"files": []map[string]any{{"path": "a"}, {"path": "b"}}}, "fs_read_many a b"},
	}
	for _, tt := range tests {
		if got := Summarize(call(tt.name, tt.in)); got != tt.want {
			t.Errorf("Summarize(%s %v) = %q, want %q", tt.name, tt.in, got, tt.want)
		}
	}
}

func TestPersistApprovalSavesSuggestedRule(t *testing.T) {
	var saved []string
	e := newEnv(t, config.ApprovalAsk, ApprovalResponse{Approved: true, Persist: true})
	e.x.PersistRule = func(rule string) error { saved = append(saved, rule); return nil }
	o := e.x.Run(context.Background(), call("fs_write", map[string]any{"path": "a.txt", "content": "x"}))
	if !o.OK() || len(e.ap.asked) != 1 || e.ap.asked[0].SuggestedRule != "fs_write" {
		t.Fatalf("outcome %+v, asked %+v", o, e.ap.asked)
	}
	if len(saved) != 1 || saved[0] != "fs_write" {
		t.Fatalf("saved = %v", saved)
	}
}

func TestNoSuggestedRuleWithoutPersistenceOrForHighRisk(t *testing.T) {
	e := newEnv(t, config.ApprovalAsk, ApprovalResponse{}, ApprovalResponse{})
	e.x.Run(context.Background(), call("fs_write", map[string]any{"path": "a.txt", "content": "x"}))
	if e.ap.asked[0].SuggestedRule != "" {
		t.Fatal("no PersistRule callback: nothing should be offered")
	}
	e.x.PersistRule = func(string) error { t.Fatal("must not persist"); return nil }
	e.x.Run(context.Background(), call("fs_write", map[string]any{"path": filepath.Join(t.TempDir(), "o.txt"), "content": "x"}))
	if e.ap.asked[1].SuggestedRule != "" {
		t.Fatal("high-risk actions must not offer an always-allow rule")
	}
}
