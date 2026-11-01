package platform

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestBashOverride(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "mybash")
	if err := os.WriteFile(fake, []byte{}, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvBash, fake)
	if got := DetectShells().Bash; got != fake {
		t.Fatalf("override ignored: %q", got)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	t.Setenv(EnvBash, missing)
	if got := DetectShells().Bash; got == missing {
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
		if s.Bash != "" && filepath.Base(filepath.Dir(s.Bash)) != "bin" {
			t.Errorf("bash should be Git's bin\\bash.exe, got %s", s.Bash)
		}
	default:
		if s.Bash == "" {
			t.Error("bash or /bin/sh should be found on Unix")
		}
		if s.PowerShell != "" {
			t.Error("shell_powershell is Windows-only")
		}
	}
}
