package filesystem

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectInfo(t *testing.T) {
	tests := []struct {
		name     string
		files    map[string]string
		want     []string
		excluded []string
	}{
		{
			name:     "go module with Makefile, CI and instructions",
			files:    map[string]string{"go.mod": "module example.com/app\n\ngo 1.22\n", "Makefile": "build:\n\tgo build\ntest: build\n\tgo test\nVAR := 1\n.PHONY: test\n", ".github/workflows/ci.yml": "on: push\n", "AGENTS.md": "rules"},
			want:     []string{"Go", "module example.com/app", "go 1.22", "test:     go test ./...", "Makefile targets: build, test", "CI workflows: " + filepath.Join(".github", "workflows", "ci.yml"), "instructions: AGENTS.md"},
			excluded: []string{"VAR", ".PHONY"},
		},
		{
			name:  "pnpm project with scripts",
			files: map[string]string{"package.json": `{"name":"web","scripts":{"build":"vite build","test":"vitest","lint":"eslint ."}}`, "pnpm-lock.yaml": "", "tsconfig.json": "{}"},
			want:  []string{"JavaScript/TypeScript", "package web", "package manager pnpm", "TypeScript", "build:    pnpm build", "test:     pnpm test", "lint:     pnpm lint", "not installed"},
		},
		{
			name:     "npm uses run for scripts other than test",
			files:    map[string]string{"package.json": `{"scripts":{"build":"tsc","test":"jest"}}`, "node_modules/.keep": ""},
			want:     []string{"package manager npm", "build:    npm run build", "test:     npm test"},
			excluded: []string{"not installed"},
		},
		{
			name:  "broken package.json is reported, not fatal",
			files: map[string]string{"package.json": `{"name":`},
			want:  []string{"could not be parsed"},
		},
		{
			name:  "python with uv, pytest and ruff",
			files: map[string]string{"pyproject.toml": "[project]\nname='x'\n[tool.ruff]\n[tool.pytest.ini_options]\n", "uv.lock": ""},
			want:  []string{"Python", "package manager uv", "test:     uv run pytest", "lint:     uv run ruff check ."},
		},
		{
			name:  "plain requirements without pytest",
			files: map[string]string{"requirements.txt": "requests\n"},
			want:  []string{"Python", "python -m unittest"},
		},
		{
			name:  "rust, gradle wrapper, dotnet",
			files: map[string]string{"Cargo.toml": "", "build.gradle.kts": "", "gradlew": "", "App.csproj": ""},
			want:  []string{"Rust", "cargo test", "./gradlew test", ".NET", "dotnet test"},
		},
		{
			name:     "monorepo lists nested projects but not ignored ones",
			files:    map[string]string{".gitignore": "vendor/\n", "services/api/go.mod": "module api\n", "web/package.json": "{}", "vendor/lib/go.mod": "module lib\n", "node_modules/x/package.json": "{}"},
			want:     []string{"no known project manifest here", "nested projects:", filepath.Join("services", "api") + " (Go)", "web (JavaScript/TypeScript)"},
			excluded: []string{"vendor", "node_modules"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ws, m := setup(t)
			for p, c := range tt.files {
				mustWrite(t, filepath.Join(ws, filepath.FromSlash(p)), c)
			}
			r, err := run(t, m["project_info"], context.Background(), map[string]any{})
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tt.want {
				if !strings.Contains(r.Output, w) {
					t.Errorf("missing %q in:\n%s", w, r.Output)
				}
			}
			for _, x := range tt.excluded {
				if strings.Contains(r.Output, x) {
					t.Errorf("%q should not appear in:\n%s", x, r.Output)
				}
			}
		})
	}
}

func TestProjectInfoGitBranchAndErrors(t *testing.T) {
	_, ws, m := setup(t)
	mustWrite(t, filepath.Join(ws, ".git", "HEAD"), "ref: refs/heads/feature/x\n")
	os.Mkdir(filepath.Join(ws, "sub"), 0o755)
	r, err := run(t, m["project_info"], context.Background(), map[string]any{"path": "sub"})
	if err != nil || !strings.Contains(r.Output, "git branch: feature/x") {
		t.Fatalf("branch from a parent .git: %v %q", err, r.Output)
	}
	mustWrite(t, filepath.Join(ws, ".git", "HEAD"), "0123456789abcdef0123\n")
	if r, _ := run(t, m["project_info"], context.Background(), map[string]any{}); !strings.Contains(r.Output, "detached at 0123456789ab") {
		t.Fatalf("detached head: %q", r.Output)
	}
	mustWrite(t, filepath.Join(ws, "f.txt"), "x")
	if _, err := run(t, m["project_info"], context.Background(), map[string]any{"path": "f.txt"}); err == nil {
		t.Fatal("a file path must be rejected")
	}
	if _, err := run(t, m["project_info"], context.Background(), map[string]any{"path": "missing"}); err == nil {
		t.Fatal("a missing path must be rejected")
	}
}
