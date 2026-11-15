// Package shell provides bounded process execution. shell_exec runs one
// executable with explicit arguments (no shell involved). shell_script runs an
// embedded Bash-compatible interpreter and portable core utilities, while
// shell_powershell runs a PowerShell script (Windows only); both are always
// treated as high risk by the policy.
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
	"sync"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
	"mvdan.cc/sh/x/coreutils"
	"scoutclaw/internal/policy"
	"scoutclaw/internal/tools"
)

type Runner struct {
	Scope          policy.Scope
	MaxOutput      int
	DefaultTimeout time.Duration // also the upper bound for per-call timeouts
	// StripEnv lists variables removed from the child environment (e.g. the model API key).
	StripEnv   []string
	PowerShell string
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

const scriptSchema = `{"type":"object","properties":{` +
	`"script":{"type":"string"},` +
	`"workdir":{"type":"string"},` +
	`"timeout_sec":{"type":"integer"},` +
	purposeDoc + `},"required":["script"],"additionalProperties":false}`

// powershellPrelude makes PowerShell output UTF-8 and suppresses progress
// records, which would otherwise be serialised as CLIXML noise.
const powershellPrelude = "$ProgressPreference='SilentlyContinue';[Console]::OutputEncoding=[System.Text.Encoding]::UTF8;$OutputEncoding=[System.Text.Encoding]::UTF8;\n"

func (r Runner) Tools() []tools.Tool {
	ts := []tools.Tool{
		&tools.Typed[execArgs]{
			ToolName: "shell_exec",
			Desc: "Run one executable with explicit arguments (no shell, so no pipes, redirects, globbing or $VARS). " +
				"Output is stdout+stderr combined and truncated. Use it for diagnostics, builds/tests, and changes. " +
				"Risky commands need user approval; credential files are never readable.",
			Schema: `{"type":"object","properties":{` +
				`"command":{"type":"string","description":"Executable name or path, e.g. \"git\"."},` +
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
				if err := checkBatchArgs(path, a.Args); err != nil {
					return tools.Result{}, err
				}
				return r.run(ctx, path, a.Args, a.Workdir, a.TimeoutSec, a.Purpose)
			},
		},
	}
	ts = append(ts, &tools.Typed[scriptArgs]{
		ToolName: "shell_script",
		Desc: "Run a multi-line POSIX/Bash-compatible script using ScoutClaw's embedded cross-platform shell. Common commands such as ls, touch, cat, cp, mv, mkdir and rm are available on every OS. " +
			"Scripts are always high risk and require explicit user approval. Prefer shell_exec for a single program.",
		Schema:     scriptSchema,
		ValidateFn: validateScript,
		Run: func(ctx context.Context, a scriptArgs) (tools.Result, error) {
			return r.runScript(ctx, a.Script, a.Workdir, a.TimeoutSec, a.Purpose)
		},
	})
	if r.PowerShell != "" {
		ts = append(ts, &tools.Typed[scriptArgs]{
			ToolName: "shell_powershell",
			Desc: "Run a PowerShell script (" + filepath.Base(r.PowerShell) + " -NoProfile -NonInteractive -Command). Use it for Windows administration " +
				"(services, processes, registry, event logs, packages) and anything needing PowerShell syntax. " +
				"It is always high risk and requires explicit user approval. Prefer shell_exec for a single program.",
			Schema:     scriptSchema,
			ValidateFn: validateScript,
			Run: func(ctx context.Context, a scriptArgs) (tools.Result, error) {
				return r.run(ctx, r.PowerShell, []string{"-NoProfile", "-NonInteractive", "-Command", powershellPrelude + a.Script}, a.Workdir, a.TimeoutSec, a.Purpose)
			},
		})
	}
	return ts
}

// runScript executes a script in the embedded shell. The interpreter and
// portable core utilities are bundled into ScoutClaw, so this tool is
// available even when no system shell is installed.
func (r Runner) runScript(ctx context.Context, script, workdir string, timeoutSec int, purpose string) (tools.Result, error) {
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

	buf := newHeadTail(r.MaxOutput)
	w := &lockedWriter{w: buf}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(script), "")
	if err != nil {
		return tools.Result{}, tools.Errorf(tools.KindInvalidInput, "invalid shell script: %v", err)
	}
	sh, err := interp.New(
		interp.Dir(dir),
		interp.Env(expand.ListEnviron(r.childEnv()...)),
		interp.StdIO(nil, w, w),
		interp.ExecHandlers(coreutils.ExecHandler, r.execHandler),
	)
	if err != nil {
		return tools.Result{}, tools.Errorf(tools.KindInvalidInput, "could not initialize embedded shell: %v", err)
	}
	start := time.Now()
	runErr := sh.Run(cctx, file)
	dur := time.Since(start)
	res := tools.Result{
		Output: buf.String(), Truncated: buf.Truncated(),
		Metadata: map[string]string{"duration": dur.Round(time.Millisecond).String(), "workdir": dir, "purpose": purpose},
		Mutated:  purpose == "change",
	}
	switch {
	case runErr == nil:
		return res, nil
	case errors.Is(cctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
		res.Kind, res.ExitCode = tools.KindTimeout, -1
		res.Output += fmt.Sprintf("\n[timed out after %s; process tree killed]", timeout)
		return res, nil
	case ctx.Err() != nil:
		res.Kind, res.ExitCode = tools.KindCancelled, -1
		res.Output += "\n[cancelled; process tree killed]"
		return res, nil
	default:
		var status interp.ExitStatus
		if errors.As(runErr, &status) {
			res.Kind, res.ExitCode = tools.KindFailed, int(status)
			return res, nil
		}
		return res, tools.Errorf(tools.KindFailed, "embedded shell failed: %v", runErr)
	}
}

// execHandler runs non-coreutils programs with the same process-tree
// cancellation behavior as shell_exec.
func (r Runner) execHandler(_ interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		hc := interp.HandlerCtx(ctx)
		if len(args) == 0 {
			return nil
		}
		path, err := interp.LookPathDir(hc.Dir, hc.Env, args[0])
		if err != nil {
			fmt.Fprintln(hc.Stderr, err)
			return interp.ExitStatus(127)
		}
		if err := checkBatchArgs(path, args[1:]); err != nil {
			fmt.Fprintln(hc.Stderr, err)
			return interp.ExitStatus(126)
		}
		cmd := exec.CommandContext(ctx, path)
		cmd.Args = args
		cmd.Dir = hc.Dir
		cmd.Env = shellEnv(hc.Env)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = hc.Stdin, hc.Stdout, hc.Stderr
		tree := configureProcess(cmd)
		defer tree.release()
		if err := cmd.Start(); err != nil {
			fmt.Fprintln(hc.Stderr, err)
			return interp.ExitStatus(127)
		}
		tree.attach(cmd)
		if err := cmd.Wait(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code := ee.ExitCode()
				if code < 0 {
					code = 128
				}
				return interp.ExitStatus(uint8(code))
			}
			fmt.Fprintln(hc.Stderr, err)
			return interp.ExitStatus(127)
		}
		return nil
	}
}

func shellEnv(env expand.Environ) []string {
	var out []string
	indexes := make(map[string]int)
	normalize := func(name string) string {
		if runtime.GOOS == "windows" {
			return strings.ToUpper(name)
		}
		return name
	}
	env.Each(func(name string, v expand.Variable) bool {
		if v.Exported && v.Set {
			pair := name + "=" + v.Str
			key := normalize(name)
			if i, ok := indexes[key]; ok {
				out[i] = pair
			} else {
				indexes[key] = len(out)
				out = append(out, pair)
			}
		}
		return true
	})
	return out
}

// Interpreter output can be written from more than one process when scripts
// use background commands. Protect the bounded writer from concurrent writes.
type lockedWriter struct {
	mu sync.Mutex
	w  *headTail
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}

func validateScript(a *scriptArgs) error {
	if strings.TrimSpace(a.Script) == "" {
		return errors.New("script is required")
	}
	return validatePurpose(a.Purpose, a.TimeoutSec)
}

// checkBatchArgs refuses arguments that cmd.exe would re-interpret. Windows
// runs .bat/.cmd files through cmd.exe, which parses the command line again,
// so metacharacters in an argument could run other commands.
func checkBatchArgs(path string, args []string) error {
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".bat" && ext != ".cmd" {
		return nil
	}
	for _, a := range args {
		if strings.ContainsAny(a, "&|<>^%!\"\r\n") {
			return tools.Errorf(tools.KindInvalidInput, "%s is a batch file run by cmd.exe; argument %q contains cmd metacharacters that cannot be passed safely. Use shell_powershell instead", filepath.Base(path), a)
		}
	}
	return nil
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
	tree := configureProcess(cmd)
	defer tree.release()
	buf := newHeadTail(r.MaxOutput)
	cmd.Stdout, cmd.Stderr = buf, buf

	start := time.Now()
	runErr := cmd.Start()
	if runErr == nil {
		tree.attach(cmd)
		runErr = cmd.Wait()
	}
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
		res.Output += fmt.Sprintf("\n[timed out after %s; process tree killed]", timeout)
		return res, nil
	case ctx.Err() != nil:
		res.Kind, res.ExitCode = tools.KindCancelled, -1
		res.Output += "\n[cancelled; process tree killed]"
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
	// Variable names are case-insensitive on Windows.
	norm := func(k string) string {
		if runtime.GOOS == "windows" {
			return strings.ToUpper(k)
		}
		return k
	}
	drop := map[string]bool{}
	for _, k := range r.StripEnv {
		drop[norm(k)] = true
	}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !drop[norm(k)] {
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
