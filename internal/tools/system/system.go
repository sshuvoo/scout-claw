// Package system provides host diagnostics. Metrics collection is implemented
// for Linux (via /proc and statfs); other platforms report what is unavailable.
package system

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"scoutclaw/internal/platform"
	"scoutclaw/internal/redact"
	"scoutclaw/internal/tools"
)

type emptyArgs struct{}
type processArgs struct {
	SortBy string `json:"sort_by"`
	Limit  int    `json:"limit"`
}
type envArgs struct {
	Prefix string `json:"prefix"`
}

func Tools() []tools.Tool {
	return []tools.Tool{
		&tools.Typed[emptyArgs]{
			ToolName: "sys_info",
			Desc:     "Report OS, kernel, uptime, load, CPU count, memory/swap and disk/inode usage per mounted filesystem.",
			Schema:   `{"type":"object","properties":{},"additionalProperties":false}`,
			Run: func(ctx context.Context, _ emptyArgs) (tools.Result, error) {
				out, err := collectSysInfo(platform.Detect())
				if err != nil {
					return tools.Result{}, err
				}
				return tools.Result{Output: out}, nil
			},
		},
		&tools.Typed[processArgs]{
			ToolName: "process_list",
			Desc:     "List the top processes by CPU or memory (CPU is sampled over ~0.5s). Command lines are redacted for secrets.",
			Schema:   `{"type":"object","properties":{"sort_by":{"type":"string","enum":["cpu","mem"]},"limit":{"type":"integer","minimum":1,"maximum":50}},"additionalProperties":false}`,
			ValidateFn: func(a *processArgs) error {
				switch a.SortBy {
				case "", "cpu", "mem":
				default:
					return fmt.Errorf("sort_by must be cpu or mem")
				}
				if a.Limit < 0 || a.Limit > 50 {
					return errors.New("limit must be 1-50")
				}
				return nil
			},
			Run: func(ctx context.Context, a processArgs) (tools.Result, error) {
				if a.SortBy == "" {
					a.SortBy = "cpu"
				}
				if a.Limit == 0 {
					a.Limit = 15
				}
				out, err := listProcesses(ctx, a.SortBy, a.Limit)
				if err != nil {
					return tools.Result{}, err
				}
				return tools.Result{Output: out}, nil
			},
		},
		&tools.Typed[envArgs]{
			ToolName: "env_list",
			Desc:     "List environment variables of the ScoutClaw process. Values of names that look secret (KEY, TOKEN, PASSWORD, ...) are masked.",
			Schema:   `{"type":"object","properties":{"prefix":{"type":"string","description":"Only names starting with this prefix."}},"additionalProperties":false}`,
			Run: func(ctx context.Context, a envArgs) (tools.Result, error) {
				var lines []string
				for _, kv := range os.Environ() {
					k, v, _ := strings.Cut(kv, "=")
					if a.Prefix != "" && !strings.HasPrefix(k, a.Prefix) {
						continue
					}
					if redact.IsSecretName(k) {
						v = redact.Mask
					}
					if len(v) > 200 {
						v = v[:200] + "…"
					}
					lines = append(lines, k+"="+v)
				}
				sort.Strings(lines)
				if len(lines) > 200 {
					lines = append(lines[:200], "[more omitted]")
				}
				return tools.Result{Output: strings.Join(lines, "\n") + "\n"}, nil
			},
		},
		&tools.Typed[emptyArgs]{
			ToolName: "dev_runtimes",
			Desc:     "Detect common development runtimes and package managers on PATH and report their versions.",
			Schema:   `{"type":"object","properties":{},"additionalProperties":false}`,
			Run: func(ctx context.Context, _ emptyArgs) (tools.Result, error) {
				return tools.Result{Output: DetectRuntimes(ctx)}, nil
			},
		},
	}
}

// runtimeProbes maps a tool to the fixed arguments used to print its version.
var runtimeProbes = []struct {
	name string
	args []string
}{
	{"go", []string{"version"}}, {"node", []string{"--version"}}, {"npm", []string{"--version"}},
	{"pnpm", []string{"--version"}}, {"yarn", []string{"--version"}}, {"bun", []string{"--version"}},
	{"python3", []string{"--version"}}, {"pip3", []string{"--version"}}, {"java", []string{"-version"}},
	{"mvn", []string{"--version"}}, {"gradle", []string{"--version"}}, {"cargo", []string{"--version"}},
	{"rustc", []string{"--version"}}, {"ruby", []string{"--version"}}, {"php", []string{"--version"}},
	{"dotnet", []string{"--version"}}, {"gcc", []string{"--version"}}, {"make", []string{"--version"}},
	{"git", []string{"--version"}}, {"docker", []string{"--version"}}, {"podman", []string{"--version"}},
	{"kubectl", []string{"version", "--client"}}, {"psql", []string{"--version"}}, {"mysql", []string{"--version"}},
	{"redis-cli", []string{"--version"}}, {"curl", []string{"--version"}},
}

func DetectRuntimes(ctx context.Context) string {
	var b strings.Builder
	for _, p := range runtimeProbes {
		path, err := exec.LookPath(p.name)
		if err != nil {
			fmt.Fprintf(&b, "%-10s not installed\n", p.name)
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		out, err := exec.CommandContext(cctx, path, p.args...).CombinedOutput()
		cancel()
		line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
		if err != nil && line == "" {
			line = "found at " + path + " (version query failed: " + err.Error() + ")"
		}
		fmt.Fprintf(&b, "%-10s %s\n", p.name, line)
	}
	return b.String()
}
