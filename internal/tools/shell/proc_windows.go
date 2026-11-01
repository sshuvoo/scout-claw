//go:build windows

package shell

import (
	"os/exec"
	"time"

	"golang.org/x/sys/windows"
)

// procTree is a Windows Job Object. Processes started by a job member join the
// job too, so terminating it kills the whole tree, including grandchildren
// whose parent has already exited (which taskkill /T would miss).
type procTree struct{ job windows.Handle }

func configureProcess(cmd *exec.Cmd) procTree {
	t := procTree{}
	if job, err := windows.CreateJobObject(nil, nil); err == nil {
		t.job = job
	}
	cmd.Cancel = func() error {
		if t.job != 0 {
			_ = windows.TerminateJobObject(t.job, 1)
		}
		if cmd.Process != nil { // in case attach failed
			_ = cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = 3 * time.Second
	return t
}

// attach adds the started process to the job. Anything it spawns from now on
// is a job member as well.
func (t procTree) attach(cmd *exec.Cmd) {
	if t.job == 0 || cmd.Process == nil {
		return
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	_ = windows.AssignProcessToJobObject(t.job, h)
}

// release closes the job handle. Without JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
// background processes a successful command left running keep running, as
// they do on Unix.
func (t procTree) release() {
	if t.job != 0 {
		_ = windows.CloseHandle(t.job)
	}
}
