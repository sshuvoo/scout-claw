// Package builtin assembles the default tool registry.
package builtin

import (
	"time"

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
	SessionID   string
	MaxOutput   int
	ToolTimeout time.Duration
	StripEnv    []string
	Tracker     *verify.Tracker
}

// New returns the registry and the per-task state shared with the task tools.
func New(o Options) (*tools.Registry, *task.State) {
	reg := tools.NewRegistry()
	state := task.NewState(o.Tracker)
	reg.MustRegister(filesystem.FS{Scope: o.Scope, BackupDir: o.BackupDir, SessionID: o.SessionID, MaxReadLen: o.MaxOutput}.Tools()...)
	reg.MustRegister(shell.Runner{Scope: o.Scope, MaxOutput: o.MaxOutput, DefaultTimeout: o.ToolTimeout, StripEnv: o.StripEnv}.Tools()...)
	reg.MustRegister(system.Tools()...)
	reg.MustRegister(network.Tools()...)
	reg.MustRegister(state.Tools()...)
	return reg, state
}
