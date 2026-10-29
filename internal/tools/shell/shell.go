// Package shell provides bounded process execution. shell_exec runs one
// executable with explicit arguments (no shell involved); shell_script runs a
// script through /bin/sh and is always treated as high risk by the policy.
package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"scoutclaw/internal/policy"
	"scoutclaw/internal/tools"
)

type Runner struct {
	Scope          policy.Scope
	MaxOutput      int
	DefaultTimeout time.Duration // also the upper bound for per-call timeouts
	// StripEnv lists variables removed from the child environment (e.g. the model API key).
	StripEnv []string
}

type execArgs struct {
	Command    string   `json:"command"`
	Args       []string `json:"args"`
	Workdir    string   `json:"workdir"`
	TimeoutSec int      `json:"timeout_sec"`
	Purpose    string   `json:"purpose"`
}

type scriptArgs struct {
	Script     string `json:"script"`
	Workdir    string `json:"workdir"`
	TimeoutSec int    `json:"timeout_sec"`
	Purpose    string `json:"purpose"`
}

const purposeDoc = `"purpose":{"type":"string","enum":["inspect","change","verify"],"description":"inspect: gather facts. change: modify something. verify: check that a fix worked (build, test, status, health check). Verification evidence is judged from commands marked verify."}`

func (r Runner) Tools() []tools.Tool {
	return []tools.Tool{
		&tools.Typed[execArgs]{
			ToolName: "shell_exec",
			Desc: "Run one executable with explicit arguments (no shell, so no pipes, redirects, globbing or $VARS). " +
				"Output is stdout+stderr combined and truncated. Use it for diagnostics (ls, ps, df, git status, systemctl status, journalctl, docker ps), builds/tests, and changes. " +
				"Risky commands need user approval; credential files are never readable.",
			Schema: `{"type":"object","properties":{` +
				`"command":{"type":"string","description":"Executable name or path, e.g. \"df\"."},` +
				`"args":{"type":"array","items":{"type":"string"},"description":"Arguments, one per element."},` +
				`"workdir":{"type":"string","description":"Working directory (default: workspace)."},` +
				`"timeout_sec":{"type":"integer","description":"Timeout in seconds (capped by configuration)."},` +
				purposeDoc + `},"required":["command"],"additionalProperties":false}`,
			ValidateFn: func(a *execArgs) error {
				if strings.TrimSpace(a.Command) == "" {
					return errors.New("command is required")
				}
				if strings.ContainsRune(a.Command, 0) {
					return errors.New("command contains NUL")
				}
				return validatePurpose(a.Purpose, a.TimeoutSec)
			},
			Run: func(ctx context.Context, a execArgs) (tools.Result, error) {
				path, err := exec.LookPath(a.Command)
				if err != nil {
					return tools.Result{}, tools.Errorf(tools.KindUnavailable, "executable %q not found in PATH or not executable: %v", a.Command, err)
				}
				return r.run(ctx, path, a.Args, a.Workdir, a.TimeoutSec, a.Purpose)
			},
		},
		&tools.Typed[scriptArgs]{
			ToolName: "shell_script",
			Desc: "Run a multi-line shell script with /bin/sh -c. Only use when shell syntax (pipes, redirects, loops) is genuinely required; " +
				"it is always high risk and requires explicit user approval. Prefer shell_exec.",
			Schema: `{"type":"object","properties":{` +
				`"script":{"type":"string"},` +
				`"workdir":{"type":"string"},` +
				`"timeout_sec":{"type":"integer"},` +
				purposeDoc + `},"required":["script"],"additionalProperties":false}`,
			ValidateFn: func(a *scriptArgs) error {
				if strings.TrimSpace(a.Script) == "" {
					return errors.New("script is required")
				}
				if runtime.GOOS == "windows" {
					return errors.New("shell_script is not supported on windows")
				}
				return validatePurpose(a.Purpose, a.TimeoutSec)
			},
			Run: func(ctx context.Context, a scriptArgs) (tools.Result, error) {
				return r.run(ctx, "/bin/sh", []string{"-c", a.Script}, a.Workdir, a.TimeoutSec, a.Purpose)
			},
		},
	}
}

func validatePurpose(p string, timeout int) error {
	switch p {
	case "", "inspect", "change", "verify":
	default:
		return fmt.Errorf("purpose must be inspect, change or verify, got %q", p)
	}
	if timeout < 0 {
		return errors.New("timeout_sec must be >= 0")
	}
	return nil
}

func (r Runner) run(ctx context.Context, path string, args []string, workdir string, timeoutSec int, purpose string) (tools.Result, error) {
	dir := r.Scope.Root
	if workdir != "" {
		dir = r.Scope.Resolve(workdir, "")
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return tools.Result{}, tools.Errorf(tools.KindInvalidInput, "workdir %q is not a directory", dir)
	}
	timeout := r.DefaultTimeout
	if timeoutSec > 0 && time.Duration(timeoutSec)*time.Second < timeout {
		timeout = time.Duration(timeoutSec) * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, path, args...)
	cmd.Dir = dir
	cmd.Env = r.childEnv()
	configureProcess(cmd)
	buf := newHeadTail(r.MaxOutput)
	cmd.Stdout, cmd.Stderr = buf, buf

	start := time.Now()
	runErr := cmd.Run()
	dur := time.Since(start)

	res := tools.Result{
		Output:    buf.String(),
		Truncated: buf.Truncated(),
		Metadata: map[string]string{
			"duration": dur.Round(time.Millisecond).String(),
			"workdir":  dir,
			"purpose":  purpose,
		},
	}
	// Anything that is not a clearly read-only purpose may have changed state.
	res.Mutated = purpose == "change"
	switch {
	case runErr == nil:
		return res, nil
	case errors.Is(cctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
		res.Kind, res.ExitCode = tools.KindTimeout, -1
		res.Output += fmt.Sprintf("\n[timed out after %s; process group killed]", timeout)
		return res, nil
	case ctx.Err() != nil:
		res.Kind, res.ExitCode = tools.KindCancelled, -1
		res.Output += "\n[cancelled; process group killed]"
		return res, nil
	}
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		res.Kind, res.ExitCode = tools.KindFailed, ee.ExitCode()
		if res.ExitCode < 0 { // killed by signal
			res.ExitCode = 128
			res.Output += "\n[terminated by signal: " + ee.String() + "]"
		}
		return res, nil
	}
	return res, tools.Errorf(tools.KindUnavailable, "could not start %s: %v", filepath.Base(path), runErr)
}

func (r Runner) childEnv() []string {
	drop := map[string]bool{}
	for _, k := range r.StripEnv {
		drop[k] = true
	}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !drop[k] {
			env = append(env, kv)
		}
	}
	// Non-interactive, colourless output.
	return append(env, "TERM=dumb", "NO_COLOR=1", "PAGER=cat", "GIT_PAGER=cat", "GIT_TERMINAL_PROMPT=0", "DEBIAN_FRONTEND=noninteractive")
}

// headTail keeps the first and last halves of the output when it exceeds the limit.
type headTail struct {
	limit   int
	head    []byte
	tail    []byte
	dropped int
}

func newHeadTail(limit int) *headTail { return &headTail{limit: limit} }

func (h *headTail) Write(p []byte) (int, error) {
	n := len(p)
	half := h.limit / 2
	if len(h.head) < half {
		take := min(half-len(h.head), len(p))
		h.head = append(h.head, p[:take]...)
		p = p[take:]
	}
	if len(p) > 0 {
		h.tail = append(h.tail, p...)
		if over := len(h.tail) - half; over > 0 {
			h.tail = h.tail[over:]
			h.dropped += over
		}
	}
	return n, nil
}

func (h *headTail) Truncated() bool { return h.dropped > 0 }

func (h *headTail) String() string {
	if h.dropped == 0 {
		return string(bytes.ToValidUTF8(append(append([]byte{}, h.head...), h.tail...), []byte("?")))
	}
	return string(bytes.ToValidUTF8(h.head, []byte("?"))) +
		fmt.Sprintf("\n... [%d bytes omitted] ...\n", h.dropped) +
		string(bytes.ToValidUTF8(h.tail, []byte("?")))
}
