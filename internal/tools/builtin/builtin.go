// Package builtin assembles the default tool registry.
package builtin

import (
	"time"

	"scoutclaw/internal/platform"
	"scoutclaw/internal/policy"
	"scoutclaw/internal/tools"
	"scoutclaw/internal/tools/filesystem"
	"scoutclaw/internal/tools/network"
	"scoutclaw/internal/tools/shell"
	"scoutclaw/internal/tools/system"
	"scoutclaw/internal/tools/task"
	"scoutclaw/internal/verify"
)

type Options struct {
	Scope       policy.Scope
	BackupDir   string
	TrashDir    string
	Files       *filesystem.State // shared file read records and change journal (optional)
	MaxRead     int               // bytes fs_read may return (default MaxOutput)
	SessionID   string
	MaxOutput   int
	ToolTimeout time.Duration
	StripEnv    []string
	Tracker     *verify.Tracker
	Shells      platform.Shells   // script interpreters; an empty path omits that tool
	Background  *shell.Background // session's background processes; created if nil
}

// FS returns the filesystem tool set for o; the CLI also uses it for /undo.
func FS(o Options) filesystem.FS {
	read := o.MaxRead
	if read <= 0 {
		read = o.MaxOutput
	}
	return filesystem.FS{Scope: o.Scope, BackupDir: o.BackupDir, TrashDir: o.TrashDir, SessionID: o.SessionID,
		MaxReadLen: read, State: o.Files}
}

// Runner returns the shell runner for o; background processes use the same settings.
func Runner(o Options) shell.Runner {
	return shell.Runner{Scope: o.Scope, MaxOutput: o.MaxOutput, DefaultTimeout: o.ToolTimeout, StripEnv: o.StripEnv,
		Bash: o.Shells.Bash, PowerShell: o.Shells.PowerShell}
}

// New returns the registry and the per-task state shared with the task tools.
func New(o Options) (*tools.Registry, *task.State) {
	reg := tools.NewRegistry()
	state := task.NewState(o.Tracker)
	reg.MustRegister(FS(o).Tools()...)
	reg.MustRegister(Runner(o).Tools()...)
	bg := o.Background
	if bg == nil {
		bg = &shell.Background{Runner: Runner(o)}
	}
	reg.MustRegister(bg.Tools()...)
	reg.MustRegister(system.Tools()...)
	reg.MustRegister(network.Tools()...)
	reg.MustRegister(state.Tools()...)
	return reg, state
}
