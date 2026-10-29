// Package config loads ScoutClaw settings.
//
// Precedence (highest first): command-line flags, environment variables
// (including values loaded from .env files), built-in defaults. .env files are
// read in the legacy order (./.env, ~/.scoutclaw.env, ~/.scout.env, ~/.env) and
// never override variables that are already set.
package config

import (
	"bufio"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Approval string

const (
	// ApprovalAsk prompts for medium and high risk actions (default).
	ApprovalAsk Approval = "ask"
	// ApprovalAuto runs medium risk actions without prompting; high risk still prompts.
	ApprovalAuto Approval = "auto"
	// ApprovalReadOnly denies every action that is not low risk.
	ApprovalReadOnly Approval = "readonly"
)

const DefaultModel = "gpt-5.5" // the model the original implementation used

type Config struct {
	APIKey      string // never printed; see Masked
	BaseURL     string
	Model       string
	Workspace   string
	StateDir    string
	Approval    Approval
	MaxSteps    int
	ToolTimeout time.Duration
	TaskTimeout time.Duration
	MaxOutput   int // bytes of tool output kept per call
	LogLevel    string
}

// Env names. OPENAI_API_KEY and OPENAI_BASE_URL are the pre-existing conventions.
const (
	EnvAPIKey      = "OPENAI_API_KEY"
	EnvBaseURL     = "OPENAI_BASE_URL"
	EnvModel       = "SCOUTCLAW_MODEL"
	EnvWorkspace   = "SCOUTCLAW_WORKSPACE"
	EnvStateDir    = "SCOUTCLAW_STATE_DIR"
	EnvApproval    = "SCOUTCLAW_APPROVAL"
	EnvMaxSteps    = "SCOUTCLAW_MAX_STEPS"
	EnvToolTimeout = "SCOUTCLAW_TOOL_TIMEOUT"
	EnvTaskTimeout = "SCOUTCLAW_TASK_TIMEOUT"
	EnvMaxOutput   = "SCOUTCLAW_MAX_OUTPUT"
	EnvLogLevel    = "SCOUTCLAW_LOG_LEVEL"
)

// Load reads configuration from the environment. Call LoadDotEnv first.
func Load() (*Config, error) {
	c := &Config{
		APIKey:      os.Getenv(EnvAPIKey),
		BaseURL:     os.Getenv(EnvBaseURL),
		Model:       getenv(EnvModel, DefaultModel),
		Approval:    Approval(getenv(EnvApproval, string(ApprovalAsk))),
		MaxSteps:    25, // same limit as the original loop
		ToolTimeout: 2 * time.Minute,
		TaskTimeout: 30 * time.Minute,
		MaxOutput:   16 * 1024,
		LogLevel:    getenv(EnvLogLevel, "warn"),
	}
	var err error
	if c.Workspace, err = resolveWorkspace(os.Getenv(EnvWorkspace)); err != nil {
		return nil, err
	}
	if c.StateDir, err = resolveStateDir(os.Getenv(EnvStateDir)); err != nil {
		return nil, err
	}
	if c.MaxSteps, err = envInt(EnvMaxSteps, c.MaxSteps); err != nil {
		return nil, err
	}
	if c.MaxOutput, err = envInt(EnvMaxOutput, c.MaxOutput); err != nil {
		return nil, err
	}
	if c.ToolTimeout, err = envDuration(EnvToolTimeout, c.ToolTimeout); err != nil {
		return nil, err
	}
	if c.TaskTimeout, err = envDuration(EnvTaskTimeout, c.TaskTimeout); err != nil {
		return nil, err
	}
	return c, c.Validate()
}

// Validate checks bounds. A missing API key is not a validation error here:
// commands such as `config` and `doctor` must work without one (see RequireAPIKey).
func (c *Config) Validate() error {
	switch c.Approval {
	case ApprovalAsk, ApprovalAuto, ApprovalReadOnly:
	default:
		return fmt.Errorf("invalid approval policy %q (want ask, auto or readonly)", c.Approval)
	}
	if c.MaxSteps < 1 || c.MaxSteps > 200 {
		return fmt.Errorf("max steps must be between 1 and 200, got %d", c.MaxSteps)
	}
	if c.MaxOutput < 1024 || c.MaxOutput > 1<<20 {
		return fmt.Errorf("max output must be between 1024 and 1048576 bytes, got %d", c.MaxOutput)
	}
	if c.ToolTimeout < time.Second || c.TaskTimeout < c.ToolTimeout {
		return fmt.Errorf("timeouts invalid: tool=%s task=%s (tool >= 1s, task >= tool)", c.ToolTimeout, c.TaskTimeout)
	}
	if st, err := os.Stat(c.Workspace); err != nil || !st.IsDir() {
		return fmt.Errorf("workspace %q is not a directory", c.Workspace)
	}
	return nil
}

func (c *Config) RequireAPIKey() error {
	if c.APIKey == "" {
		return fmt.Errorf("%s is not set. Export it, or put it in ./.env, ~/.scoutclaw.env, ~/.scout.env or ~/.env (see .env.example)", EnvAPIKey)
	}
	return nil
}

func (c *Config) AuditPath() string   { return filepath.Join(c.StateDir, "audit.jsonl") }
func (c *Config) SessionsDir() string { return filepath.Join(c.StateDir, "sessions") }
func (c *Config) BackupsDir() string  { return filepath.Join(c.StateDir, "backups") }

// MaskedKey describes the key without revealing it.
func (c *Config) MaskedKey() string {
	switch {
	case c.APIKey == "":
		return "(not set)"
	case len(c.APIKey) <= 8:
		return "set (****)"
	default:
		return "set (" + c.APIKey[:3] + "…, " + fmt.Sprint(len(c.APIKey)) + " chars)"
	}
}

func getenv(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) (int, error) {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not an integer", k, v)
	}
	return n, nil
}

func envDuration(k string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a duration (e.g. 90s, 5m)", k, v)
	}
	return d, nil
}

func resolveWorkspace(v string) (string, error) {
	if v == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		v = wd
	}
	abs, err := filepath.Abs(v)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	return abs, nil
}

func resolveStateDir(v string) (string, error) {
	if v == "" {
		if x := os.Getenv("XDG_STATE_HOME"); x != "" {
			v = filepath.Join(x, "scoutclaw")
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("cannot determine state dir: %w (set %s)", err, EnvStateDir)
			}
			v = filepath.Join(home, ".local", "state", "scoutclaw")
		}
	}
	return filepath.Abs(v)
}

// LoadDotEnv loads KEY=VALUE files without overriding existing variables.
func LoadDotEnv() {
	parseEnvFile(".env")
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		for _, n := range []string{".scoutclaw.env", ".scout.env", ".env"} {
			parseEnvFile(filepath.Join(u.HomeDir, n))
		}
	}
}

func parseEnvFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if k != "" && os.Getenv(k) == "" {
			_ = os.Setenv(k, v)
		}
	}
}
