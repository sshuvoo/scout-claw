//go:build windows

package shell

import "os/exec"

// configureProcess: process-group termination is not implemented on Windows yet;
// only the direct child is killed on cancellation.
func configureProcess(cmd *exec.Cmd) {}
