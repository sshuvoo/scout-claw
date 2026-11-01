//go:build windows

package platform

import "golang.org/x/sys/windows"

// isElevated reports whether the process runs with an elevated (administrator)
// token. os.Geteuid always returns -1 on Windows.
func isElevated() bool { return windows.GetCurrentProcessToken().IsElevated() }
