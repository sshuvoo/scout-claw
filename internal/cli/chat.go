package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"scoutclaw/internal/config"
	"scoutclaw/internal/session"
)

const banner = `
  ____                  _    ____ _
 / ___|  ___ ___  _   _| |_ / ___| | __ ___      __
 \___ \ / __/ _ \| | | | __| |   | |/ _` + "`" + ` \ \ /\ / /
  ___) | (_| (_) | |_| | |_| |___| | (_| |\ V  V /
 |____/ \___\___/ \__,_|\__|\____|_|\__,_| \_/\_/ `

var tips = []string{
	"Ask ScoutClaw to inspect logs, disk usage, memory, or network ports.",
	"Type /help to see all slash commands.",
	"/approval readonly makes ScoutClaw look but never touch.",
	"Press Ctrl-C to cancel the running task and stop its commands.",
	"Try: 'Which process is using the most CPU right now?'",
	"/reset starts a fresh conversation; /exit quits.",
}

const helpText = `Type a task in plain language, or a slash command:
  /help                 show this help
  /config               show effective configuration (key masked)
  /status               session, limits and workspace
  /model [name]         show or switch the model (this session only)
  /approval [mode]      show or switch approvals: ask | auto | readonly (this session only)
  /key                  enter or replace your OpenAI API key (hidden input, saved to ~/.scoutclaw.env)
  /history              recent sessions
  /doctor               quick local checks
  /clear                clear the screen
  /reset                start a new session (forget conversation context)
  /exit                 quit (also: exit, quit, q)
Ctrl-C cancels a running task. Persistent settings: edit ~/.scoutclaw.env.`

func (a *App) chat(cmd *cobra.Command) error {
	cfg, err := a.loadConfig(cmd)
	if err != nil {
		return err
	}
	sess, err := a.resumeSession(cfg)
	if err != nil {
		return err
	}
	ui := a.newUI()
	if cfg.APIKey == "" && a.stdinT {
		fmt.Fprintln(a.Out, ui.c(cYellow, "No OPENAI_API_KEY found."))
		if err := a.promptForKey(cmd.Context(), cfg, ui, false); err != nil {
			return err
		}
	}
	rt, err := newRuntime(cfg, ui, sess)
	if err != nil {
		return err
	}
	defer func() { rt.Close() }()

	fmt.Fprintln(a.Out, ui.c(cBold+cCyan, banner))
	fmt.Fprintf(a.Out, "   %s\n", ui.c(cDim, "Autonomous on-device diagnostic agent · session "+rt.Session.ID))
	fmt.Fprintf(a.Out, "   %s\n\n", ui.c(cGray, fmt.Sprintf("workspace %s · approval %s · model %s · /help for commands", cfg.Workspace, cfg.Approval, cfg.Model)))

	n := 0
	for {
		if n > 0 {
			fmt.Fprintln(a.Out, ui.c(cDim, "💡 "+tips[(n-1)%len(tips)]))
		}
		fmt.Fprint(a.Out, ui.c(cGreen, "❯ "))
		line, err := ui.In.ReadLine(cmd.Context())
		if err != nil {
			fmt.Fprintln(a.Out)
			if errors.Is(err, errEOF) {
				return nil
			}
			return err
		}
		prompt := strings.TrimSpace(line)
		if prompt == "" {
			continue
		}
		lower := strings.ToLower(prompt)
		if lower == "exit" || lower == "quit" || lower == "q" || lower == "help" || lower == "clear" || lower == "reset" {
			prompt, lower = "/"+lower, "/"+lower // legacy bare words
		}
		if strings.HasPrefix(prompt, "/") {
			quit, newRT := a.slash(cmd.Context(), cfg, ui, rt, prompt)
			if quit {
				fmt.Fprintln(a.Out, ui.c(cCyan, "Bye from ScoutClaw."))
				return nil
			}
			if newRT != nil {
				rt = newRT
				n = 0
			}
			continue
		}
		fmt.Fprintln(a.Out)
		rt.RunTask(cmd.Context(), prompt)
		fmt.Fprintln(a.Out)
		n++
		if cmd.Context().Err() != nil {
			return nil
		}
	}
}

// slash handles one slash command. It returns quit=true to leave the chat and a
// replacement runtime when the command had to rebuild it.
func (a *App) slash(ctx context.Context, cfg *config.Config, ui *UI, rt *Runtime, line string) (quit bool, newRT *Runtime) {
	fields := strings.Fields(line)
	name, args := strings.ToLower(fields[0]), fields[1:]
	say := func(f string, v ...any) { fmt.Fprintf(a.Out, f+"\n", v...) }

	switch name {
	case "/exit", "/quit", "/q":
		return true, nil
	case "/help", "/?":
		say("%s", helpText)
	case "/clear":
		fmt.Fprint(a.Out, "\033[H\033[2J")
	case "/reset":
		fresh, err := newRuntime(cfg, ui, nil)
		if err != nil {
			say("%s", ui.c(cRed, "cannot start a new session: "+err.Error()))
			return false, nil
		}
		rt.Close()
		say("%s", ui.c(cYellow, "Context cleared. New session "+fresh.Session.ID))
		return false, fresh
	case "/config":
		a.printConfig(cfg)
	case "/status":
		say("session   %s (%d tasks)\nmodel     %s\napproval  %s\nworkspace %s\nlimits    %d steps/task, %s per tool, %s per task",
			rt.Session.ID, len(rt.Session.Tasks), cfg.Model, cfg.Approval, cfg.Workspace, cfg.MaxSteps, cfg.ToolTimeout, cfg.TaskTimeout)
	case "/model":
		if len(args) == 0 {
			say("model: %s   (switch with /model <name>)", cfg.Model)
			break
		}
		old := cfg.Model
		cfg.Model = args[0]
		rt.syncSession()
		fresh, err := newRuntime(cfg, ui, rt.Session)
		if err != nil {
			cfg.Model = old
			say("%s", ui.c(cRed, "cannot switch model: "+err.Error()))
			break
		}
		rt.Close()
		say("model is now %s for this session (set SCOUTCLAW_MODEL in ~/.scoutclaw.env to make it permanent)", cfg.Model)
		return false, fresh
	case "/approval":
		if len(args) == 0 {
			say("approval: %s   (ask | auto | readonly; switch with /approval <mode>)", cfg.Approval)
			break
		}
		mode := config.Approval(strings.ToLower(args[0]))
		switch mode {
		case config.ApprovalAsk, config.ApprovalAuto, config.ApprovalReadOnly:
			cfg.Approval = mode
			rt.Agent.Exec.Policy.Mode = mode
			say("approval is now %s for this session", mode)
		default:
			say("%s", ui.c(cRed, "unknown mode; use ask, auto or readonly"))
		}
	case "/key":
		if err := a.promptForKey(ctx, cfg, ui, false); err != nil {
			say("%s", ui.c(cRed, err.Error()))
			break
		}
		rt.syncSession()
		fresh, err := newRuntime(cfg, ui, rt.Session)
		if err != nil {
			say("%s", ui.c(cRed, "key saved, but the runtime could not restart: "+err.Error()))
			break
		}
		rt.Close()
		say("%s", ui.c(cGreen, "API key saved and active ("+cfg.MaskedKey()+")."))
		return false, fresh
	case "/history":
		sessions, _ := session.List(cfg.SessionsDir())
		if len(sessions) == 0 {
			say("no history yet")
		}
		for i, s := range sessions {
			if i >= 10 {
				break
			}
			last := "-"
			if k := len(s.Tasks); k > 0 {
				last = clip(s.Tasks[k-1].Prompt, 60)
			}
			say("%s  %d task(s)  %s", s.ID, len(s.Tasks), last)
		}
	case "/doctor":
		say("api key   %s\nstate dir %s\nworkspace %s\n(run `scout doctor --online` outside chat for connectivity checks)", cfg.MaskedKey(), cfg.StateDir, cfg.Workspace)
	default:
		say("%s", ui.c(cYellow, "unknown command "+name+" — type /help"))
	}
	return false, nil
}

// promptForKey reads the API key with "*" feedback, stores it in
// ~/.scoutclaw.env (mode 0600) and applies it to the running configuration.
// With allowPipe (scout setup) a key piped on stdin is accepted without a terminal.
func (a *App) promptForKey(ctx context.Context, cfg *config.Config, ui *UI, allowPipe bool) error {
	if !a.stdinT && !allowPipe {
		return errors.New("/key needs an interactive terminal; run `scout setup` in a shell instead")
	}
	path, err := config.UserEnvPath()
	if err != nil {
		return err
	}
	var line string
	if a.stdinT {
		fmt.Fprintf(a.Out, "Paste your OpenAI API key and press Enter (Ctrl+Shift+V pastes in most terminals).\n"+
			"Characters appear as *, Ctrl-C cancels. Saved to %s (mode 600).\nAPI key: ", path)
		restore := rawNoEcho()
		pctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		line, err = ui.In.ReadSecret(pctx)
		stop()
		restore()
		if err != nil {
			fmt.Fprintln(a.Out)
			if ctx.Err() == nil && errors.Is(err, context.Canceled) {
				return errors.New("cancelled; nothing changed")
			}
			return err
		}
	} else if line, err = ui.In.ReadLine(ctx); err != nil {
		return errors.New("no key received on stdin")
	}
	key := strings.TrimSpace(line)
	if key == "" {
		return errors.New("no key entered; nothing changed")
	}
	if err := config.SetEnvFile(path, config.EnvAPIKey, key); err != nil {
		return fmt.Errorf("could not save key: %w", err)
	}
	os.Setenv(config.EnvAPIKey, key)
	cfg.APIKey = key
	fmt.Fprintf(a.Out, "Received %d characters.\n", len(key))
	if !strings.HasPrefix(key, "sk-") {
		fmt.Fprintln(a.Out, ui.c(cYellow, "note: OpenAI keys normally start with \"sk-\"; check with: scout doctor --online"))
	}
	return nil
}

// rawNoEcho switches the terminal to unbuffered, no-echo input and returns a
// function that restores the previous settings (also used on Ctrl-C).
func rawNoEcho() func() {
	get := exec.Command("stty", "-g")
	get.Stdin = os.Stdin
	saved, err := get.Output()
	if err != nil {
		return func() {}
	}
	set := exec.Command("stty", "-icanon", "-echo", "min", "1")
	set.Stdin = os.Stdin
	if set.Run() != nil {
		return func() {}
	}
	return func() {
		r := exec.Command("stty", strings.TrimSpace(string(saved)))
		r.Stdin = os.Stdin
		_ = r.Run()
	}
}
