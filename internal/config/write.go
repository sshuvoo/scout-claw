package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Editing config files keeps every key the user wrote; values are validated
// before anything is written.

// ParseLayer accepts global, project or local.
func ParseLayer(s string) (Layer, error) {
	switch strings.ToLower(s) {
	case "global", "user":
		return LayerGlobal, nil
	case "project", "shared":
		return LayerProject, nil
	case "local", "personal", "":
		return LayerLocal, nil
	}
	return 0, fmt.Errorf("unknown config layer %q (want global, project or local)", s)
}

func readJSONObject(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]any{"$schema": SchemaURL}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: invalid JSON: %v", path, err)
	}
	return m, nil
}

// writeLayerFile writes a config file; creating a local file also git-ignores it.
func writeLayerFile(path string, m map[string]any) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path, append(b, '\n'), 0o644); err != nil {
		return err
	}
	if filepath.Base(path) == "config.local.json" {
		return ensureGitignore(filepath.Dir(path))
	}
	return nil
}

// ensureGitignore keeps config.local.json out of version control with a
// .scout/.gitignore, so the repository's own .gitignore is never touched.
func ensureGitignore(dir string) error {
	p := filepath.Join(dir, ".gitignore")
	b, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) == "config.local.json" {
			return nil
		}
	}
	text := string(b)
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	text += "# personal ScoutClaw settings and approvals\nconfig.local.json\n"
	return os.WriteFile(p, []byte(text), 0o644)
}

// SetValue sets a scalar setting in a config file after checking the value.
func SetValue(path string, layer Layer, key, value string) error {
	var s *setting
	probe := &Config{}
	for _, st := range settingsFor(probe) {
		if st.key == key {
			st := st
			s = &st
		}
	}
	if s == nil {
		return fmt.Errorf("unknown setting %q (known: %s)", key, strings.Join(SettingKeys(), ", "))
	}
	if s.globalOnly && layer != LayerGlobal {
		return fmt.Errorf("%s may only be set in the global config (--global)", key)
	}
	if err := s.apply(probe, value); err != nil {
		return fmt.Errorf("%s: %v", key, err)
	}
	if err := checkValue(key, probe); err != nil {
		return err
	}
	m, err := readJSONObject(path)
	if err != nil {
		return err
	}
	if n, err := strconv.Atoi(value); err == nil && key != "model" && key != "baseUrl" && key != "logLevel" {
		m[key] = n
	} else {
		m[key] = strings.TrimSpace(value)
	}
	return writeLayerFile(path, m)
}

// checkValue applies the same bounds as Validate to a single setting.
func checkValue(key string, c *Config) error {
	switch key {
	case "approval":
		switch c.Approval {
		case ApprovalAsk, ApprovalEdits, ApprovalAuto, ApprovalReadOnly:
		default:
			return fmt.Errorf("approval must be ask, edits, auto or readonly")
		}
	case "maxSteps":
		if c.MaxSteps < 1 || c.MaxSteps > MaxMaxSteps {
			return fmt.Errorf("maxSteps must be between 1 and %d", MaxMaxSteps)
		}
	case "maxOutput":
		if c.MaxOutput < 1024 || c.MaxOutput > 1<<20 {
			return fmt.Errorf("maxOutput must be between 1024 and 1048576")
		}
	case "maxRead":
		if c.MaxRead < 4096 || c.MaxRead > 1<<20 {
			return fmt.Errorf("maxRead must be between 4096 and 1048576")
		}
	case "compactAt":
		if c.CompactAt != 0 && c.CompactAt < MinCompactAt {
			return fmt.Errorf("compactAt must be 0 (off) or at least %d", MinCompactAt)
		}
	case "toolTimeout", "taskTimeout":
		if c.ToolTimeout < 0 || c.TaskTimeout < 0 {
			return fmt.Errorf("%s must be positive", key)
		}
	case "model":
		if c.Model == "" {
			return fmt.Errorf("model must not be empty")
		}
	}
	return nil
}

// UnsetValue removes a setting from a config file. It reports whether it was there.
func UnsetValue(path, key string) (bool, error) {
	m, err := readJSONObject(path)
	if err != nil {
		return false, err
	}
	if _, ok := m[key]; !ok {
		return false, nil
	}
	delete(m, key)
	return true, writeLayerFile(path, m)
}

// AddRule appends a permission rule to a config file unless it is already there.
func AddRule(path, action, rule string) (bool, error) {
	switch action {
	case "allow", "ask", "deny":
	default:
		return false, fmt.Errorf("action must be allow, ask or deny, got %q", action)
	}
	m, err := readJSONObject(path)
	if err != nil {
		return false, err
	}
	perms, _ := m["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
	}
	list, _ := perms[action].([]any)
	for _, r := range list {
		if r == rule {
			return false, nil
		}
	}
	perms[action] = append(list, rule)
	m["permissions"] = perms
	return true, writeLayerFile(path, m)
}

// RemoveRule deletes a rule from every list in a config file.
func RemoveRule(path, rule string) (bool, error) {
	m, err := readJSONObject(path)
	if err != nil {
		return false, err
	}
	perms, _ := m["permissions"].(map[string]any)
	removed := false
	for _, action := range []string{"allow", "ask", "deny"} {
		list, _ := perms[action].([]any)
		kept := list[:0:0]
		for _, r := range list {
			if r == rule {
				removed = true
				continue
			}
			kept = append(kept, r)
		}
		if perms != nil && list != nil {
			perms[action] = kept
		}
	}
	if !removed {
		return false, nil
	}
	return true, writeLayerFile(path, m)
}

// InitProject creates .scout/config.json and .scout/.gitignore in root. It
// returns the files it created.
func InitProject(root string) ([]string, error) {
	dir := filepath.Join(root, scoutDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var created []string
	cfg := filepath.Join(dir, "config.json")
	if _, err := os.Stat(cfg); os.IsNotExist(err) {
		m := map[string]any{"$schema": SchemaURL, "permissions": map[string]any{"allow": []any{}, "ask": []any{}, "deny": []any{}}}
		if err := writeLayerFile(cfg, m); err != nil {
			return nil, err
		}
		created = append(created, cfg)
	}
	gi := filepath.Join(dir, ".gitignore")
	_, statErr := os.Stat(gi)
	if err := ensureGitignore(dir); err != nil {
		return nil, err
	}
	if os.IsNotExist(statErr) {
		created = append(created, gi)
	}
	return created, nil
}
