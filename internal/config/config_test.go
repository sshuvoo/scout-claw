package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func clearEnv(t *testing.T) {
	for _, k := range []string{EnvAPIKey, EnvBaseURL, EnvModel, EnvWorkspace, EnvStateDir, EnvApproval, EnvMaxSteps, EnvToolTimeout, EnvTaskTimeout, EnvMaxOutput, EnvLogLevel} {
		t.Setenv(k, "")
	}
	t.Setenv(EnvStateDir, t.TempDir())
}

func TestDefaults(t *testing.T) {
	clearEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Model != DefaultModel || c.MaxSteps != 25 || c.Approval != ApprovalAsk {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if c.RequireAPIKey() == nil {
		t.Fatal("missing key must be reported")
	}
}

func TestOverridesAndValidation(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvMaxSteps, "7")
	t.Setenv(EnvToolTimeout, "30s")
	t.Setenv(EnvApproval, "readonly")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxSteps != 7 || c.ToolTimeout != 30*time.Second || c.Approval != ApprovalReadOnly {
		t.Fatalf("overrides not applied: %+v", c)
	}
	for k, v := range map[string]string{EnvApproval: "yolo", EnvMaxSteps: "abc"} {
		clearEnv(t)
		t.Setenv(k, v)
		if _, err := Load(); err == nil {
			t.Errorf("%s=%s should fail", k, v)
		}
	}
	clearEnv(t)
	t.Setenv(EnvMaxSteps, "0")
	if _, err := Load(); err == nil {
		t.Error("max steps 0 should fail")
	}
}

func TestMaskedKeyHidesSecret(t *testing.T) {
	c := &Config{APIKey: "sk-supersecretvalue"}
	if m := c.MaskedKey(); strings.Contains(m, "supersecret") || strings.Contains(m, c.APIKey[3:]) {
		t.Fatalf("not masked: %s", m)
	}
}

func TestDotEnvDoesNotOverride(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.env")
	os.WriteFile(p, []byte("# c\nexport FOO_A=\"one\"\nFOO_B=two\n"), 0o600)
	t.Setenv("FOO_A", "")
	t.Setenv("FOO_B", "keep")
	parseEnvFile(p)
	if os.Getenv("FOO_A") != "one" || os.Getenv("FOO_B") != "keep" {
		t.Fatalf("A=%q B=%q", os.Getenv("FOO_A"), os.Getenv("FOO_B"))
	}
}
