package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"scoutclaw/internal/audit"
	"scoutclaw/internal/config"
	"scoutclaw/internal/executor"
	"scoutclaw/internal/llm"
	"scoutclaw/internal/policy"
	"scoutclaw/internal/redact"
	"scoutclaw/internal/tools/builtin"
	"scoutclaw/internal/tools/task"
	"scoutclaw/internal/verify"
)

type rig struct {
	a    *Agent
	llm  *llm.Scripted
	ws   string
	logs []string
}

type reporter struct{ r *rig }

func (p reporter) Delta(s string)               { p.r.logs = append(p.r.logs, "delta:"+s) }
func (p reporter) Plan(s []task.Step, h string) {}
func (p reporter) ToolStart(s string)           { p.r.logs = append(p.r.logs, "start:"+s) }
func (p reporter) ToolEnd(o executor.Outcome)   { p.r.logs = append(p.r.logs, "end:"+o.Call.Name) }
func (p reporter) Notice(string)                {}

func newRig(t *testing.T, mode config.Approval, steps ...func(llm.Request) (*llm.Response, error)) *rig {
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	state := t.TempDir()
	scope := policy.NewScope(ws)
	tr := &verify.Tracker{}
	reg, st := builtin.New(builtin.Options{Scope: scope, BackupDir: filepath.Join(state, "bk"), SessionID: "t", MaxOutput: 4096, ToolTimeout: 10 * time.Second, Tracker: tr})
	red := redact.New()
	lg, _ := audit.Open(filepath.Join(state, "a.jsonl"), red)
	t.Cleanup(func() { lg.Close() })
	x := &executor.Executor{Registry: reg, Policy: policy.New(scope, mode), Approver: executor.DenyAll{}, Audit: lg, Redactor: red,
		Tracker: tr, SessionID: "t", ToolTimeout: 10 * time.Second, MaxOutput: 4096}
	fake := &llm.Scripted{Steps: steps}
	r := &rig{llm: fake, ws: ws}
	r.a = &Agent{LLM: fake, Exec: x, Registry: reg, State: st, Audit: lg, SessionID: "t",
		Limits: Limits{MaxSteps: 10, TaskTimeout: time.Minute, MaxConsecutiveFails: 3}, Reporter: reporter{r}}
	return r
}

func tc(id, name string, args any) llm.ToolCall {
	b, _ := json.Marshal(args)
	return llm.ToolCall{ID: id, Name: name, Arguments: string(b)}
}

func TestMultiStepFixAndVerify(t *testing.T) {
	var r *rig
	r = newRig(t, config.ApprovalAuto,
		llm.Calls(tc("1", "fs_read", map[string]any{"path": "app.conf"})),
		func(req llm.Request) (*llm.Response, error) {
			if len(req.ToolOutputs) != 1 || !strings.Contains(req.ToolOutputs[0].Output, "port=80") {
				t.Errorf("model did not receive the file contents: %+v", req.ToolOutputs)
			}
			return &llm.Response{ToolCalls: []llm.ToolCall{tc("2", "fs_patch", map[string]any{"path": "app.conf", "old_string": "port=80", "new_string": "port=8080"})}}, nil
		},
		llm.Calls(tc("3", "shell_exec", map[string]any{"command": "grep", "args": []string{"-q", "port=8080", "app.conf"}, "purpose": "verify"})),
		llm.Calls(tc("4", "finish", map[string]any{"summary": "fixed port", "root_cause": "wrong port", "changes": []string{"app.conf"}, "status": "verified"})),
	)
	os.WriteFile(filepath.Join(r.ws, "app.conf"), []byte("port=80\n"), 0o644)
	res := r.a.Run(context.Background(), "fix the port")
	if res.Status != StatusCompleted || res.Verify != verify.Verified || res.Report == nil || res.Steps != 4 {
		t.Fatalf("%+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(r.ws, "app.conf")); string(b) != "port=8080\n" {
		t.Fatalf("file not fixed: %q", b)
	}
	if !strings.Contains(strings.Join(r.logs, "|"), "start:grep -q port=8080 app.conf") {
		t.Fatalf("logs: %v", r.logs)
	}
	// Conversation continuity: each request after the first references the previous response.
	for i, q := range r.llm.Requests[1:] {
		if q.PreviousID == "" {
			t.Errorf("request %d lost PreviousID", i+1)
		}
	}
	if !strings.Contains(r.llm.Requests[0].UserText, "fix the port") || len(r.llm.Requests[0].Tools) < 10 {
		t.Fatalf("first request malformed: %+v", r.llm.Requests[0])
	}
}

func TestUnverifiedClaimIsRejectedThenDowngraded(t *testing.T) {
	var r *rig
	r = newRig(t, config.ApprovalAuto,
		llm.Calls(tc("1", "fs_write", map[string]any{"path": "x.txt", "content": "hi"})),
		llm.Calls(tc("2", "finish", map[string]any{"summary": "done", "status": "verified"})),
		func(req llm.Request) (*llm.Response, error) {
			if !strings.Contains(req.ToolOutputs[0].Output, "finish rejected") {
				t.Errorf("model should be told why finish was rejected: %q", req.ToolOutputs[0].Output)
			}
			return &llm.Response{ToolCalls: []llm.ToolCall{tc("3", "finish", map[string]any{"summary": "done anyway", "status": "verified"})}}, nil
		},
	)
	res := r.a.Run(context.Background(), "write x")
	if res.Status != StatusCompleted || res.Verify != verify.Unverified {
		t.Fatalf("claim must be downgraded: %+v", res)
	}
	if res.Report == nil || len(res.Report.Notes) == 0 || !strings.Contains(res.Report.Notes[0], "downgraded") {
		t.Fatalf("report must say it was downgraded: %+v", res.Report)
	}
}

func TestReplansAfterToolFailureWithoutRepeating(t *testing.T) {
	var r *rig
	r = newRig(t, config.ApprovalAuto,
		llm.Calls(tc("1", "shell_exec", map[string]any{"command": "cat", "args": []string{"missing.log"}})),
		func(req llm.Request) (*llm.Response, error) {
			out := req.ToolOutputs[0].Output
			if !strings.Contains(out, "failed (exit 1)") || !strings.Contains(out, "No such file") {
				t.Errorf("failure details not passed back: %q", out)
			}
			return &llm.Response{ToolCalls: []llm.ToolCall{tc("2", "fs_search", map[string]any{"name_glob": "*.log"})}}, nil
		},
		llm.Reply("The log is named differently; no changes were made."),
	)
	res := r.a.Run(context.Background(), "read the log")
	if res.Status != StatusCompleted || res.Verify != verify.NotNeeded || !strings.Contains(res.Text, "no changes") {
		t.Fatalf("%+v", res)
	}
}

func TestMalformedAndUnknownToolCallsAreRecoverable(t *testing.T) {
	r := newRig(t, config.ApprovalAuto,
		llm.Calls(llm.ToolCall{ID: "1", Name: "shell_exec", Arguments: `{"command":`}, tc("2", "launch_missiles", map[string]any{})),
		func(req llm.Request) (*llm.Response, error) {
			if len(req.ToolOutputs) != 2 || !strings.Contains(req.ToolOutputs[0].Output, "invalid_input") || !strings.Contains(req.ToolOutputs[1].Output, "unknown tool") {
				t.Errorf("both bad calls need an error observation: %+v", req.ToolOutputs)
			}
			return &llm.Response{Text: "recovered"}, nil
		},
	)
	res := r.a.Run(context.Background(), "x")
	if res.Status != StatusCompleted || res.Text != "recovered" {
		t.Fatalf("%+v", res)
	}
}

func TestIterationLimitStopsWithHonestWrapUp(t *testing.T) {
	r := newRig(t, config.ApprovalAuto)
	r.a.Limits.MaxSteps = 3
	for i := 0; i < 3; i++ {
		id := string(rune('a' + i))
		r.llm.Steps = append(r.llm.Steps, llm.Calls(tc(id, "sys_info", map[string]any{})))
	}
	r.llm.Steps = append(r.llm.Steps, func(req llm.Request) (*llm.Response, error) {
		if len(req.Tools) != 0 || !strings.Contains(req.UserText, "STOP") || len(req.ToolOutputs) != 1 {
			t.Errorf("wrap-up must be tool-less and deliver pending outputs: %+v", req)
		}
		return &llm.Response{Text: "Only partial inspection done."}, nil
	})
	res := r.a.Run(context.Background(), "loop forever")
	if res.Status != StatusLimit || res.Steps != 3 || !strings.Contains(res.Text, "partial") || res.Err == nil {
		t.Fatalf("%+v", res)
	}
	if len(r.llm.Requests) != 4 {
		t.Fatalf("expected exactly 3 steps + 1 wrap-up, got %d requests", len(r.llm.Requests))
	}
	if len(r.a.Carry) != 0 {
		t.Fatal("delivered outputs must not be carried over")
	}
}

func TestRepeatedFailuresStopTheLoop(t *testing.T) {
	r := newRig(t, config.ApprovalAuto)
	for i := 0; i < 3; i++ {
		r.llm.Steps = append(r.llm.Steps, llm.Calls(tc(string(rune('a'+i)), "shell_exec", map[string]any{"command": "false", "args": []string{string(rune('0' + i))}})))
	}
	r.llm.Steps = append(r.llm.Steps, llm.Reply("Blocked: every attempt failed."))
	res := r.a.Run(context.Background(), "do it")
	if res.Status != StatusStuck || res.Steps != 3 {
		t.Fatalf("%+v", res)
	}
}

func TestProviderErrorIsReported(t *testing.T) {
	boom := &llm.Error{Kind: llm.ErrAuth, Status: 401, Err: errors.New("bad key")}
	r := newRig(t, config.ApprovalAuto, func(llm.Request) (*llm.Response, error) { return nil, boom })
	res := r.a.Run(context.Background(), "x")
	if res.Status != StatusError || !errors.Is(res.Err, boom) {
		t.Fatalf("%+v", res)
	}
}

func TestEmptyResponsesAreBounded(t *testing.T) {
	r := newRig(t, config.ApprovalAuto, llm.Reply(""), llm.Reply(""), llm.Reply(""))
	res := r.a.Run(context.Background(), "x")
	if res.Status != StatusError || res.Err == nil {
		t.Fatalf("%+v", res)
	}
}

func TestCancellationTerminatesCommandsAndCarriesOutputs(t *testing.T) {
	r := newRig(t, config.ApprovalAuto,
		llm.Calls(tc("1", "shell_exec", map[string]any{"command": "sleep", "args": []string{"30"}}), tc("2", "sys_info", map[string]any{})),
	)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	start := time.Now()
	res := r.a.Run(ctx, "sleepy")
	if res.Status != StatusCancelled || time.Since(start) > 6*time.Second {
		t.Fatalf("%+v after %s", res, time.Since(start))
	}
	// The provider still expects outputs for both calls; they are delivered with the next task.
	if len(r.a.Carry) != 2 {
		t.Fatalf("carry: %+v", r.a.Carry)
	}
	r.llm.Steps = append(r.llm.Steps, func(req llm.Request) (*llm.Response, error) {
		if len(req.ToolOutputs) != 2 || req.PreviousID == "" || req.UserText != "next task" {
			t.Errorf("follow-up request inconsistent: %+v", req)
		}
		return &llm.Response{Text: "ok"}, nil
	})
	if res := r.a.Run(context.Background(), "next task"); res.Status != StatusCompleted {
		t.Fatalf("%+v", res)
	}
}

func TestDeclinedApprovalIsReportedToModel(t *testing.T) {
	r := newRig(t, config.ApprovalAsk,
		llm.Calls(tc("1", "fs_write", map[string]any{"path": "x", "content": "y"})),
		func(req llm.Request) (*llm.Response, error) {
			if !strings.Contains(req.ToolOutputs[0].Output, "declined") {
				t.Errorf("%q", req.ToolOutputs[0].Output)
			}
			return &llm.Response{Text: "I could not write the file because you declined."}, nil
		},
	)
	res := r.a.Run(context.Background(), "write")
	if res.Status != StatusCompleted || res.Verify != verify.NotNeeded {
		t.Fatalf("%+v", res)
	}
	if _, err := os.Stat(filepath.Join(r.ws, "x")); err == nil {
		t.Fatal("declined action executed")
	}
}

func TestPlainAnswerAfterChangesIsMarkedUnverified(t *testing.T) {
	r := newRig(t, config.ApprovalAuto,
		llm.Calls(tc("1", "fs_write", map[string]any{"path": "x", "content": "y"})),
		llm.Reply("All fixed!"),
	)
	res := r.a.Run(context.Background(), "x")
	if res.Verify != verify.Unverified {
		t.Fatalf("a confident sentence is not evidence: %+v", res)
	}
}
