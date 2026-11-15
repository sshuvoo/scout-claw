package platform

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPowerShellOverride(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "pwsh")
	if err := os.WriteFile(fake, []byte{}, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvPowerShell, fake)
	if got := powerShell(); got != fake {
		t.Fatalf("override ignored: %q", got)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	t.Setenv(EnvPowerShell, missing)
	if got := powerShell(); got == missing {
		t.Fatal("a missing override must not be used")
	}
}

func TestDetectShellsPerOS(t *testing.T) {
	s := DetectShells()
	switch runtime.GOOS {
	case "windows":
		if s.PowerShell == "" {
			t.Error("PowerShell should always be found on Windows")
		}
	default:
		if s.PowerShell != "" {
			t.Error("shell_powershell is Windows-only")
		}
	}
}
