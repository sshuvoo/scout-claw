// Package platform detects the operating system and the capabilities ScoutClaw
// can rely on. Only Linux has full diagnostic support today; other platforms
// report what they are and the tools that need OS-specific data say so.
package platform

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
)

type Info struct {
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	Distro         string `json:"distro,omitempty"`
	Hostname       string `json:"hostname"`
	PackageManager string `json:"package_manager,omitempty"`
	Systemd        bool   `json:"systemd"`
	IsRoot         bool   `json:"is_root"`
	FullySupported bool   `json:"fully_supported"`
}

func Detect() Info {
	host, _ := os.Hostname()
	i := Info{
		OS:             runtime.GOOS,
		Arch:           runtime.GOARCH,
		Hostname:       host,
		IsRoot:         os.Geteuid() == 0,
		FullySupported: runtime.GOOS == "linux",
	}
	if runtime.GOOS == "linux" {
		i.Distro = osReleaseName()
		if st, err := os.Stat("/run/systemd/system"); err == nil && st.IsDir() {
			i.Systemd = true
		}
	}
	for _, pm := range []string{"apt-get", "dnf", "yum", "pacman", "zypper", "apk", "brew"} {
		if _, err := exec.LookPath(pm); err == nil {
			i.PackageManager = pm
			break
		}
	}
	return i
}

func osReleaseName() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}
