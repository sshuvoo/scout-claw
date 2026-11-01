package platform

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Env overrides for shell discovery.
const (
	EnvBash       = "SCOUT_BASH_PATH"       // bash used by shell_script (on Windows: Git Bash's bin\bash.exe)
	EnvPowerShell = "SCOUT_POWERSHELL_PATH" // PowerShell used by shell_powershell (Windows only)
)

// Shells reports the script interpreters available to the agent. An empty
// path means the corresponding tool is not offered.
type Shells struct {
	Bash       string `json:"bash,omitempty"`
	PowerShell string `json:"powershell,omitempty"`
}

// DetectShells follows the same conventions as other coding agents: the
// system bash on Linux and macOS; on Windows, Git Bash when Git for Windows
// is installed plus PowerShell (7 preferred over Windows PowerShell 5.1).
func DetectShells() Shells {
	if runtime.GOOS != "windows" {
		return Shells{Bash: unixBash()}
	}
	return Shells{Bash: gitBash(), PowerShell: powerShell()}
}

func unixBash() string {
	if p := fromEnv(EnvBash); p != "" {
		return p
	}
	if p, err := exec.LookPath("bash"); err == nil {
		return p
	}
	if isFile("/bin/sh") {
		return "/bin/sh"
	}
	return ""
}

// gitBash finds Git for Windows' bin\bash.exe. That wrapper puts Git's
// usr\bin (coreutils, grep, sed, ...) on PATH. A bare LookPath("bash") is
// avoided on purpose: it usually finds System32\bash.exe, the WSL launcher,
// which runs in a different filesystem and environment.
func gitBash() string {
	if p := fromEnv(EnvBash); p != "" {
		return p
	}
	var roots []string
	if git, err := exec.LookPath("git"); err == nil {
		// <root>\cmd\git.exe or <root>\bin\git.exe or <root>\mingw64\bin\git.exe
		dir := filepath.Dir(git)
		roots = append(roots, filepath.Dir(dir), filepath.Dir(filepath.Dir(dir)))
	}
	for _, env := range []string{"ProgramFiles", "ProgramW6432", "ProgramFiles(x86)", "LocalAppData"} {
		if v := os.Getenv(env); v != "" {
			roots = append(roots, filepath.Join(v, "Git"), filepath.Join(v, "Programs", "Git"))
		}
	}
	for _, r := range roots {
		if p := filepath.Join(r, "bin", "bash.exe"); isFile(p) {
			return p
		}
	}
	return ""
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
