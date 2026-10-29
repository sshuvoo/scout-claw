package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"scoutclaw/internal/audit"
	"scoutclaw/internal/config"
	"scoutclaw/internal/llm"
	"scoutclaw/internal/platform"
	"scoutclaw/internal/redact"
	"scoutclaw/internal/session"
	"scoutclaw/internal/update"
)

func (a *App) versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the ScoutClaw version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintf(a.Out, "scout %s (%s, %s/%s)\n", Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		},
	}
}

func (a *App) configCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "config",
		Short: "Show the effective configuration and where each value comes from",
		Long: `Show the effective configuration. Precedence (highest first):
  1. command-line flags (--model, --approval, --max-steps, --workspace)
  2. environment variables
  3. .env files: ./.env, ~/.scoutclaw.env, ~/.scout.env, ~/.env (never override real environment variables)
  4. built-in defaults
Secrets are never printed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadConfig(cmd)
			if err != nil {
				return err
			}
			a.printConfig(cfg, a.Out)
			return nil
		},
	}
}

func (a *App) statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show agent configuration and recorded session state (no secrets)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadConfig(cmd)
			if err != nil {
				return err
			}
			p := platform.Detect()
			fmt.Fprintf(a.Out, "version      %s\nplatform     %s/%s %s (full diagnostics: %v)\n", Version, p.OS, p.Arch, p.Distro, p.FullySupported)
			fmt.Fprintf(a.Out, "model        %s   api key: %s\napproval     %s   workspace: %s\n", cfg.Model, cfg.MaskedKey(), cfg.Approval, cfg.Workspace)
			fmt.Fprintf(a.Out, "limits       %d steps/task, %s per tool, %s per task, %d bytes output\n", cfg.MaxSteps, cfg.ToolTimeout, cfg.TaskTimeout, cfg.MaxOutput)
			sessions, _ := session.List(cfg.SessionsDir())
			tasks := 0
			for _, s := range sessions {
				tasks += len(s.Tasks)
			}
			fmt.Fprintf(a.Out, "state dir    %s\nsessions     %d (%d tasks)\n", cfg.StateDir, len(sessions), tasks)
			if len(sessions) > 0 {
				s := sessions[0]
				fmt.Fprintf(a.Out, "last session %s (updated %s", s.ID, s.Updated.Local().Format("2006-01-02 15:04"))
				if n := len(s.Tasks); n > 0 {
					fmt.Fprintf(a.Out, ", last task: %s/%s", s.Tasks[n-1].Status, orStr(s.Tasks[n-1].Verified, "-"))
				}
				fmt.Fprintln(a.Out, ")")
			}
			if st, err := os.Stat(cfg.AuditPath()); err == nil {
				fmt.Fprintf(a.Out, "audit log    %s (%d bytes)\n", cfg.AuditPath(), st.Size())
			} else {
				fmt.Fprintf(a.Out, "audit log    %s (not created yet)\n", cfg.AuditPath())
			}
			return nil
		},
	}
}

func (a *App) historyCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "history [session-id]",
		Short: "List recorded sessions, or show one session's tasks and actions",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadConfig(cmd)
			if err != nil {
				return err
			}
			if len(args) == 1 {
				return a.showSession(cfg, args[0])
			}
			sessions, err := session.List(cfg.SessionsDir())
			if err != nil {
				return err
			}
			if len(sessions) == 0 {
				fmt.Fprintln(a.Out, "no history yet")
				return nil
			}
			for i, s := range sessions {
				if i >= limit {
					fmt.Fprintf(a.Out, "… %d older sessions (use --limit)\n", len(sessions)-limit)
					break
				}
				last := "-"
				if n := len(s.Tasks); n > 0 {
					t := s.Tasks[n-1]
					last = fmt.Sprintf("[%s/%s] %s", t.Status, orStr(t.Verified, "-"), clip(t.Prompt, 60))
				}
				fmt.Fprintf(a.Out, "%s  %s  %d task(s)  %s\n", s.ID, s.Updated.Local().Format("2006-01-02 15:04"), len(s.Tasks), last)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "maximum sessions to list")
	return cmd
}

func (a *App) showSession(cfg *config.Config, id string) error {
	s, err := session.Load(cfg.SessionsDir(), id)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "session %s · %s · model %s · workspace %s\n", s.ID, s.Created.Local().Format("2006-01-02 15:04"), s.Model, s.Workspace)
	for i, t := range s.Tasks {
		fmt.Fprintf(a.Out, "\n#%d %s\n   status: %s · verification: %s · %d steps · %s\n", i+1, t.Prompt, t.Status, orStr(t.Verified, "-"), t.Steps, t.Finished.Sub(t.Started).Round(time.Second))
		if t.Summary != "" {
			fmt.Fprintf(a.Out, "   summary: %s\n", t.Summary)
		}
		for _, c := range t.Changes {
			fmt.Fprintf(a.Out, "   change: %s\n", c)
		}
	}
	events, err := audit.Read(cfg.AuditPath(), s.ID)
	if err != nil || len(events) == 0 {
		return nil
	}
	fmt.Fprintln(a.Out, "\nactions:")
	for _, e := range events {
		switch e.Kind {
		case "tool_request":
			fmt.Fprintf(a.Out, "  %s  %-13s %s\n", e.Time.Local().Format("15:04:05"), e.Tool, clip(fmt.Sprint(e.Data["input"]), 100))
		case "decision":
			fmt.Fprintf(a.Out, "                    policy: %v risk, %v — %v\n", e.Data["risk"], e.Data["action"], e.Data["reason"])
		case "approval":
			fmt.Fprintf(a.Out, "                    approval: approved=%v\n", e.Data["approved"])
		case "tool_result":
			fmt.Fprintf(a.Out, "                    result: %v (exit %v, %vms)\n", e.Data["status"], e.Data["exit_code"], e.Data["duration_ms"])
		}
	}
	return nil
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func (a *App) doctorCmd() *cobra.Command {
	var online bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check ScoutClaw's configuration, state directory and provider connectivity",
		Long: `Runs local checks and a DNS/TCP reachability test of the API host.
With --online it also sends one tiny request to the model to prove the key and model work (uses a few tokens).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			failures := 0
			check := func(ok bool, hard bool, name, detail string) {
				mark := "✓"
				if !ok {
					mark = "!"
					if hard {
						mark, failures = "✗", failures+1
					}
				}
				fmt.Fprintf(a.Out, "%s %-22s %s\n", mark, name, detail)
			}
			p := platform.Detect()
			check(p.FullySupported, false, "platform", fmt.Sprintf("%s/%s %s%s", p.OS, p.Arch, p.Distro, map[bool]string{true: "", false: " (built-in diagnostics are Linux-only)"}[p.FullySupported]))
			if p.IsRoot {
				check(false, false, "privileges", "running as root; ScoutClaw is designed to run as a normal user")
			}

			cfg, err := a.loadConfig(cmd)
			if err != nil {
				check(false, true, "configuration", err.Error())
				return fmt.Errorf("doctor found %d problem(s)", failures)
			}
			check(true, true, "configuration", fmt.Sprintf("model %s, approval %s, %d max steps", cfg.Model, cfg.Approval, cfg.MaxSteps))
			check(cfg.APIKey != "", true, "api key", cfg.MaskedKey()+" ("+config.EnvAPIKey+")")
			check(true, true, "workspace", cfg.Workspace)

			if err := os.MkdirAll(cfg.StateDir, 0o700); err == nil {
				f, werr := os.CreateTemp(cfg.StateDir, ".doctor-*")
				if werr == nil {
					f.Close()
					os.Remove(f.Name())
				}
				err = werr
			} else {
				err = fmt.Errorf("%w", err)
			}
			check(err == nil, true, "state directory", orStr(errString(err), cfg.StateDir+" (writable)"))

			for _, bin := range []string{"git", "docker", "systemctl"} {
				path, err := lookPath(bin)
				check(err == nil, false, "optional: "+bin, orStr(path, "not found (related diagnostics unavailable)"))
			}

			host, port := apiHost(cfg.BaseURL)
			ctx, cancel := context.WithTimeout(cmd.Context(), 8*time.Second)
			defer cancel()
			_, derr := net.DefaultResolver.LookupHost(ctx, host)
			check(derr == nil, false, "dns "+host, orStr(errString(derr), "resolves"))
			if derr == nil {
				var d net.Dialer
				conn, cerr := d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
				if cerr == nil {
					conn.Close()
				}
				check(cerr == nil, false, "tcp "+host+":"+port, orStr(errString(cerr), "reachable"))
			}

			if online {
				if cfg.APIKey == "" {
					check(false, true, "model request", "skipped: no API key")
				} else {
					ui := a.newUI()
					prov, perr := providerFactory(cfg, ui)
					if perr == nil {
						rctx, rcancel := context.WithTimeout(cmd.Context(), 60*time.Second)
						start := time.Now()
						var resp *llm.Response
						resp, perr = prov.Generate(rctx, llm.Request{Instructions: "Reply with the single word: pong", UserText: "ping"})
						rcancel()
						if perr == nil {
							check(true, true, "model request", fmt.Sprintf("%s answered in %s (%d in / %d out tokens)", prov.Model(), time.Since(start).Round(time.Millisecond), resp.Usage.InputTokens, resp.Usage.OutputTokens))
						}
					}
					if perr != nil {
						check(false, true, "model request", redact.String(perr.Error()))
					}
				}
			} else {
				fmt.Fprintln(a.Out, "  (use --online to test the key and model with one small request)")
			}
			if failures > 0 {
				return fmt.Errorf("doctor found %d problem(s)", failures)
			}
			fmt.Fprintln(a.Out, "\nall required checks passed")
			return nil
		},
	}
	cmd.Flags().BoolVar(&online, "online", false, "send one small request to the model")
	return cmd
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func lookPath(bin string) (string, error) {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		p := filepath.Join(dir, bin)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", os.ErrNotExist
}

func apiHost(base string) (host, port string) {
	if base == "" {
		return "api.openai.com", "443"
	}
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" {
		return "api.openai.com", "443"
	}
	port = u.Port()
	if port == "" {
		port = map[string]string{"http": "80"}[u.Scheme]
		if port == "" {
			port = "443"
		}
	}
	return u.Hostname(), port
}

func (a *App) printConfig(cfg *config.Config, out io.Writer) {
	row := func(k, v, env string) { fmt.Fprintf(out, "%-14s %-44s %s\n", k, v, env) }
	row("api key", cfg.MaskedKey(), config.EnvAPIKey)
	row("base url", orStr(cfg.BaseURL, "(provider default)"), config.EnvBaseURL)
	row("model", cfg.Model, config.EnvModel)
	row("workspace", cfg.Workspace, config.EnvWorkspace)
	row("state dir", cfg.StateDir, config.EnvStateDir)
	row("approval", string(cfg.Approval), config.EnvApproval)
	row("max steps", fmt.Sprint(cfg.MaxSteps), config.EnvMaxSteps)
	row("tool timeout", cfg.ToolTimeout.String(), config.EnvToolTimeout)
	row("task timeout", cfg.TaskTimeout.String(), config.EnvTaskTimeout)
	row("max output", fmt.Sprintf("%d bytes", cfg.MaxOutput), config.EnvMaxOutput)
}

const repo = "sshuvoo/scout-claw"

// newUpdater is replaced in tests.
var newUpdater = func() *update.Updater { return update.New(repo) }

func (a *App) updateCmd() *cobra.Command {
	var check bool
	var version string
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update scout to the latest release",
		Long: `Downloads the latest GitHub release for this OS and architecture, verifies its SHA-256
checksum, and replaces the running scout executable in place.

  scout update               install the latest release
  scout update --check       only report whether a newer release exists
  scout update --version v0.1.2   install a specific release (also works for downgrades)`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			u := newUpdater()
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
			defer cancel()

			tag := version
			if tag == "" {
				var err error
				if tag, err = u.Latest(ctx); err != nil {
					return err
				}
			} else if !strings.HasPrefix(tag, "v") {
				tag = "v" + tag
			}
			fmt.Fprintf(a.Out, "installed: %s\navailable: %s\n", Version, tag)
			if version == "" && !update.Newer(Version, tag) {
				fmt.Fprintln(a.Out, "scout is up to date.")
				return nil
			}
			if check {
				fmt.Fprintf(a.Out, "A newer release is available. Run: scout update\n")
				return nil
			}
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			if real, err := filepath.EvalSymlinks(exe); err == nil {
				exe = real
			}
			fmt.Fprintf(a.Out, "downloading %s …\n", u.ArchiveName(tag))
			bin, err := u.Fetch(ctx, tag)
			if err != nil {
				return err
			}
			if err := update.Replace(exe, bin); err != nil {
				return err
			}
			fmt.Fprintf(a.Out, "updated %s → %s (%s)\nrelease notes: https://github.com/%s/releases/tag/%s\n", Version, tag, exe, repo, tag)
			return nil
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "only check whether an update is available")
	cmd.Flags().StringVar(&version, "version", "", "install this specific release tag")
	return cmd
}
