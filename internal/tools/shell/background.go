package shell

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"scoutclaw/internal/tools"
)

// Background runs long-lived processes (dev servers, watchers) that must keep
// running across tool calls. Processes belong to the session: Shutdown, called
// when ScoutClaw exits or the runtime is rebuilt, stops every one of them.
type Background struct {
	Runner     Runner
	MaxRunning int // concurrent running processes (default 8)
	BufferSize int // bytes of output kept per process (default 256 KiB)

	mu    sync.Mutex
	procs map[string]*bgProc
	seq   int
}

type bgProc struct {
	id, name string
	argv     []string
	dir      string
	started  time.Time
	cancel   context.CancelFunc
	done     chan struct{}

	mu       sync.Mutex
	out      ring
	readPos  int64 // total bytes already returned by proc_output
	exitCode int
	exitErr  string
}

// ring keeps the last cap bytes of a stream and how many bytes were written in total.
type ring struct {
	cap   int
	data  []byte
	total int64
}

func (r *ring) write(p []byte) {
	r.total += int64(len(p))
	r.data = append(r.data, p...)
	if over := len(r.data) - r.cap; over > 0 {
		r.data = r.data[over:]
	}
}

// since returns output written after position pos, and how many of those bytes
// were already dropped from the buffer.
func (r *ring) since(pos int64) (string, int64) {
	start := r.total - int64(len(r.data))
	dropped := int64(0)
	if pos < start {
		dropped, pos = start-pos, start
	}
	return string(r.data[pos-start:]), dropped
}

// procWriter feeds a process's stdout and stderr into its ring buffer.
type procWriter struct{ p *bgProc }

func (w procWriter) Write(b []byte) (int, error) {
	w.p.mu.Lock()
	w.p.out.write(b)
	w.p.mu.Unlock()
	return len(b), nil
}

func (p *bgProc) running() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *bgProc) status() string {
	if p.running() {
		return fmt.Sprintf("running (%s)", time.Since(p.started).Round(time.Second))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exitErr != "" {
		return "exited: " + p.exitErr
	}
	return fmt.Sprintf("exited with code %d", p.exitCode)
}

type procStartArgs struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Workdir string   `json:"workdir"`
	Name    string   `json:"name"`
	WaitFor string   `json:"wait_for"`
	WaitSec int      `json:"wait_sec"`
}

type procOutputArgs struct {
	ID      string `json:"id"`
	WaitFor string `json:"wait_for"`
	WaitSec int    `json:"wait_sec"`
	All     bool   `json:"all"`
}

type procIDArgs struct {
	ID string `json:"id"`
}

const maxWaitSec = 120

func validateWait(pattern string, sec int) error {
	if sec < 0 || sec > maxWaitSec {
		return fmt.Errorf("wait_sec must be 0-%d", maxWaitSec)
	}
	if pattern != "" {
		if _, err := regexp.Compile(pattern); err != nil {
			return fmt.Errorf("bad wait_for pattern: %v", err)
		}
	}
	return nil
}

func (b *Background) Tools() []tools.Tool {
	return []tools.Tool{
		&tools.Typed[procStartArgs]{
			ToolName: "proc_start",
			Desc: "Start a long-running program in the background (dev server, file watcher, database) and return at once with an id; use shell_exec for commands that finish. " +
				"No shell: one executable with arguments. Optionally wait up to wait_sec seconds for output matching wait_for (e.g. \"listening on|ready\"). " +
				"Read its output with proc_output and stop it with proc_stop; ScoutClaw stops all background processes when it exits.",
			Schema: `{"type":"object","properties":{"command":{"type":"string"},"args":{"type":"array","items":{"type":"string"}},` +
				`"workdir":{"type":"string","description":"Working directory (default: workspace)."},"name":{"type":"string","description":"Short label, e.g. \"web\"."},` +
				`"wait_for":{"type":"string","description":"Regex that signals readiness in the output."},"wait_sec":{"type":"integer","description":"Seconds to wait for wait_for or for early exit (default 5, at most 120)."}},` +
				`"required":["command"],"additionalProperties":false}`,
			ValidateFn: func(a *procStartArgs) error {
				if strings.TrimSpace(a.Command) == "" {
					return errors.New("command is required")
				}
				if strings.ContainsRune(a.Command, 0) {
					return errors.New("command contains NUL")
				}
				return validateWait(a.WaitFor, a.WaitSec)
			},
			Run: b.start,
		},
		&tools.Typed[procOutputArgs]{
			ToolName: "proc_output",
			Desc:     "Show a background process's status and the output it produced since the last proc_output call (all=true: everything still buffered). Optionally wait up to wait_sec seconds for output matching wait_for.",
			Schema: `{"type":"object","properties":{"id":{"type":"string"},"wait_for":{"type":"string"},"wait_sec":{"type":"integer"},"all":{"type":"boolean"}},` +
				`"required":["id"],"additionalProperties":false}`,
			ValidateFn: func(a *procOutputArgs) error {
				if a.ID == "" {
					return errors.New("id is required")
				}
				return validateWait(a.WaitFor, a.WaitSec)
			},
			Run: b.output,
		},
		&tools.Typed[procIDArgs]{
			ToolName: "proc_stop",
			Desc:     "Stop a background process started with proc_start, together with every process it spawned, and show its final output.",
			Schema:   `{"type":"object","properties":{"id":{"type":"string"}},"required":["id"],"additionalProperties":false}`,
			ValidateFn: func(a *procIDArgs) error {
				if a.ID == "" {
					return errors.New("id is required")
				}
				return nil
			},
			Run: b.stop,
		},
		&tools.Typed[struct{}]{
			ToolName: "proc_list",
			Desc:     "List the background processes started in this session with their status.",
			Schema:   `{"type":"object","properties":{},"additionalProperties":false}`,
			Run: func(ctx context.Context, _ struct{}) (tools.Result, error) {
				return tools.Result{Output: b.List()}, nil
			},
		},
	}
}

func (b *Background) get(id string) (*bgProc, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.procs[id]
	if p == nil {
		return nil, tools.Errorf(tools.KindNotFound, "no background process %q; proc_list shows them", id)
	}
	return p, nil
}

func (b *Background) start(ctx context.Context, a procStartArgs) (tools.Result, error) {
	path, err := exec.LookPath(a.Command)
	if err != nil {
		return tools.Result{}, tools.Errorf(tools.KindUnavailable, "executable %q not found in PATH or not executable: %v", a.Command, err)
	}
	if err := checkBatchArgs(path, a.Args); err != nil {
		return tools.Result{}, err
	}
	dir := b.Runner.Scope.Root
	if a.Workdir != "" {
		dir = b.Runner.Scope.Resolve(a.Workdir, "")
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return tools.Result{}, tools.Errorf(tools.KindInvalidInput, "workdir %q is not a directory", dir)
	}

	b.mu.Lock()
	if b.procs == nil {
		b.procs = map[string]*bgProc{}
	}
	running := 0
	for _, p := range b.procs {
		if p.running() {
			running++
		}
	}
	limit := b.MaxRunning
	if limit <= 0 {
		limit = 8
	}
	if running >= limit {
		b.mu.Unlock()
		return tools.Result{}, tools.Errorf(tools.KindDenied, "%d background processes are already running; stop one with proc_stop first", running)
	}
	b.seq++
	id := fmt.Sprintf("p%d", b.seq)
	b.mu.Unlock()

	size := b.BufferSize
	if size <= 0 {
		size = 256 << 10
	}
	// The process outlives the tool call, so it gets its own context, not ctx.
	pctx, cancel := context.WithCancel(context.Background())
	p := &bgProc{id: id, name: a.Name, argv: append([]string{a.Command}, a.Args...), dir: dir, started: time.Now(),
		cancel: cancel, done: make(chan struct{}), out: ring{cap: size}}
	cmd := exec.CommandContext(pctx, path, a.Args...)
	cmd.Dir = dir
	cmd.Env = b.Runner.childEnv()
	cmd.Stdout, cmd.Stderr = procWriter{p}, procWriter{p}
	tree := configureProcess(cmd)
	if err := cmd.Start(); err != nil {
		cancel()
		tree.release()
		return tools.Result{}, tools.Errorf(tools.KindUnavailable, "could not start %s: %v", filepath.Base(path), err)
	}
	tree.attach(cmd)
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		var ee *exec.ExitError
		switch {
		case err == nil:
		case errors.As(err, &ee) && ee.ExitCode() >= 0:
			p.exitCode = ee.ExitCode()
		default:
			p.exitErr = err.Error()
		}
		p.mu.Unlock()
		tree.release()
		cancel()
		close(p.done)
	}()
	b.mu.Lock()
	b.procs[id] = p
	b.mu.Unlock()

	wait := time.Duration(a.WaitSec) * time.Second
	if a.WaitSec == 0 {
		wait = 5 * time.Second
	}
	matched := b.waitFor(ctx, p, a.WaitFor, wait)
	label := strings.Join(p.argv, " ")
	if p.name != "" {
		label = p.name + ": " + label
	}
	out, _ := b.read(p, false)
	head := fmt.Sprintf("started %s as %s (pid %d) in %s\nstatus: %s\n", label, id, cmd.Process.Pid, dir, p.status())
	if a.WaitFor != "" && !matched && p.running() {
		head += fmt.Sprintf("note: %q did not appear within %s; check proc_output later\n", a.WaitFor, wait)
	}
	// Not a lasting change: background processes never outlive ScoutClaw, so
	// starting one does not by itself demand verification.
	res := tools.Result{Output: head + out, Metadata: map[string]string{"id": id}}
	if !p.running() {
		// It ended during the wait: report it as a failure if it failed.
		p.mu.Lock()
		code := p.exitCode
		p.mu.Unlock()
		if code != 0 {
			res.Kind, res.ExitCode = tools.KindFailed, code
		}
	}
	return res, nil
}

// waitFor waits until pattern appears in the unread output, the process
// exits, the wait ends or ctx is cancelled. It reports whether pattern matched.
func (b *Background) waitFor(ctx context.Context, p *bgProc, pattern string, wait time.Duration) bool {
	var re *regexp.Regexp
	if pattern != "" {
		re = regexp.MustCompile(pattern)
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		if re != nil {
			p.mu.Lock()
			text, _ := p.out.since(p.readPos)
			p.mu.Unlock()
			if re.MatchString(text) {
				return true
			}
		}
		select {
		case <-p.done:
			return false
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return false
		case <-tick.C:
		}
	}
}

// read returns new (or, with all, all buffered) output and advances the read position.
func (b *Background) read(p *bgProc, all bool) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pos := p.readPos
	if all {
		pos = 0
	}
	text, dropped := p.out.since(pos)
	p.readPos = p.out.total
	var sb strings.Builder
	if dropped > 0 {
		fmt.Fprintf(&sb, "[%d earlier bytes no longer buffered]\n", dropped)
	}
	if text == "" {
		sb.WriteString("(no new output)\n")
	} else {
		sb.WriteString(text)
		if !strings.HasSuffix(text, "\n") {
			sb.WriteString("\n")
		}
	}
	return sb.String(), dropped > 0
}

func (b *Background) output(ctx context.Context, a procOutputArgs) (tools.Result, error) {
	p, err := b.get(a.ID)
	if err != nil {
		return tools.Result{}, err
	}
	if a.WaitSec > 0 {
		b.waitFor(ctx, p, a.WaitFor, time.Duration(a.WaitSec)*time.Second)
	}
	out, truncated := b.read(p, a.All)
	return tools.Result{Output: fmt.Sprintf("%s: %s\n%s", p.id, p.status(), out), Truncated: truncated}, nil
}

func (b *Background) stop(ctx context.Context, a procIDArgs) (tools.Result, error) {
	p, err := b.get(a.ID)
	if err != nil {
		return tools.Result{}, err
	}
	if !p.running() {
		out, _ := b.read(p, false)
		return tools.Result{Output: fmt.Sprintf("%s already %s\n%s", p.id, p.status(), out)}, nil
	}
	b.terminate(p)
	out, _ := b.read(p, false)
	return tools.Result{Output: fmt.Sprintf("stopped %s: %s\n%s", p.id, p.status(), out)}, nil
}

// terminate stops the process tree (SIGTERM, then SIGKILL after 2s on Unix;
// the Job Object on Windows) and waits for it to be reaped.
func (b *Background) terminate(p *bgProc) {
	p.cancel()
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
	}
}

// List describes every background process of the session.
func (b *Background) List() string {
	b.mu.Lock()
	procs := make([]*bgProc, 0, len(b.procs))
	for _, p := range b.procs {
		procs = append(procs, p)
	}
	b.mu.Unlock()
	if len(procs) == 0 {
		return "no background processes\n"
	}
	sort.Slice(procs, func(i, j int) bool { return procs[i].started.Before(procs[j].started) })
	var sb strings.Builder
	for _, p := range procs {
		name := ""
		if p.name != "" {
			name = " [" + p.name + "]"
		}
		fmt.Fprintf(&sb, "%s%s  %s  %s\n", p.id, name, p.status(), strings.Join(p.argv, " "))
	}
	return sb.String()
}

// Running returns how many background processes are still running.
func (b *Background) Running() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, p := range b.procs {
		if p.running() {
			n++
		}
	}
	return n
}

// Shutdown stops every running background process and returns how many it stopped.
func (b *Background) Shutdown() int {
	b.mu.Lock()
	var running []*bgProc
	for _, p := range b.procs {
		if p.running() {
			running = append(running, p)
		}
	}
	b.mu.Unlock()
	var wg sync.WaitGroup
	for _, p := range running {
		wg.Add(1)
		go func(p *bgProc) { defer wg.Done(); b.terminate(p) }(p)
	}
	wg.Wait()
	return len(running)
}
