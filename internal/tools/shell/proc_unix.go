//go:build !windows

package shell

import (
	"os/exec"
	"syscall"
	"time"
)

// configureProcess puts the child in its own process group so that timeout and
// cancellation can terminate it together with everything it spawned.
func configureProcess(cmd *exec.Cmd) {
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
}
