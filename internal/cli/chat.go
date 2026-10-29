package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

const banner = `
  ____                  _    ____ _
 / ___|  ___ ___  _   _| |_ / ___| | __ ___      __
 \___ \ / __/ _ \| | | | __| |   | |/ _` + "`" + ` \ \ /\ / /
  ___) | (_| (_) | |_| | |_| |___| | (_| |\ V  V /
 |____/ \___\___/ \__,_|\__|\____|_|\__,_| \_/\_/ `

var tips = []string{
	"Ask ScoutClaw to inspect logs, disk usage, memory, or network ports.",
	"Type 'reset' to start a fresh conversation (new session).",
	"Risky actions always ask first; use --approval readonly for look-only mode.",
	"Press Ctrl-C to cancel the running task and stop its commands.",
	"Try: 'Which process is using the most CPU right now?'",
	"Type 'clear' to clean the screen, 'exit' to quit.",
}

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
	rt, err := newRuntime(cfg, ui, sess)
	if err != nil {
		return err
	}
	defer rt.Close()

	fmt.Fprintln(a.Out, ui.c(cBold+cCyan, banner))
	fmt.Fprintf(a.Out, "   %s\n", ui.c(cDim, "Autonomous on-device diagnostic agent · session "+rt.Session.ID))
	fmt.Fprintf(a.Out, "   %s\n\n", ui.c(cGray, fmt.Sprintf("workspace %s · approval %s · model %s · 'help' for commands", cfg.Workspace, cfg.Approval, cfg.Model)))

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
		switch strings.ToLower(prompt) {
		case "exit", "quit", "q":
			fmt.Fprintln(a.Out, ui.c(cCyan, "Bye from ScoutClaw."))
			return nil
		case "clear":
			fmt.Fprint(a.Out, "\033[H\033[2J")
			continue
		case "help":
			fmt.Fprintln(a.Out, "Type a task in plain language. Commands: help, clear, reset (new session), exit.\nCtrl-C cancels a running task.")
			continue
		case "reset":
			rt.Close()
			if rt, err = newRuntime(cfg, ui, nil); err != nil {
				return err
			}
			defer rt.Close()
			n = 0
			fmt.Fprintln(a.Out, ui.c(cYellow, "Context cleared. New session "+rt.Session.ID))
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
