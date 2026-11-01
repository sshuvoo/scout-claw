// Package cli implements the scout command line.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"scoutclaw/internal/config"
	"scoutclaw/internal/session"
)

// Version is set at build time with -ldflags "-X scoutclaw/internal/cli.Version=...".
var Version = "dev"

type flags struct {
	workspace string
	approval  string
	model     string
	maxSteps  int
	resume    string
	verbose   bool
	noColor   bool
	plain     bool
	trust     bool
}

type App struct {
	In     io.Reader
	Out    io.Writer
	Err    io.Writer
	fl     flags
	input  *Input
	stdinT bool
	warned bool // config warnings were printed once
}

// Execute runs the CLI and returns the process exit code.
func Execute(ctx context.Context) int {
	app := &App{In: os.Stdin, Out: os.Stdout, Err: os.Stderr, stdinT: isTerminal(os.Stdin)}
	return app.Run(ctx, os.Args[1:])
}

func (a *App) Run(ctx context.Context, args []string) int {
	root := a.Command()
	root.SetArgs(args)
	root.SetOut(a.Out)
	root.SetErr(a.Err)
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(a.Err, "scout: "+err.Error())
		return 1
	}
	return 0
}

func (a *App) Command() *cobra.Command {
	root := &cobra.Command{
		Use:   "scout [task]",
		Short: "Autonomous diagnostic and repair agent for your computer or server",
		Long: `ScoutClaw investigates problems on the machine it runs on: it inspects files, logs,
processes and the network, forms and tests hypotheses, applies the smallest justified
fix (with your approval for anything risky) and verifies the result.

  scout "why is my disk full?"     run one task (same as 'scout run')
  scout                            interactive chat session`,
		Example: `  scout run "Diagnose why Docker is failing"
  scout run --approval readonly "Why is this machine slow?"
  scout chat
  scout history`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return a.chat(cmd)
			}
			return a.runTask(cmd, strings.Join(args, " "))
		},
	}
	pf := root.PersistentFlags()
	pf.StringVar(&a.fl.workspace, "workspace", "", "directory the agent may modify (default: current directory; env "+config.EnvWorkspace+")")
	pf.StringVar(&a.fl.approval, "approval", "", "approval policy: ask | edits | auto | readonly (default ask; env "+config.EnvApproval+")")
	pf.StringVar(&a.fl.model, "model", "", "model name (default "+config.DefaultModel+"; env "+config.EnvModel+")")
	pf.IntVar(&a.fl.maxSteps, "max-steps", 0, fmt.Sprintf("maximum agent iterations per task (default %d; env %s)", config.DefaultMaxSteps, config.EnvMaxSteps))
	pf.BoolVarP(&a.fl.verbose, "verbose", "v", false, "show more tool output")
	pf.BoolVar(&a.fl.noColor, "no-color", false, "disable colored output")
	pf.BoolVar(&a.fl.plain, "plain", false, "use the simple line-based chat instead of the full-screen UI")
	pf.BoolVar(&a.fl.trust, "trust-workspace", false, "apply this project's shared .scout/config.json fully for this run, without recording trust")
	root.Flags().StringVar(&a.fl.resume, "resume", "", "resume a session: an id or 'last'")

	run := &cobra.Command{
		Use:   "run <task>",
		Short: "Run a single task and exit",
		Example: `  scout run "Find out why my application is not starting and fix it"
  scout run --workspace ~/myapp "The tests fail, find out why"`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return a.runTask(cmd, strings.Join(args, " ")) },
	}
	run.Flags().StringVar(&a.fl.resume, "resume", "", "continue a session: an id or 'last'")

	chat := &cobra.Command{
		Use:   "chat",
		Short: "Interactive session; follow-up requests keep the conversation",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return a.chat(cmd) },
	}
	chat.Flags().StringVar(&a.fl.resume, "resume", "", "continue a session: an id or 'last'")

	setup := &cobra.Command{
		Use:   "setup",
		Short: "Enter your OpenAI API key once (hidden input, saved for all future runs)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadConfig(cmd)
			if err != nil {
				return err
			}
			if err := a.promptForKey(cmd.Context(), cfg, a.newUI(), true); err != nil {
				return err
			}
			fmt.Fprintf(a.Out, "Saved. Now verify it with: scout doctor --online\n")
			return nil
		},
	}
	root.AddCommand(setup, a.updateCmd(), run, chat, a.doctorCmd(), a.statusCmd(), a.configCmd(), a.historyCmd(), a.versionCmd(),
		a.initCmd(), a.permissionsCmd(), a.trustCmd())
	return root
}

// loadConfig applies .env files, the environment and then explicit flags.
func (a *App) loadConfig(cmd *cobra.Command) (*config.Config, error) {
	config.LoadDotEnv()
	if a.fl.workspace != "" {
		os.Setenv(config.EnvWorkspace, a.fl.workspace)
	}
	cfg, err := config.LoadWith(config.Options{TrustProject: a.fl.trust})
	if err != nil {
		return nil, err
	}
	changed := func(n string) bool { f := cmd.Flags().Lookup(n); return f != nil && f.Changed }
	if changed("approval") {
		cfg.Approval, cfg.Sources["approval"] = config.Approval(a.fl.approval), "flag --approval"
	}
	if changed("model") {
		cfg.Model, cfg.Sources["model"] = a.fl.model, "flag --model"
	}
	if changed("max-steps") {
		cfg.MaxSteps, cfg.Sources["maxSteps"] = a.fl.maxSteps, "flag --max-steps"
	}
	if !a.warned {
		a.warned = true
		for _, w := range cfg.Warnings {
			fmt.Fprintln(a.Err, "! "+w)
		}
	}
	return cfg, cfg.Validate()
}

// ensureTrust asks whether to trust a project's shared config when it would
// loosen anything and has not been trusted with its current content. Without
// a terminal the loosening settings stay ignored.
func (a *App) ensureTrust(cmd *cobra.Command, cfg *config.Config, ui *UI) (*config.Config, error) {
	t := cfg.Trust
	if !t.Needed() {
		return cfg, nil
	}
	fmt.Fprintf(a.Out, "%s\n", ui.c(cYellow+cBold, "This project's shared settings ("+t.SharedPath+") would loosen ScoutClaw's safety checks:"))
	for _, l := range t.Loosening {
		fmt.Fprintf(a.Out, "  - %s\n", l)
	}
	if !a.stdinT {
		fmt.Fprintln(a.Err, "! not trusted: these settings are ignored (no terminal to ask). Run `scout trust` in this project, or pass --trust-workspace for one run.")
		return cfg, nil
	}
	fmt.Fprint(a.Out, "Trust this project's settings? Only say yes if you trust the repository. [y/N]: ")
	line, err := ui.In.ReadLine(cmd.Context())
	if err != nil {
		return nil, err
	}
	if ans := strings.ToLower(strings.TrimSpace(line)); ans != "y" && ans != "yes" {
		fmt.Fprintln(a.Out, "Not trusted: those settings are ignored; stricter settings (deny/ask rules) still apply.")
		return cfg, nil
	}
	if err := cfg.TrustProject(); err != nil {
		return nil, err
	}
	fmt.Fprintln(a.Out, ui.c(cGreen, "Trusted. ScoutClaw asks again if this file changes."))
	return a.loadConfig(cmd)
}

func (a *App) newUI() *UI {
	color := !a.fl.noColor && os.Getenv("NO_COLOR") == "" && a.Out == io.Writer(os.Stdout) && isTerminal(os.Stdout)
	if a.input == nil {
		a.input = NewInput(a.In, a.Out)
	}
	return &UI{Out: a.Out, Err: a.Err, In: a.input, Color: color, Interactive: a.stdinT, Verbose: a.fl.verbose}
}

func (a *App) resumeSession(cfg *config.Config) (*session.Session, error) {
	if a.fl.resume == "" {
		return nil, nil
	}
	if a.fl.resume == "last" {
		s, err := session.Latest(cfg.SessionsDir())
		if err != nil {
			return nil, err
		}
		if s == nil {
			return nil, fmt.Errorf("no previous session to resume")
		}
		return s, nil
	}
	return session.Load(cfg.SessionsDir(), a.fl.resume)
}

func (a *App) runTask(cmd *cobra.Command, prompt string) error {
	cfg, err := a.loadConfig(cmd)
	if err != nil {
		return err
	}
	ui := a.newUI()
	if cfg, err = a.ensureTrust(cmd, cfg, ui); err != nil {
		return err
	}
	sess, err := a.resumeSession(cfg)
	if err != nil {
		return err
	}
	rt, err := newRuntime(cfg, ui, sess)
	if err != nil {
		return err
	}
	defer rt.Close()
	fmt.Fprintf(a.Out, "%s session %s · workspace %s · approval %s\n", ui.c(cGray, "▸"), rt.Session.ID, cfg.Workspace, cfg.Approval)
	res := rt.RunTask(cmd.Context(), prompt)
	switch res.Status {
	case "completed":
		if res.Verify == "failed" {
			return fmt.Errorf("task finished but the problem is not resolved")
		}
		return nil
	case "cancelled":
		return fmt.Errorf("cancelled")
	}
	return fmt.Errorf("task ended: %s", res.Status)
}
