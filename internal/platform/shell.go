package platform

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// EnvPowerShell overrides PowerShell discovery on Windows.
const EnvPowerShell = "SCOUT_POWERSHELL_PATH"

// Shells reports optional platform shells. shell_script is embedded and does
// not depend on this discovery.
type Shells struct {
	PowerShell string `json:"powershell,omitempty"`
}

// DetectShells finds PowerShell on Windows (7 preferred over Windows
// PowerShell 5.1).
func DetectShells() Shells {
	if runtime.GOOS != "windows" {
		return Shells{}
	}
	return Shells{PowerShell: powerShell()}
}

func powerShell() string {
	if p := fromEnv(EnvPowerShell); p != "" {
		return p
	}
	// Windows PowerShell 5.1 strips embedded double quotes from arguments to
	// native programs; PowerShell 7 does not, so prefer it.
	for _, name := range []string{"pwsh.exe", "powershell.exe"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

func fromEnv(key string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" && isFile(v) {
		return v
	}
	return ""
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
