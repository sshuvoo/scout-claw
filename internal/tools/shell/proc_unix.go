//go:build !windows

package shell

import (
	"os/exec"
	"syscall"
	"time"
)

type procTree struct{}

// configureProcess puts the child in its own process group so that timeout and
// cancellation can terminate it together with everything it spawned.
func configureProcess(cmd *exec.Cmd) procTree {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		go func(pid int) {
			time.Sleep(2 * time.Second)
			if syscall.Kill(-pid, 0) == nil { // group still alive: escalate
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			}
		}(cmd.Process.Pid)
		return nil
	}
	cmd.WaitDelay = 3 * time.Second
	return procTree{}
}

// attach is a no-op: the process group is set up at spawn time.
func (procTree) attach(*exec.Cmd) {}
func (procTree) release()         {}
