//go:build !windows

package platform

import "os"

func isElevated() bool { return os.Geteuid() == 0 }
