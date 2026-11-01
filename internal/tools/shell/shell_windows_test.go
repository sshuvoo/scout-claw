//go:build windows

package shell

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"scoutclaw/internal/policy"
	"scoutclaw/internal/tools"
)

func powershellRunner(t *testing.T) (Runner, tools.Tool) {
	ps, err := exec.LookPath("pwsh.exe")
	if err != nil {
		if ps, err = exec.LookPath("powershell.exe"); err != nil {
			t.Skip("no PowerShell")
		}
	}
	r := Runner{Scope: policy.NewScope(t.TempDir()), MaxOutput: 4096, DefaultTimeout: 20 * time.Second, PowerShell: ps}
	for _, tl := range r.Tools() {
		if tl.Name() == "shell_powershell" {
			return r, tl
		}
	}
	t.Fatal("no shell_powershell")
	return r, nil
}

func TestPowerShellScriptOutputAndExitCode(t *testing.T) {
	_, tl := powershellRunner(t)
	res, err := call(t, tl, context.Background(), map[string]any{"script": `Write-Output "héllo"; exit 3`})
	if err != nil || res.ExitCode != 3 || !strings.Contains(res.Output, "héllo") {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestTimeoutKillsJobTree(t *testing.T) {
	_, tl := powershellRunner(t)
	marker := filepath.Join(t.TempDir(), "child-survived")
	// The grandchild would create the marker after 4s if the job were not terminated.
	script := `Start-Process -WindowStyle Hidden -FilePath (Get-Process -Id $PID).Path -ArgumentList '-NoProfile','-Command',"Start-Sleep 4; New-Item -ItemType File '` + marker + `'"; Start-Sleep 30`
	res, err := call(t, tl, context.Background(), map[string]any{"script": script, "timeout_sec": 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != tools.KindTimeout {
		t.Fatalf("want timeout, got %+v", res)
	}
	time.Sleep(5 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("grandchild survived: job object was not terminated")
	}
}

func TestChildEnvStripIsCaseInsensitive(t *testing.T) {
	t.Setenv("Openai_Api_Key", "sk-should-not-leak-1234567890")
	r := Runner{StripEnv: []string{"OPENAI_API_KEY"}}
	for _, kv := range r.childEnv() {
		if strings.Contains(strings.ToUpper(kv), "OPENAI_API_KEY") {
			t.Fatalf("leaked: %s", kv)
		}
	}
}
