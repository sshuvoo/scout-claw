package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Config files, lowest precedence first: ~/.scout/config.json (global),
// <project>/.scout/config.json (shared with the team, committed) and
// <project>/.scout/config.local.json (personal, git-ignored). Environment
// variables and flags override all of them; .env files only fill in what no
// config file sets.

// Layer says where a setting came from, lowest precedence first.
type Layer int

const (
	LayerDefault Layer = iota
	LayerDotEnv
	LayerGlobal
	LayerProject
	LayerLocal
	LayerEnv
	LayerFlag
)

func (l Layer) String() string {
	return [...]string{"default", ".env file", "global", "project", "local", "environment", "flag"}[l]
}

const (
	EnvConfigDir = "SCOUT_CONFIG_DIR"
	SchemaURL    = "https://raw.githubusercontent.com/sshuvoo/scout-claw/main/schema/config.schema.json"
	scoutDir     = ".scout"
)

// RuleSpec is one permission rule as written in a config file. The policy
// engine parses and evaluates it.
type RuleSpec struct {
	Action string // allow, ask or deny
	Text   string // e.g. shell_exec(go test *)
	Layer  Layer
	Path   string // config file it came from
}

// Trust describes whether the shared project file may loosen anything.
type Trust struct {
	SharedPath string   // <project>/.scout/config.json, if it exists
	Hash       string   // sha256 of that file
	Trusted    bool     // trusted with this exact content
	Loosening  []string // what it would loosen; ignored unless Trusted
}

// Needed reports whether a trust decision is pending.
func (t Trust) Needed() bool { return len(t.Loosening) > 0 && !t.Trusted }

// setting describes one scalar configuration value.
type setting struct {
	key        string // camelCase key in config files
	env        string
	globalOnly bool // never read from project files (see baseUrl)
	apply      func(c *Config, v string) error
}

func parseInt(dst *int) func(*Config, string) error {
	return func(_ *Config, v string) error {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("%q is not an integer", v)
		}
		*dst = n
		return nil
	}
}

func parseDuration(dst *time.Duration) func(*Config, string) error {
	return func(_ *Config, v string) error {
		d, err := time.ParseDuration(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("%q is not a duration (e.g. 90s, 5m)", v)
		}
		*dst = d
		return nil
	}
}

func settingsFor(c *Config) []setting {
	str := func(dst *string) func(*Config, string) error {
		return func(_ *Config, v string) error { *dst = strings.TrimSpace(v); return nil }
	}
	return []setting{
		{key: "model", env: EnvModel, apply: str(&c.Model)},
		{key: "baseUrl", env: EnvBaseURL, globalOnly: true, apply: str(&c.BaseURL)},
		{key: "approval", env: EnvApproval, apply: func(c *Config, v string) error {
			c.Approval = Approval(strings.ToLower(strings.TrimSpace(v)))
			return nil
		}},
		{key: "maxSteps", env: EnvMaxSteps, apply: parseInt(&c.MaxSteps)},
		{key: "toolTimeout", env: EnvToolTimeout, apply: parseDuration(&c.ToolTimeout)},
		{key: "taskTimeout", env: EnvTaskTimeout, apply: parseDuration(&c.TaskTimeout)},
		{key: "maxOutput", env: EnvMaxOutput, apply: parseInt(&c.MaxOutput)},
		{key: "maxRead", env: EnvMaxRead, apply: parseInt(&c.MaxRead)},
		{key: "compactAt", env: EnvCompactAt, apply: parseInt(&c.CompactAt)},
		{key: "logLevel", env: EnvLogLevel, apply: str(&c.LogLevel)},
	}
}

// SettingKeys lists the scalar keys accepted in config files.
func SettingKeys() []string {
	var out []string
	for _, s := range settingsFor(&Config{}) {
		out = append(out, s.key)
	}
	return out
}

// fileData is a parsed config file.
type fileData struct {
	path        string
	layer       Layer
	values      map[string]string // scalar settings, as strings
	allow       []string
	ask         []string
	deny        []string
	extraDirs   []string
	instruction []string
	raw         []byte
}

var apiKeyNames = map[string]bool{"apiKey": true, "api_key": true, "openaiApiKey": true, "OPENAI_API_KEY": true, "key": true}

// readFile parses one config file. A missing file returns nil without error.
func readFile(path string, layer Layer, warn func(string)) (*fileData, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("%s: invalid JSON: %v", path, err)
	}
	fd := &fileData{path: path, layer: layer, values: map[string]string{}, raw: raw}
	known := map[string]bool{"$schema": true, "permissions": true, "instructions": true}
	for _, k := range SettingKeys() {
		known[k] = true
	}
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := top[k]
		switch {
		case apiKeyNames[k]:
			warn(fmt.Sprintf("%s: %q ignored: API keys are never read from config files (use scout setup)", path, k))
		case !known[k]:
			warn(fmt.Sprintf("%s: unknown setting %q ignored", path, k))
		case k == "$schema":
		case k == "permissions":
			var p struct {
				Allow                 []string `json:"allow"`
				Ask                   []string `json:"ask"`
				Deny                  []string `json:"deny"`
				AdditionalDirectories []string `json:"additionalDirectories"`
			}
			dec := json.NewDecoder(bytes.NewReader(v))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&p); err != nil {
				return nil, fmt.Errorf("%s: permissions: %v (want allow, ask, deny and additionalDirectories lists of strings)", path, err)
			}
			fd.allow, fd.ask, fd.deny, fd.extraDirs = p.Allow, p.Ask, p.Deny, p.AdditionalDirectories
		case k == "instructions":
			if err := json.Unmarshal(v, &fd.instruction); err != nil {
				return nil, fmt.Errorf("%s: instructions: want a list of file paths", path)
			}
		default:
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				var n json.Number
				dec := json.NewDecoder(bytes.NewReader(v))
				dec.UseNumber()
				if err := dec.Decode(&n); err != nil {
					return nil, fmt.Errorf("%s: %s: want a string or number", path, k)
				}
				s = n.String()
			}
			fd.values[k] = s
		}
	}
	return fd, nil
}

// ConfigDir is the directory of the global config file and trust store.
func ConfigDir() (string, error) {
	if d := os.Getenv(EnvConfigDir); d != "" {
		return filepath.Abs(d)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, scoutDir), nil
}

// FindProjectRoot returns the nearest directory at or above workspace that
// has a .scout folder, stopping at the home directory; otherwise workspace.
func FindProjectRoot(workspace string) string {
	home, _ := os.UserHomeDir()
	for dir := workspace; ; dir = filepath.Dir(dir) {
		if st, err := os.Stat(filepath.Join(dir, scoutDir)); err == nil && st.IsDir() && dir != home {
			return dir
		}
		if dir == home || filepath.Dir(dir) == dir {
			return workspace
		}
	}
}

// LayerPath is the config file of a layer for this configuration.
func (c *Config) LayerPath(l Layer) (string, error) {
	switch l {
	case LayerGlobal:
		d, err := ConfigDir()
		return filepath.Join(d, "config.json"), err
	case LayerProject:
		return filepath.Join(c.ProjectRoot, scoutDir, "config.json"), nil
	case LayerLocal:
		return filepath.Join(c.ProjectRoot, scoutDir, "config.local.json"), nil
	}
	return "", fmt.Errorf("layer %s has no config file", l)
}

func hashBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// loosening lists what a shared project file would relax.
func loosening(fd *fileData) []string {
	var out []string
	if a := strings.ToLower(fd.values["approval"]); a == string(ApprovalAuto) || a == string(ApprovalEdits) {
		out = append(out, fmt.Sprintf("approval %q (runs actions without asking)", a))
	}
	for _, r := range fd.allow {
		out = append(out, "allow rule "+r)
	}
	for _, d := range fd.extraDirs {
		out = append(out, "extra writable directory "+d)
	}
	for _, f := range fd.instruction {
		out = append(out, "instruction file "+f+" (added to the model's context)")
	}
	return out
}

// restrictToTightening removes everything loosening from an untrusted file.
func restrictToTightening(fd *fileData) {
	if a := strings.ToLower(fd.values["approval"]); a == string(ApprovalAuto) || a == string(ApprovalEdits) {
		delete(fd.values, "approval")
	}
	fd.allow, fd.extraDirs, fd.instruction = nil, nil, nil
}

// loadFiles reads the three config files and applies them to c in order.
// envSet reports environment variables that override files.
func (c *Config) loadFiles(trustOverride bool) error {
	warn := func(s string) { c.Warnings = append(c.Warnings, s) }
	var layers []*fileData
	for _, l := range []Layer{LayerGlobal, LayerProject, LayerLocal} {
		path, err := c.LayerPath(l)
		if err != nil {
			if l == LayerGlobal {
				continue // no home directory: no global file
			}
			return err
		}
		fd, err := readFile(path, l, warn)
		if err != nil {
			return err
		}
		if fd == nil {
			continue
		}
		if l == LayerProject {
			c.Trust = Trust{SharedPath: path, Hash: hashBytes(fd.raw), Loosening: loosening(fd)}
			c.Trust.Trusted = trustOverride || isTrusted(c.ProjectRoot, c.Trust.Hash)
			if !c.Trust.Trusted {
				restrictToTightening(fd)
			}
		}
		layers = append(layers, fd)
	}
	byKey := map[string]setting{}
	for _, s := range settingsFor(c) {
		byKey[s.key] = s
	}
	for _, fd := range layers {
		keys := make([]string, 0, len(fd.values))
		for k := range fd.values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s := byKey[k]
			if s.globalOnly && fd.layer != LayerGlobal {
				warn(fmt.Sprintf("%s: %q ignored: it may only be set in the global config, environment or flags", fd.path, k))
				continue
			}
			if c.envOverrides(s.env) {
				continue
			}
			if err := s.apply(c, fd.values[k]); err != nil {
				return fmt.Errorf("%s: %s: %v", fd.path, k, err)
			}
			c.Sources[k] = fd.path
		}
		for _, group := range []struct {
			action string
			list   []string
		}{{"allow", fd.allow}, {"ask", fd.ask}, {"deny", fd.deny}} { // fixed order: rule lists are reported as written
			for _, r := range group.list {
				c.Rules = append(c.Rules, RuleSpec{Action: group.action, Text: strings.TrimSpace(r), Layer: fd.layer, Path: fd.path})
			}
		}
		for _, d := range fd.extraDirs {
			abs := c.resolveFrom(fd, d)
			if st, err := os.Stat(abs); err != nil || !st.IsDir() {
				warn(fmt.Sprintf("%s: additional directory %s does not exist; ignored", fd.path, abs))
				continue
			}
			c.ExtraDirs = append(c.ExtraDirs, abs)
		}
		for _, f := range fd.instruction {
			abs := c.resolveFrom(fd, f)
			if fd.layer != LayerGlobal && !within(c.ProjectRoot, abs) {
				warn(fmt.Sprintf("%s: instruction file %s is outside the project; ignored", fd.path, f))
				continue
			}
			c.Instructions = append(c.Instructions, abs)
		}
	}
	sort.SliceStable(c.Rules, func(i, j int) bool { return c.Rules[i].Layer < c.Rules[j].Layer })
	return nil
}

// resolveFrom resolves a path written in a config file: "~/" is the home
// directory, relative paths are relative to the project root.
func (c *Config) resolveFrom(fd *fileData, p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(c.ProjectRoot, p)
	}
	return filepath.Clean(p)
}

func within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// envOverrides reports whether a real environment variable (not one loaded
// from a .env file) is set for this setting, so files must not override it.
func (c *Config) envOverrides(env string) bool {
	if env == "" || strings.TrimSpace(os.Getenv(env)) == "" {
		return false
	}
	_, fromDotEnv := dotEnvKeys[env]
	return !fromDotEnv
}

// trust store: ~/.scout/trusted.json

type trustStore struct {
	Projects map[string]trustEntry `json:"projects"`
}

type trustEntry struct {
	SHA256    string    `json:"sha256"`
	TrustedAt time.Time `json:"trusted_at"`
}

func trustPath() (string, error) {
	d, err := ConfigDir()
	return filepath.Join(d, "trusted.json"), err
}

func readTrust() trustStore {
	ts := trustStore{Projects: map[string]trustEntry{}}
	p, err := trustPath()
	if err != nil {
		return ts
	}
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &ts)
	}
	if ts.Projects == nil {
		ts.Projects = map[string]trustEntry{}
	}
	return ts
}

func isTrusted(root, hash string) bool {
	e, ok := readTrust().Projects[root]
	return ok && e.SHA256 == hash
}

// TrustProject records that the user trusts the shared config of this
// project with its current content. Any later change asks again.
func (c *Config) TrustProject() error {
	if c.Trust.SharedPath == "" {
		return fmt.Errorf("%s has no shared .scout/config.json to trust", c.ProjectRoot)
	}
	ts := readTrust()
	ts.Projects[c.ProjectRoot] = trustEntry{SHA256: c.Trust.Hash, TrustedAt: time.Now().UTC()}
	return writeTrust(ts)
}

// RevokeTrust forgets the trust decision for this project.
func (c *Config) RevokeTrust() (bool, error) {
	ts := readTrust()
	if _, ok := ts.Projects[c.ProjectRoot]; !ok {
		return false, nil
	}
	delete(ts.Projects, c.ProjectRoot)
	return true, writeTrust(ts)
}

func writeTrust(ts trustStore) error {
	p, err := trustPath()
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(ts, "", "  ")
	return writeFileAtomic(p, append(b, '\n'), 0o600)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
