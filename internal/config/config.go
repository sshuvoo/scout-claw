// Package config loads ScoutClaw settings.
//
// Precedence (highest first): command-line flags, environment variables,
// <project>/.scout/config.local.json, <project>/.scout/config.json,
// ~/.scout/config.json, .env files, built-in defaults. .env files are read in
// the legacy order (./.env, ~/.scoutclaw.env, ~/.scout.env, ~/.env) and never
// override variables that are already set.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Approval string

const (
	// ApprovalAsk prompts for medium and high risk actions (default).
	ApprovalAsk Approval = "ask"
	// ApprovalEdits runs medium risk file edits inside the workspace without
	// prompting; everything else behaves like ApprovalAsk.
	ApprovalEdits Approval = "edits"
	// ApprovalAuto runs medium risk actions without prompting; high risk still prompts.
	ApprovalAuto Approval = "auto"
	// ApprovalReadOnly denies every action that is not low risk.
	ApprovalReadOnly Approval = "readonly"
)

const DefaultModel = "gpt-5.5" // the model the original implementation used

// DefaultMaxSteps leaves room for coding tasks (explore, edit, build, test,
// fix, re-test); simple diagnostics finish long before it.
const (
	DefaultMaxSteps = 100
	MaxMaxSteps     = 500
)

// DefaultCompactAt keeps long coding sessions well inside current context
// windows and bounds what every turn costs.
const (
	DefaultCompactAt = 200_000
	MinCompactAt     = 20_000
)

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
	MaxRead     int // bytes of file content fs_read may return
	CompactAt   int // context size in tokens at which the provider compacts the conversation; 0 disables
	LogLevel    string

	ProjectRoot  string            // directory holding .scout/ (the workspace when there is none)
	Sources      map[string]string // setting key -> where its value came from
	Rules        []RuleSpec        // permission rules from all config files, lowest layer first
	ExtraDirs    []string          // additional writable directories (permissions.additionalDirectories)
	Instructions []string          // extra instruction files listed in config files
	Trust        Trust             // state of the shared project config
	Warnings     []string          // problems that did not stop loading
}

// Options adjust loading.
type Options struct {
	// TrustProject applies the shared project config fully for this run
	// without recording trust (scout --trust-workspace).
	TrustProject bool
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
	EnvMaxRead     = "SCOUTCLAW_MAX_READ"
	EnvCompactAt   = "SCOUTCLAW_COMPACT_AT"
	EnvLogLevel    = "SCOUTCLAW_LOG_LEVEL"
)

// Load reads configuration from defaults, .env files, config files and the
// environment. Call LoadDotEnv first.
func Load() (*Config, error) { return LoadWith(Options{}) }

// LoadWith is Load with options.
func LoadWith(o Options) (*Config, error) {
	c := &Config{
		APIKey:      os.Getenv(EnvAPIKey),
		Model:       DefaultModel,
		Approval:    ApprovalAsk,
		MaxSteps:    DefaultMaxSteps,
		ToolTimeout: 2 * time.Minute,
		TaskTimeout: 30 * time.Minute,
		MaxOutput:   16 * 1024,
		MaxRead:     64 * 1024,
		CompactAt:   DefaultCompactAt,
		LogLevel:    "warn",
		Sources:     map[string]string{},
	}
	var err error
	if c.Workspace, err = resolveWorkspace(os.Getenv(EnvWorkspace)); err != nil {
		return nil, err
	}
	if c.StateDir, err = resolveStateDir(os.Getenv(EnvStateDir)); err != nil {
		return nil, err
	}
	c.ProjectRoot = FindProjectRoot(c.Workspace)
	settings := settingsFor(c)
	for _, s := range settings {
		c.Sources[s.key] = "default"
	}
	// .env values are the lowest layer above defaults ...
	for _, s := range settings {
		if file, ok := dotEnvKeys[s.env]; ok && strings.TrimSpace(os.Getenv(s.env)) != "" {
			if err := s.apply(c, os.Getenv(s.env)); err != nil {
				return nil, fmt.Errorf("%s (from %s): %v", s.env, file, err)
			}
			c.Sources[s.key] = file
		}
	}
	// ... then config files ...
	if err := c.loadFiles(o.TrustProject); err != nil {
		return nil, err
	}
	// ... and real environment variables override them.
	for _, s := range settings {
		if c.envOverrides(s.env) {
			if err := s.apply(c, os.Getenv(s.env)); err != nil {
				return nil, fmt.Errorf("%s: %v", s.env, err)
			}
			c.Sources[s.key] = "env " + s.env
		}
	}
	return c, c.Validate()
}

// Validate checks bounds. A missing API key is not a validation error here:
// commands such as `config` and `doctor` must work without one (see RequireAPIKey).
func (c *Config) Validate() error {
	switch c.Approval {
	case ApprovalAsk, ApprovalEdits, ApprovalAuto, ApprovalReadOnly:
	case "":
		c.Approval = ApprovalAsk
	default:
		return fmt.Errorf("invalid approval policy %q (want ask, edits, auto or readonly)", c.Approval)
	}
	if c.MaxSteps < 1 || c.MaxSteps > MaxMaxSteps {
		return fmt.Errorf("max steps must be between 1 and %d, got %d", MaxMaxSteps, c.MaxSteps)
	}
	if c.MaxOutput < 1024 || c.MaxOutput > 1<<20 {
		return fmt.Errorf("max output must be between 1024 and 1048576 bytes, got %d", c.MaxOutput)
	}
	if c.CompactAt != 0 && c.CompactAt < MinCompactAt {
		return fmt.Errorf("compact-at must be 0 (off) or at least %d tokens, got %d", MinCompactAt, c.CompactAt)
	}
	if c.MaxRead < 4096 || c.MaxRead > 1<<20 {
		return fmt.Errorf("max read must be between 4096 and 1048576 bytes, got %d", c.MaxRead)
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
func (c *Config) TrashDir() string    { return filepath.Join(c.StateDir, "trash") }

// JournalPath is the file-change journal of one session (used by fs_changes and fs_undo).
func (c *Config) JournalPath(sessionID string) string {
	return filepath.Join(c.StateDir, "journal", sessionID+".jsonl")
}

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
	if home, err := os.UserHomeDir(); err == nil && home != "" { // honours $HOME, like UserEnvPath
		for _, n := range []string{".scoutclaw.env", ".scout.env", ".env"} {
			parseEnvFile(filepath.Join(home, n))
		}
	}
}

// dotEnvKeys records variables set from .env files (name -> file), which rank
// below config files.
var dotEnvKeys = map[string]string{}

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
			dotEnvKeys[k] = path
		}
	}
}

// UserEnvPath is the per-user env file that `scout` reads from any directory.
func UserEnvPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".scoutclaw.env"), nil
}

// SetEnvFile sets KEY=value in an env file (replacing an existing KEY line,
// keeping everything else), creating it with mode 0600 if needed.
func SetEnvFile(path, key, value string) error {
	if key == "" || strings.ContainsAny(key, "= \t\n") || strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("invalid key or value")
	}
	var lines []string
	if b, err := os.ReadFile(path); err == nil {
		lines = strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	} else if !os.IsNotExist(err) {
		return err
	}
	set := false
	for i, l := range lines {
		t := strings.TrimPrefix(strings.TrimSpace(l), "export ")
		if k, _, ok := strings.Cut(t, "="); ok && strings.TrimSpace(k) == key {
			lines[i], set = key+"="+value, true
		}
	}
	if !set {
		lines = append(lines, key+"="+value)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".scoutclaw-env-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
