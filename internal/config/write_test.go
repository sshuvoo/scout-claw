package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

func readMap(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return m
}

func TestSetAndUnsetValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".scout", "config.local.json")
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte(`{"custom":"kept","model":"old"}`), 0o644)

	tests := []struct {
		key, value string
		layer      Layer
		want       any
		wantErr    string
	}{
		{"model", "gpt-x", LayerLocal, "gpt-x", ""},
		{"maxSteps", "150", LayerLocal, float64(150), ""},
		{"approval", "edits", LayerLocal, "edits", ""},
		{"toolTimeout", "90s", LayerLocal, "90s", ""},
		{"approval", "yolo", LayerLocal, nil, "approval must be"},
		{"maxSteps", "0", LayerLocal, nil, "maxSteps must be"},
		{"maxSteps", "many", LayerLocal, nil, "not an integer"},
		{"compactAt", "100", LayerLocal, nil, "compactAt must be"},
		{"toolTimeout", "soon", LayerLocal, nil, "not a duration"},
		{"baseUrl", "https://x", LayerLocal, nil, "only be set in the global"},
		{"colour", "red", LayerLocal, nil, "unknown setting"},
		{"model", "", LayerLocal, nil, "must not be empty"},
	}
	for _, tt := range tests {
		err := SetValue(path, tt.layer, tt.key, tt.value)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("set %s=%q: err = %v, want %q", tt.key, tt.value, err, tt.wantErr)
			}
			continue
		}
		if err != nil {
			t.Fatalf("set %s=%q: %v", tt.key, tt.value, err)
		}
		if got := readMap(t, path)[tt.key]; got != tt.want {
			t.Errorf("%s = %#v, want %#v", tt.key, got, tt.want)
		}
	}
	if readMap(t, path)["custom"] != "kept" {
		t.Fatal("unrelated keys must be preserved")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".scout", ".gitignore")); !strings.Contains(string(b), "config.local.json") {
		t.Fatal("writing the personal file must git-ignore it")
	}
	if removed, err := UnsetValue(path, "model"); err != nil || !removed {
		t.Fatalf("unset: %v %v", removed, err)
	}
	if _, ok := readMap(t, path)["model"]; ok {
		t.Fatal("model still set")
	}
	if removed, _ := UnsetValue(path, "model"); removed {
		t.Fatal("second unset should report nothing removed")
	}
	global := filepath.Join(t.TempDir(), "config.json")
	if err := SetValue(global, LayerGlobal, "baseUrl", "https://proxy"); err != nil {
		t.Fatal(err)
	}
	if m := readMap(t, global); m["baseUrl"] != "https://proxy" || m["$schema"] != SchemaURL {
		t.Fatalf("new file should get $schema: %v", m)
	}
}

func TestAddAndRemoveRules(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	for _, r := range []struct{ action, rule string }{{"allow", "fs_write"}, {"deny", "fs_delete"}, {"allow", "shell_exec(go test *)"}} {
		if added, err := AddRule(path, r.action, r.rule); err != nil || !added {
			t.Fatalf("add %v: %v %v", r, added, err)
		}
	}
	if added, _ := AddRule(path, "allow", "fs_write"); added {
		t.Fatal("duplicate rule must not be added")
	}
	if _, err := AddRule(path, "permit", "x"); err == nil {
		t.Fatal("bad action must fail")
	}
	perms := readMap(t, path)["permissions"].(map[string]any)
	var allow []string
	for _, v := range perms["allow"].([]any) {
		allow = append(allow, v.(string))
	}
	sort.Strings(allow)
	if !slices.Equal(allow, []string{"fs_write", "shell_exec(go test *)"}) {
		t.Fatalf("allow = %v", allow)
	}
	if removed, err := RemoveRule(path, "fs_write"); err != nil || !removed {
		t.Fatalf("remove: %v %v", removed, err)
	}
	if removed, _ := RemoveRule(path, "fs_write"); removed {
		t.Fatal("removing a missing rule must report false")
	}
	if removed, _ := RemoveRule(filepath.Join(t.TempDir(), "none.json"), "x"); removed {
		t.Fatal("missing file has no rules")
	}
}

func TestInitProject(t *testing.T) {
	root := t.TempDir()
	created, err := InitProject(root)
	if err != nil || len(created) != 2 {
		t.Fatalf("created %v, %v", created, err)
	}
	m := readMap(t, filepath.Join(root, ".scout", "config.json"))
	if m["$schema"] != SchemaURL || m["permissions"] == nil {
		t.Fatalf("config.json = %v", m)
	}
	again, err := InitProject(root)
	if err != nil || len(again) != 0 {
		t.Fatalf("init must be idempotent: %v %v", again, err)
	}
	gi, _ := os.ReadFile(filepath.Join(root, ".scout", ".gitignore"))
	if strings.Count(string(gi), "config.local.json") != 1 {
		t.Fatalf(".gitignore = %q", gi)
	}
}

func TestSchemaMatchesSettings(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "schema", "config.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]struct {
			Properties map[string]any `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(b, &schema); err != nil {
		t.Fatal(err)
	}
	var got []string
	for k := range schema.Properties {
		got = append(got, k)
	}
	want := append(SettingKeys(), "$schema", "permissions", "instructions")
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("schema properties %v, want %v", got, want)
	}
	var perms []string
	for k := range schema.Properties["permissions"].Properties {
		perms = append(perms, k)
	}
	sort.Strings(perms)
	if !slices.Equal(perms, []string{"additionalDirectories", "allow", "ask", "deny"}) {
		t.Fatalf("permissions properties %v", perms)
	}
}
