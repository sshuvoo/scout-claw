// Package integration exercises the compiled scout binary. It needs the Go
// toolchain but no API credentials or network access.
package integration

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "scout-it")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "scout")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/scout")
	if out, err := build.CombinedOutput(); err != nil {
		os.Stderr.Write(out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func scout(t *testing.T, env []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	// Isolated: no inherited credentials, config files or state.
	home := t.TempDir()
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "SCOUTCLAW_STATE_DIR=" + filepath.Join(home, "state")}, env...)
	cmd.Dir = t.TempDir()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

func TestVersionAndHelp(t *testing.T) {
	out, _, code := scout(t, nil, "version")
	if code != 0 || !strings.HasPrefix(out, "scout ") {
		t.Fatalf("%d %q", code, out)
	}
	out, _, code = scout(t, nil, "--help")
	if code != 0 || !strings.Contains(out, "doctor") || !strings.Contains(out, "history") {
		t.Fatalf("%d %q", code, out)
	}
}

func TestRunWithoutCredentialsFailsClearly(t *testing.T) {
	_, errb, code := scout(t, nil, "run", "hello")
	if code == 0 || !strings.Contains(errb, "OPENAI_API_KEY") {
		t.Fatalf("code=%d stderr=%q", code, errb)
	}
}

func TestConfigRejectsBadValuesAndHidesSecrets(t *testing.T) {
	_, errb, code := scout(t, []string{"SCOUTCLAW_APPROVAL=yolo"}, "config")
	if code == 0 || !strings.Contains(errb, "approval") {
		t.Fatalf("code=%d stderr=%q", code, errb)
	}
	out, _, code := scout(t, []string{"OPENAI_API_KEY=sk-integration-secret-123456"}, "config")
	if code != 0 || strings.Contains(out, "integration-secret") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestDoctorFlagsMissingKey(t *testing.T) {
	out, _, code := scout(t, nil, "doctor")
	if code == 0 || !strings.Contains(out, "✗ api key") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestHistoryEmpty(t *testing.T) {
	out, _, code := scout(t, nil, "history")
	if code != 0 || !strings.Contains(out, "no history") {
		t.Fatalf("%d %q", code, out)
	}
}
