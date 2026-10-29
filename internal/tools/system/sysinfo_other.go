//go:build !linux

package system

import (
	"context"
	"fmt"
	"runtime"

	"scoutclaw/internal/platform"
	"scoutclaw/internal/tools"
)

func collectSysInfo(p platform.Info) (string, error) {
	return fmt.Sprintf("host: %s\nos: %s/%s\nnote: detailed metrics are only implemented for Linux; use shell_exec with native tools on %s\n", p.Hostname, p.OS, p.Arch, runtime.GOOS), nil
}

func listProcesses(context.Context, string, int) (string, error) {
	return "", tools.Errorf(tools.KindUnavailable, "process_list is only implemented for Linux; use shell_exec with the platform's process tool")
}
