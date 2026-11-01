package shell

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"scoutclaw/internal/policy"
	"scoutclaw/internal/tools"
)

func toolNames(r Runner) []string {
	var names []string
	for _, tl := range r.Tools() {
		names = append(names, tl.Name())
	}
	return names
}

func TestScriptToolsFollowDetectedShells(t *testing.T) {
	r := Runner{Scope: policy.NewScope(t.TempDir()), MaxOutput: 2048, DefaultTimeout: 5 * time.Second}
	if got := strings.Join(toolNames(r), ","); got != "shell_exec" {
		t.Fatalf("no shells: got %s", got)
	}
	r.Bash, r.PowerShell = "/bin/bash", "pwsh"
	if got := strings.Join(toolNames(r), ","); got != "shell_exec,shell_script,shell_powershell" {
		t.Fatalf("both shells: got %s", got)
	}
}

func TestShellScriptRunsBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil || runtime.GOOS == "windows" {
		t.Skip("no system bash")
	}
	r := Runner{Scope: policy.NewScope(t.TempDir()), MaxOutput: 4096, DefaultTimeout: 5 * time.Second, Bash: bash}
	var tl tools.Tool
	for _, x := range r.Tools() {
		if x.Name() == "shell_script" {
			tl = x
		}
	}
	// [[ ]] and arrays are bash-only: this fails under a plain POSIX sh.
	res, err := call(t, tl, context.Background(), map[string]any{"script": `a=(x y z); [[ ${#a[@]} -eq 3 ]] && echo "len=${#a[@]}" | tr a-z A-Z`})
	if err != nil || !res.OK() || !strings.Contains(res.Output, "LEN=3") {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestBatchFilesRejectCmdMetacharacters(t *testing.T) {
	dir := t.TempDir()
	bat := filepath.Join(dir, "x.cmd")
	if err := checkBatchArgs(bat, []string{"--flag", "value"}); err != nil {
		t.Fatalf("plain args should pass: %v", err)
	}
	for _, a := range []string{"a&calc", "x|y", "%PATH%", `"q"`, "a^b", "!v!"} {
		if err := checkBatchArgs(bat, []string{a}); tools.KindOf(err) != tools.KindInvalidInput {
			t.Errorf("%q should be rejected for a batch file, got %v", a, err)
		}
	}
	if err := checkBatchArgs(filepath.Join(dir, "x.exe"), []string{"a&b"}); err != nil {
		t.Errorf("non-batch executables get arguments verbatim: %v", err)
	}
}
