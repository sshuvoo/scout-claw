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
}

type App struct {
	In     io.Reader
	Out    io.Writer
	Err    io.Writer
	fl     flags
	input  *Input
	stdinT bool
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
	pf.StringVar(&a.fl.approval, "approval", "", "approval policy: ask | auto | readonly (default ask; env "+config.EnvApproval+")")
	pf.StringVar(&a.fl.model, "model", "", "model name (default "+config.DefaultModel+"; env "+config.EnvModel+")")
	pf.IntVar(&a.fl.maxSteps, "max-steps", 0, "maximum agent iterations per task (default 25; env "+config.EnvMaxSteps+")")
	pf.BoolVarP(&a.fl.verbose, "verbose", "v", false, "show more tool output")
	pf.BoolVar(&a.fl.noColor, "no-color", false, "disable colored output")
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
			if err := a.promptForKey(cmd.Context(), cfg, a.newUI()); err != nil {
				return err
			}
			fmt.Fprintf(a.Out, "Saved (%s). Verify with: scout doctor --online\n", cfg.MaskedKey())
			return nil
		},
	}
	root.AddCommand(setup, run, chat, a.doctorCmd(), a.statusCmd(), a.configCmd(), a.historyCmd(), a.versionCmd())
	return root
}

// loadConfig applies .env files, the environment and then explicit flags.
func (a *App) loadConfig(cmd *cobra.Command) (*config.Config, error) {
	config.LoadDotEnv()
	if a.fl.workspace != "" {
		os.Setenv(config.EnvWorkspace, a.fl.workspace)
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	changed := func(n string) bool { f := cmd.Flags().Lookup(n); return f != nil && f.Changed }
	if changed("approval") {
		cfg.Approval = config.Approval(a.fl.approval)
	}
	if changed("model") {
		cfg.Model = a.fl.model
	}
	if changed("max-steps") {
		cfg.MaxSteps = a.fl.maxSteps
	}
	return cfg, cfg.Validate()
}

func (a *App) newUI() *UI {
	color := !a.fl.noColor && os.Getenv("NO_COLOR") == "" && a.Out == io.Writer(os.Stdout) && isTerminal(os.Stdout)
	if a.input == nil {
		a.input = NewInput(a.In)
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
	sess, err := a.resumeSession(cfg)
	if err != nil {
		return err
	}
	ui := a.newUI()
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
