package filesystem

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"scoutclaw/internal/tools"
)

// project_info reads manifests and conventions so the agent knows how to
// build, test and lint a project before it changes anything. It only reads
// files; it never runs the commands it suggests.

type projectArgs struct {
	Path string `json:"path"`
}

// projectKind is one detected ecosystem with its suggested commands.
type projectKind struct {
	name     string
	details  []string
	commands [][2]string // label, command
}

func (f FS) projectTool() tools.Tool {
	return &tools.Typed[projectArgs]{
		ToolName: "project_info",
		Desc: "Describe the project in a directory (default: workspace) by reading its manifests: languages, package manager, " +
			"build/test/lint/format commands, Makefile targets, CI workflows, instruction files (AGENTS.md, CONTRIBUTING.md, ...), git branch and nested projects. " +
			"Call it before changing code to learn how to verify your change. Read-only; it does not run anything.",
		Schema: `{"type":"object","properties":{"path":{"type":"string"}},"additionalProperties":false}`,
		ValidateFn: func(a *projectArgs) error {
			if a.Path == "" {
				a.Path = "."
			}
			return needPath(a.Path)
		},
		Run: f.projectInfo,
	}
}

// hasFile reports whether dir contains name (a file or directory).
func hasFile(dir, name string) bool { _, err := os.Stat(filepath.Join(dir, name)); return err == nil }

func readSmall(path string) string {
	st, err := os.Stat(path)
	if err != nil || st.Size() > 512*1024 {
		return ""
	}
	b, _ := os.ReadFile(path)
	return string(b)
}

func detectGo(dir string) *projectKind {
	mod := readSmall(filepath.Join(dir, "go.mod"))
	if mod == "" {
		return nil
	}
	k := &projectKind{name: "Go"}
	for _, l := range strings.Split(mod, "\n") {
		l = strings.TrimSpace(l)
		if v, ok := strings.CutPrefix(l, "module "); ok {
			k.details = append(k.details, "module "+v)
		} else if v, ok := strings.CutPrefix(l, "go "); ok {
			k.details = append(k.details, "go "+v)
		}
	}
	k.commands = [][2]string{{"build", "go build ./..."}, {"test", "go test ./..."}, {"lint", "go vet ./..."}, {"format", "gofmt -l . (gofmt -w <files> to fix)"}}
	return k
}

func detectNode(dir string) *projectKind {
	raw := readSmall(filepath.Join(dir, "package.json"))
	if raw == "" {
		return nil
	}
	var pkg struct {
		Name           string            `json:"name"`
		Scripts        map[string]string `json:"scripts"`
		PackageManager string            `json:"packageManager"`
		Workspaces     json.RawMessage   `json:"workspaces"`
	}
	k := &projectKind{name: "JavaScript/TypeScript"}
	if err := json.Unmarshal([]byte(raw), &pkg); err != nil {
		k.details = append(k.details, "package.json could not be parsed: "+err.Error())
		return k
	}
	pm := "npm"
	switch {
	case strings.HasPrefix(pkg.PackageManager, "pnpm"), hasFile(dir, "pnpm-lock.yaml"):
		pm = "pnpm"
	case strings.HasPrefix(pkg.PackageManager, "yarn"), hasFile(dir, "yarn.lock"):
		pm = "yarn"
	case strings.HasPrefix(pkg.PackageManager, "bun"), hasFile(dir, "bun.lockb"), hasFile(dir, "bun.lock"):
		pm = "bun"
	}
	if pkg.Name != "" {
		k.details = append(k.details, "package "+pkg.Name)
	}
	k.details = append(k.details, "package manager "+pm)
	if hasFile(dir, "tsconfig.json") {
		k.details = append(k.details, "TypeScript (tsconfig.json)")
	}
	if len(pkg.Workspaces) > 0 || hasFile(dir, "pnpm-workspace.yaml") {
		k.details = append(k.details, "monorepo workspaces")
	}
	if !hasFile(dir, "node_modules") {
		k.details = append(k.details, "dependencies not installed (no node_modules): run "+pm+" install first")
	}
	run := func(s string) string {
		if pm == "npm" && s != "test" && s != "start" {
			return "npm run " + s
		}
		return pm + " " + s
	}
	for _, label := range []string{"build", "test", "lint", "typecheck", "format", "dev", "start"} {
		if _, ok := pkg.Scripts[label]; ok {
			k.commands = append(k.commands, [2]string{label, run(label)})
		}
	}
	if len(pkg.Scripts) > 0 {
		var names []string
		for n := range pkg.Scripts {
			names = append(names, n)
		}
		sort.Strings(names)
		k.details = append(k.details, "scripts: "+strings.Join(names, ", "))
	}
	return k
}

func detectPython(dir string) *projectKind {
	py := readSmall(filepath.Join(dir, "pyproject.toml"))
	if py == "" && !hasFile(dir, "setup.py") && !hasFile(dir, "requirements.txt") && !hasFile(dir, "setup.cfg") {
		return nil
	}
	k := &projectKind{name: "Python"}
	runner := ""
	switch {
	case hasFile(dir, "uv.lock"):
		runner, k.details = "uv run ", append(k.details, "package manager uv")
	case strings.Contains(py, "[tool.poetry]"), hasFile(dir, "poetry.lock"):
		runner, k.details = "poetry run ", append(k.details, "package manager poetry")
	case hasFile(dir, "Pipfile"):
		runner, k.details = "pipenv run ", append(k.details, "package manager pipenv")
	}
	reqs := readSmall(filepath.Join(dir, "requirements.txt")) + readSmall(filepath.Join(dir, "requirements-dev.txt"))
	if strings.Contains(py, "pytest") || strings.Contains(reqs, "pytest") || hasFile(dir, "pytest.ini") || hasFile(dir, "conftest.py") || hasFile(dir, "tests") {
		k.commands = append(k.commands, [2]string{"test", runner + "pytest"})
	} else {
		k.commands = append(k.commands, [2]string{"test", runner + "python -m unittest"})
	}
	if strings.Contains(py, "[tool.ruff") || strings.Contains(reqs, "ruff") || hasFile(dir, "ruff.toml") {
		k.commands = append(k.commands, [2]string{"lint", runner + "ruff check ."}, [2]string{"format", runner + "ruff format ."})
	}
	if strings.Contains(py, "[tool.mypy") || hasFile(dir, "mypy.ini") {
		k.commands = append(k.commands, [2]string{"typecheck", runner + "mypy ."})
	}
	if strings.Contains(py, "[tool.black") {
		k.commands = append(k.commands, [2]string{"format", runner + "black ."})
	}
	return k
}

func detectSimple(dir string) []*projectKind {
	var out []*projectKind
	if hasFile(dir, "Cargo.toml") {
		out = append(out, &projectKind{name: "Rust", commands: [][2]string{{"build", "cargo build"}, {"test", "cargo test"}, {"lint", "cargo clippy"}, {"format", "cargo fmt"}}})
	}
	if hasFile(dir, "pom.xml") {
		out = append(out, &projectKind{name: "Java (Maven)", commands: [][2]string{{"build", "mvn -q package -DskipTests"}, {"test", "mvn -q test"}}})
	}
	for _, g := range []string{"build.gradle", "build.gradle.kts"} {
		if hasFile(dir, g) {
			gradle := "gradle"
			if hasFile(dir, "gradlew") {
				gradle = "./gradlew"
			}
			out = append(out, &projectKind{name: "JVM (Gradle)", commands: [][2]string{{"build", gradle + " build -x test"}, {"test", gradle + " test"}}})
			break
		}
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*.sln")); len(m) > 0 {
		out = append(out, &projectKind{name: ".NET", commands: [][2]string{{"build", "dotnet build"}, {"test", "dotnet test"}, {"format", "dotnet format"}}})
	} else if m, _ := filepath.Glob(filepath.Join(dir, "*.csproj")); len(m) > 0 {
		out = append(out, &projectKind{name: ".NET", commands: [][2]string{{"build", "dotnet build"}, {"test", "dotnet test"}, {"format", "dotnet format"}}})
	}
	if hasFile(dir, "Gemfile") {
		k := &projectKind{name: "Ruby", commands: [][2]string{{"install", "bundle install"}}}
		if hasFile(dir, "spec") {
			k.commands = append(k.commands, [2]string{"test", "bundle exec rspec"})
		} else {
			k.commands = append(k.commands, [2]string{"test", "bundle exec rake test"})
		}
		out = append(out, k)
	}
	if hasFile(dir, "composer.json") {
		out = append(out, &projectKind{name: "PHP", commands: [][2]string{{"install", "composer install"}, {"test", "vendor/bin/phpunit"}}})
	}
	return out
}

var makeTarget = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9_.-]*)\s*:([^=]|$)`)

func makeTargets(dir string) []string {
	for _, name := range []string{"Makefile", "makefile", "GNUmakefile"} {
		file, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		defer file.Close()
		var out []string
		seen := map[string]bool{}
		sc := bufio.NewScanner(file)
		for sc.Scan() {
			if m := makeTarget.FindStringSubmatch(sc.Text()); m != nil && !seen[m[1]] {
				seen[m[1]] = true
				out = append(out, m[1])
			}
		}
		return out
	}
	return nil
}

func gitBranch(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		head := readSmall(filepath.Join(d, ".git", "HEAD"))
		if head != "" {
			if ref, ok := strings.CutPrefix(strings.TrimSpace(head), "ref: refs/heads/"); ok {
				return ref
			}
			return "detached at " + short(strings.TrimSpace(head))
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

func detectAll(dir string) []*projectKind {
	var kinds []*projectKind
	for _, k := range []*projectKind{detectGo(dir), detectNode(dir), detectPython(dir)} {
		if k != nil {
			kinds = append(kinds, k)
		}
	}
	return append(kinds, detectSimple(dir)...)
}

func (f FS) projectInfo(ctx context.Context, a projectArgs) (tools.Result, error) {
	dir := f.resolve(a.Path)
	st, err := os.Stat(dir)
	if err != nil {
		return tools.Result{}, mapErr(err)
	}
	if !st.IsDir() {
		return tools.Result{}, tools.Errorf(tools.KindInvalidInput, "%s is not a directory", dir)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "project: %s\n", f.rel(dir))
	if br := gitBranch(dir); br != "" {
		fmt.Fprintf(&b, "git branch: %s (run git status for local changes)\n", br)
	}
	var docs []string
	for _, n := range []string{"AGENTS.md", "SCOUT.md", "CLAUDE.md", ".github/copilot-instructions.md", "CONTRIBUTING.md", "README.md"} {
		if hasFile(dir, n) {
			docs = append(docs, n)
		}
	}
	if len(docs) > 0 {
		fmt.Fprintf(&b, "instructions: %s (read the first ones before changing code)\n", strings.Join(docs, ", "))
	}

	kinds := detectAll(dir)
	if len(kinds) == 0 {
		b.WriteString("\nno known project manifest here (go.mod, package.json, pyproject.toml, Cargo.toml, pom.xml, ...)\n")
	}
	for _, k := range kinds {
		fmt.Fprintf(&b, "\n%s\n", k.name)
		for _, d := range k.details {
			fmt.Fprintf(&b, "  %s\n", d)
		}
		for _, c := range k.commands {
			fmt.Fprintf(&b, "  %-9s %s\n", c[0]+":", c[1])
		}
	}
	if targets := makeTargets(dir); len(targets) > 0 {
		fmt.Fprintf(&b, "\nMakefile targets: %s\n", strings.Join(targets, ", "))
		b.WriteString("  (prefer `make <target>` when it wraps the commands above)\n")
	}
	if wf, _ := filepath.Glob(filepath.Join(dir, ".github", "workflows", "*.y*ml")); len(wf) > 0 {
		var names []string
		for _, w := range wf {
			names = append(names, f.rel(w))
		}
		fmt.Fprintf(&b, "\nCI workflows: %s (they show the exact commands CI runs)\n", strings.Join(names, ", "))
	}

	// Nested projects (monorepos), up to two levels down.
	var nested []string
	ign := newIgnorer(dir)
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || ctx.Err() != nil || !d.IsDir() || p == dir {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".") || ign.ignored(p, true) || strings.Count(rel, string(filepath.Separator)) > 1 {
			return filepath.SkipDir
		}
		if ks := detectAll(p); len(ks) > 0 {
			var names []string
			for _, k := range ks {
				names = append(names, k.name)
			}
			nested = append(nested, fmt.Sprintf("%s (%s)", f.rel(p), strings.Join(names, ", ")))
		}
		return nil
	})
	if len(nested) > 0 {
		fmt.Fprintf(&b, "\nnested projects: %s\n", strings.Join(nested, "; "))
	}
	return tools.Result{Output: b.String()}, nil
}
