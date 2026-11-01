package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// project sets up a workspace with optional global, shared and personal
// config files and returns the workspace path.
func project(t *testing.T, global, shared, local string) string {
	t.Helper()
	clearEnv(t)
	ws := os.Getenv(EnvWorkspace)
	write := func(path, content string) {
		if content == "" {
			return
		}
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(os.Getenv(EnvConfigDir), "config.json"), global)
	write(filepath.Join(ws, ".scout", "config.json"), shared)
	write(filepath.Join(ws, ".scout", "config.local.json"), local)
	if shared == "" && local == "" {
		os.MkdirAll(filepath.Join(ws, ".scout"), 0o755)
	}
	if real, err := filepath.EvalSymlinks(ws); err == nil {
		ws = real // Load resolves the workspace through symlinks (macOS /var -> /private/var)
	}
	return ws
}

func TestLayerPrecedence(t *testing.T) {
	tests := []struct {
		name                  string
		global, shared, local string
		env                   map[string]string
		dotenv                string
		wantModel, wantSource string
	}{
		{name: "default", wantModel: DefaultModel, wantSource: "default"},
		{name: "global", global: `{"model":"g"}`, wantModel: "g", wantSource: "global"},
		{name: "project beats global", global: `{"model":"g"}`, shared: `{"model":"p"}`, wantModel: "p", wantSource: "project"},
		{name: "local beats project", global: `{"model":"g"}`, shared: `{"model":"p"}`, local: `{"model":"l"}`, wantModel: "l", wantSource: "local"},
		{name: "environment beats files", local: `{"model":"l"}`, env: map[string]string{EnvModel: "e"}, wantModel: "e", wantSource: "env"},
		{name: ".env file only fills gaps", local: `{"model":"l"}`, dotenv: "SCOUTCLAW_MODEL=d\n", wantModel: "l", wantSource: "local"},
		{name: ".env file beats defaults", dotenv: "SCOUTCLAW_MODEL=d\n", wantModel: "d", wantSource: ".env"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ws := project(t, tt.global, tt.shared, tt.local)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			dotEnvKeys = map[string]string{}
			t.Cleanup(func() { dotEnvKeys = map[string]string{} })
			if tt.dotenv != "" {
				p := filepath.Join(t.TempDir(), ".env")
				os.WriteFile(p, []byte(tt.dotenv), 0o600)
				parseEnvFile(p)
				t.Cleanup(func() { os.Unsetenv(EnvModel) })
			}
			c, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if c.Model != tt.wantModel {
				t.Fatalf("model = %q, want %q", c.Model, tt.wantModel)
			}
			src := c.Sources["model"]
			switch tt.wantSource {
			case "global":
				if !strings.HasSuffix(src, filepath.Join("config.json")) || strings.Contains(src, ws) {
					t.Fatalf("source = %q", src)
				}
			case "project":
				if src != filepath.Join(ws, ".scout", "config.json") {
					t.Fatalf("source = %q", src)
				}
			case "local":
				if src != filepath.Join(ws, ".scout", "config.local.json") {
					t.Fatalf("source = %q", src)
				}
			case "env":
				if src != "env "+EnvModel {
					t.Fatalf("source = %q", src)
				}
			case ".env":
				if !strings.HasSuffix(src, ".env") {
					t.Fatalf("source = %q", src)
				}
			default:
				if src != tt.wantSource {
					t.Fatalf("source = %q, want %q", src, tt.wantSource)
				}
			}
		})
	}
}

func TestFileValuesAndValidation(t *testing.T) {
	project(t, "", "", `{"$schema":"x","approval":"edits","maxSteps":150,"toolTimeout":"45s","compactAt":0,"maxRead":"8192"}`)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Approval != ApprovalEdits || c.MaxSteps != 150 || c.ToolTimeout.String() != "45s" || c.CompactAt != 0 || c.MaxRead != 8192 {
		t.Fatalf("values not applied: %+v", c)
	}

	errs := []struct{ name, local, want string }{
		{"invalid json", `{"model":`, "invalid JSON"},
		{"bad number", `{"maxSteps":"lots"}`, "maxSteps"},
		{"out of range", `{"maxSteps":9999}`, "max steps"},
		{"bad duration", `{"toolTimeout":"soon"}`, "toolTimeout"},
		{"bad permissions shape", `{"permissions":{"allow":"x"}}`, "permissions"},
		{"unknown permissions key", `{"permissions":{"permit":[]}}`, "permissions"},
		{"instructions not a list", `{"instructions":"AGENTS.md"}`, "instructions"},
		{"object value", `{"model":{"name":"x"}}`, "want a string or number"},
	}
	for _, tt := range errs {
		t.Run(tt.name, func(t *testing.T) {
			ws := project(t, "", "", tt.local)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if tt.name != "out of range" && !strings.Contains(err.Error(), filepath.Join(ws, ".scout", "config.local.json")) {
				t.Fatalf("error should name the file: %v", err)
			}
		})
	}
}

func TestWarnings(t *testing.T) {
	project(t, `{"baseUrl":"https://proxy.example"}`, `{"baseUrl":"https://evil.example","colour":"red"}`, `{"apiKey":"sk-123"}`)
	c, err := LoadWith(Options{TrustProject: true})
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != "https://proxy.example" {
		t.Fatalf("baseUrl must only come from the global file: %q", c.BaseURL)
	}
	all := strings.Join(c.Warnings, "\n")
	for _, w := range []string{`"baseUrl" ignored`, `unknown setting "colour"`, `"apiKey" ignored: API keys are never read`} {
		if !strings.Contains(all, w) {
			t.Errorf("missing warning %q in:\n%s", w, all)
		}
	}
	if c.APIKey == "sk-123" {
		t.Fatal("API key read from a config file")
	}
}

func TestRulesExtraDirsAndInstructions(t *testing.T) {
	extra := t.TempDir()
	ws := project(t,
		`{"permissions":{"deny":["fs_read(**/.env*)"]}}`,
		`{"permissions":{"ask":["shell_exec(git push *)"]}}`,
		`{"permissions":{"allow":["shell_exec(go test *)"],"additionalDirectories":["`+filepath.ToSlash(extra)+`","missing-dir"]},"instructions":["docs/guide.md","../outside.md"]}`)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range c.Rules {
		got = append(got, r.Action+" "+r.Text+" "+r.Layer.String())
	}
	want := []string{"deny fs_read(**/.env*) global", "ask shell_exec(git push *) project", "allow shell_exec(go test *) local"}
	if !slices.Equal(got, want) {
		t.Fatalf("rules = %v, want %v", got, want)
	}
	if len(c.ExtraDirs) != 1 || c.ExtraDirs[0] != extra {
		t.Fatalf("extra dirs = %v", c.ExtraDirs)
	}
	if len(c.Instructions) != 1 || c.Instructions[0] != filepath.Join(ws, "docs", "guide.md") {
		t.Fatalf("instructions = %v", c.Instructions)
	}
	all := strings.Join(c.Warnings, "\n")
	if !strings.Contains(all, "missing-dir does not exist") || !strings.Contains(all, "outside the project") {
		t.Fatalf("warnings: %s", all)
	}
}

func TestUntrustedSharedConfigCanOnlyTighten(t *testing.T) {
	shared := `{"approval":"auto","maxSteps":7,"permissions":{"allow":["fs_write"],"ask":["shell_exec(git *)"],"deny":["fs_delete"],"additionalDirectories":[".."]},"instructions":["NOTES.md"]}`
	project(t, "", shared, "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.Trust.Needed() || c.Trust.Trusted || len(c.Trust.Loosening) != 4 {
		t.Fatalf("trust = %+v", c.Trust)
	}
	if c.Approval != ApprovalAsk || len(c.ExtraDirs) != 0 || len(c.Instructions) != 0 {
		t.Fatalf("loosening settings applied without trust: %+v", c)
	}
	if c.MaxSteps != 7 {
		t.Fatalf("neutral settings still apply: %d", c.MaxSteps)
	}
	var actions []string
	for _, r := range c.Rules {
		actions = append(actions, r.Action)
	}
	if !slices.Equal(actions, []string{"ask", "deny"}) {
		t.Fatalf("only tightening rules may apply untrusted: %v", actions)
	}

	// trusting applies everything; changing the file revokes it
	if err := c.TrustProject(); err != nil {
		t.Fatal(err)
	}
	c, _ = Load()
	if !c.Trust.Trusted || c.Approval != ApprovalAuto || len(c.ExtraDirs) != 1 {
		t.Fatalf("trusted config not applied: %+v", c)
	}
	os.WriteFile(c.Trust.SharedPath, []byte(strings.Replace(shared, `"maxSteps":7`, `"maxSteps":8`, 1)), 0o644)
	c, _ = Load()
	if c.Trust.Trusted || c.Approval != ApprovalAsk {
		t.Fatal("a changed shared file must need trust again")
	}
	if removed, err := c.RevokeTrust(); err != nil || !removed {
		t.Fatalf("revoke: %v %v", removed, err)
	}
	if removed, _ := c.RevokeTrust(); removed {
		t.Fatal("second revoke should find nothing")
	}
	// a one-run override applies it without recording trust
	c, _ = LoadWith(Options{TrustProject: true})
	if c.Approval != ApprovalAuto || isTrusted(c.ProjectRoot, c.Trust.Hash) {
		t.Fatal("--trust-workspace must apply without saving")
	}
	// a shared file that loosens nothing needs no trust
	project(t, "", `{"maxSteps":9,"permissions":{"deny":["fs_delete"]}}`, "")
	if c, _ := Load(); c.Trust.Needed() {
		t.Fatalf("tightening-only file should not ask for trust: %+v", c.Trust)
	}
}

func TestTrustStoreFile(t *testing.T) {
	project(t, "", `{"approval":"edits"}`, "")
	c, _ := Load()
	if err := c.TrustProject(); err != nil {
		t.Fatal(err)
	}
	p, _ := trustPath()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if os.PathSeparator == '/' && st.Mode().Perm() != 0o600 {
		t.Fatalf("trust store mode = %v", st.Mode().Perm())
	}
	var ts trustStore
	b, _ := os.ReadFile(p)
	if json.Unmarshal(b, &ts) != nil || ts.Projects[c.ProjectRoot].SHA256 != c.Trust.Hash {
		t.Fatalf("trust store: %s", b)
	}
	none := &Config{ProjectRoot: t.TempDir()}
	if err := none.TrustProject(); err == nil {
		t.Fatal("trusting a project without a shared file must fail")
	}
}

func TestFindProjectRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	repo := filepath.Join(home, "code", "repo")
	deep := filepath.Join(repo, "pkg", "sub")
	os.MkdirAll(deep, 0o755)
	if got := FindProjectRoot(deep); got != deep {
		t.Fatalf("no .scout anywhere: got %s", got)
	}
	os.MkdirAll(filepath.Join(repo, ".scout"), 0o755)
	if got := FindProjectRoot(deep); got != repo {
		t.Fatalf("got %s, want %s", got, repo)
	}
	// ~/.scout is the global config directory, never a project
	os.RemoveAll(filepath.Join(repo, ".scout"))
	os.MkdirAll(filepath.Join(home, ".scout"), 0o755)
	if got := FindProjectRoot(deep); got != deep {
		t.Fatalf("home must not become the project root: got %s", got)
	}
}

func TestParseLayer(t *testing.T) {
	for in, want := range map[string]Layer{"global": LayerGlobal, "user": LayerGlobal, "project": LayerProject, "local": LayerLocal, "": LayerLocal} {
		if got, err := ParseLayer(in); err != nil || got != want {
			t.Errorf("ParseLayer(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseLayer("team"); err == nil {
		t.Error("unknown layer must fail")
	}
}
