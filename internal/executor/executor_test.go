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
