package cli

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"scoutclaw/internal/config"
	"scoutclaw/internal/policy"
)

// Commands that read and edit the layered config files: scout config
// get/set/unset, scout permissions, scout trust and scout init.

// layerFlags adds --global/--project/--local and returns the chosen layer.
func layerFlags(cmd *cobra.Command) func() (config.Layer, error) {
	var global, project, local bool
	cmd.Flags().BoolVar(&global, "global", false, "edit ~/.scout/config.json (all projects)")
	cmd.Flags().BoolVar(&project, "project", false, "edit .scout/config.json (shared with the team; commit it)")
	cmd.Flags().BoolVar(&local, "local", false, "edit .scout/config.local.json (personal, git-ignored; the default)")
	return func() (config.Layer, error) {
		n := 0
		for _, b := range []bool{global, project, local} {
			if b {
				n++
			}
		}
		switch {
		case n > 1:
			return 0, errors.New("use only one of --global, --project and --local")
		case global:
			return config.LayerGlobal, nil
		case project:
			return config.LayerProject, nil
		}
		return config.LayerLocal, nil
	}
}

// configSourceLabel shortens a source for display.
func configSourceLabel(cfg *config.Config, src string) string {
	if rel, err := filepath.Rel(cfg.ProjectRoot, src); err == nil && !strings.HasPrefix(rel, "..") && filepath.IsAbs(src) {
		return rel
	}
	if d, err := config.ConfigDir(); err == nil {
		if rel, err := filepath.Rel(d, src); err == nil && !strings.HasPrefix(rel, "..") && filepath.IsAbs(src) {
			return filepath.Join("~/.scout", rel)
		}
	}
	return src
}

func (a *App) configSubcommands(root *cobra.Command) {
	get := &cobra.Command{
		Use:   "get <key>",
		Short: "Print one effective setting and where it comes from",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadConfig(cmd)
			if err != nil {
				return err
			}
			v, ok := settingValue(cfg, args[0])
			if !ok {
				return fmt.Errorf("unknown setting %q (known: %s)", args[0], strings.Join(config.SettingKeys(), ", "))
			}
			fmt.Fprintf(a.Out, "%s\t(%s)\n", v, configSourceLabel(cfg, cfg.Sources[args[0]]))
			return nil
		},
	}
	set := &cobra.Command{
		Use:     "set <key> <value>",
		Short:   "Set a value in a config file (default: the personal project file)",
		Example: "  scout config set approval edits\n  scout config set model gpt-5.5 --global\n  scout config set maxSteps 150 --project",
		Args:    cobra.ExactArgs(2),
	}
	setLayer := layerFlags(set)
	set.RunE = func(cmd *cobra.Command, args []string) error {
		cfg, path, layer, err := a.layerFile(cmd, setLayer)
		if err != nil {
			return err
		}
		if err := config.SetValue(path, layer, args[0], args[1]); err != nil {
			return err
		}
		fmt.Fprintf(a.Out, "%s = %s in %s\n", args[0], args[1], configSourceLabel(cfg, path))
		if src := cfg.Sources[args[0]]; strings.HasPrefix(src, "env ") {
			fmt.Fprintf(a.Out, "note: %s overrides config files while it is set\n", strings.TrimPrefix(src, "env "))
		}
		return nil
	}
	unset := &cobra.Command{Use: "unset <key>", Short: "Remove a value from a config file", Args: cobra.ExactArgs(1)}
	unsetLayer := layerFlags(unset)
	unset.RunE = func(cmd *cobra.Command, args []string) error {
		cfg, path, _, err := a.layerFile(cmd, unsetLayer)
		if err != nil {
			return err
		}
		removed, err := config.UnsetValue(path, args[0])
		if err != nil {
			return err
		}
		if !removed {
			fmt.Fprintf(a.Out, "%s is not set in %s\n", args[0], configSourceLabel(cfg, path))
			return nil
		}
		fmt.Fprintf(a.Out, "removed %s from %s\n", args[0], configSourceLabel(cfg, path))
		return nil
	}
	root.AddCommand(get, set, unset)
}

func (a *App) layerFile(cmd *cobra.Command, layer func() (config.Layer, error)) (*config.Config, string, config.Layer, error) {
	l, err := layer()
	if err != nil {
		return nil, "", 0, err
	}
	cfg, err := a.loadConfig(cmd)
	if err != nil {
		return nil, "", 0, err
	}
	path, err := cfg.LayerPath(l)
	return cfg, path, l, err
}

func settingValue(cfg *config.Config, key string) (string, bool) {
	switch key {
	case "model":
		return cfg.Model, true
	case "baseUrl":
		return orStr(cfg.BaseURL, "(provider default)"), true
	case "approval":
		return string(cfg.Approval), true
	case "maxSteps":
		return fmt.Sprint(cfg.MaxSteps), true
	case "toolTimeout":
		return cfg.ToolTimeout.String(), true
	case "taskTimeout":
		return cfg.TaskTimeout.String(), true
	case "maxOutput":
		return fmt.Sprint(cfg.MaxOutput), true
	case "maxRead":
		return fmt.Sprint(cfg.MaxRead), true
	case "compactAt":
		return compactLabel(cfg.CompactAt), true
	case "logLevel":
		return cfg.LogLevel, true
	}
	return "", false
}

func (a *App) permissionsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "permissions",
		Short: "List, add or remove permission rules",
		Long: `Permission rules decide actions before the approval mode does: deny wins, then ask, then allow.
Syntax: Tool or Tool(spec)
  fs_*        path glob relative to the project root:  fs_write(src/**), fs_read(**/.env*)
  shell_exec  command, " *" = any further arguments:     shell_exec(go test *)
  proc_start  same as shell_exec:                         proc_start(npm run *)
  net_*       domain:host pattern:                        net_http(domain:*.github.com)
shell_script and shell_powershell can be denied or asked about, never allowed.
Rules in the shared project file never allow high-risk actions.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadConfig(cmd)
			if err != nil {
				return err
			}
			printRules(a.Out, cfg)
			return nil
		},
	}
	add := &cobra.Command{
		Use:     "add <allow|ask|deny> <rule>",
		Short:   "Add a rule (default: the personal project file)",
		Example: "  scout permissions add allow 'shell_exec(go test *)'\n  scout permissions add deny 'fs_read(**/.env*)' --project",
		Args:    cobra.ExactArgs(2),
	}
	addLayer := layerFlags(add)
	add.RunE = func(cmd *cobra.Command, args []string) error {
		action, err := policy.ParseRuleAction(args[0])
		if err != nil {
			return err
		}
		cfg, path, layer, err := a.layerFile(cmd, addLayer)
		if err != nil {
			return err
		}
		if _, err := policy.ParseRule(args[1], action, path, layer != config.LayerProject); err != nil {
			return err
		}
		added, err := config.AddRule(path, args[0], args[1])
		if err != nil {
			return err
		}
		if !added {
			fmt.Fprintf(a.Out, "%s %s is already in %s\n", args[0], args[1], configSourceLabel(cfg, path))
			return nil
		}
		fmt.Fprintf(a.Out, "added %s %s to %s\n", args[0], args[1], configSourceLabel(cfg, path))
		if layer == config.LayerProject && action == policy.RuleAllow {
			fmt.Fprintln(a.Out, "note: allow rules in the shared file apply only after you trust the project (scout trust)")
		}
		return nil
	}
	remove := &cobra.Command{Use: "remove <rule>", Short: "Remove a rule from a config file", Args: cobra.ExactArgs(1)}
	removeLayer := layerFlags(remove)
	remove.RunE = func(cmd *cobra.Command, args []string) error {
		cfg, path, _, err := a.layerFile(cmd, removeLayer)
		if err != nil {
			return err
		}
		removed, err := config.RemoveRule(path, args[0])
		if err != nil {
			return err
		}
		if !removed {
			return fmt.Errorf("%s is not in %s", args[0], configSourceLabel(cfg, path))
		}
		fmt.Fprintf(a.Out, "removed %s from %s\n", args[0], configSourceLabel(cfg, path))
		return nil
	}
	cmd.AddCommand(add, remove)
	return cmd
}

func printRules(out io.Writer, cfg *config.Config) {
	if len(cfg.Rules) == 0 {
		fmt.Fprintln(out, "no permission rules (add one with: scout permissions add allow 'shell_exec(go test *)')")
	}
	for _, r := range cfg.Rules {
		fmt.Fprintf(out, "%-5s %-40s %s\n", r.Action, r.Text, configSourceLabel(cfg, r.Path))
	}
	if t := cfg.Trust; t.Needed() {
		fmt.Fprintf(out, "\n%d loosening setting(s) in %s are ignored until you run: scout trust\n", len(t.Loosening), configSourceLabel(cfg, t.SharedPath))
	}
	for _, d := range cfg.ExtraDirs {
		fmt.Fprintf(out, "extra writable directory: %s\n", d)
	}
}

func (a *App) trustCmd() *cobra.Command {
	var revoke bool
	cmd := &cobra.Command{
		Use:   "trust",
		Short: "Trust (or with --revoke, stop trusting) this project's shared .scout/config.json",
		Long: `A project's shared .scout/config.json can loosen ScoutClaw's checks (allow rules, approval auto/edits,
extra writable directories, instruction files). Those settings apply only after you trust the file;
trust is tied to its exact content, so any change asks again.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadConfig(cmd)
			if err != nil {
				return err
			}
			if revoke {
				removed, err := cfg.RevokeTrust()
				if err != nil {
					return err
				}
				if !removed {
					fmt.Fprintf(a.Out, "%s was not trusted\n", cfg.ProjectRoot)
					return nil
				}
				fmt.Fprintf(a.Out, "no longer trusting %s\n", cfg.ProjectRoot)
				return nil
			}
			if cfg.Trust.SharedPath == "" {
				fmt.Fprintf(a.Out, "%s has no shared .scout/config.json; nothing to trust\n", cfg.ProjectRoot)
				return nil
			}
			for _, l := range cfg.Trust.Loosening {
				fmt.Fprintf(a.Out, "  - %s\n", l)
			}
			if err := cfg.TrustProject(); err != nil {
				return err
			}
			fmt.Fprintf(a.Out, "trusted %s (asks again if the file changes)\n", configSourceLabel(cfg, cfg.Trust.SharedPath))
			return nil
		},
	}
	cmd.Flags().BoolVar(&revoke, "revoke", false, "stop trusting this project")
	return cmd
}

func (a *App) initCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Create .scout/config.json for this project (shared settings and permission rules)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadConfig(cmd)
			if err != nil {
				return err
			}
			created, err := config.InitProject(cfg.Workspace)
			if err != nil {
				return err
			}
			if len(created) == 0 {
				fmt.Fprintf(a.Out, "%s already has .scout/config.json\n", cfg.Workspace)
				return nil
			}
			for _, p := range created {
				fmt.Fprintf(a.Out, "created %s\n", p)
			}
			fmt.Fprintln(a.Out, "Commit .scout/config.json to share rules with your team; personal approvals go to .scout/config.local.json (git-ignored).")
			fmt.Fprintln(a.Out, "Project instructions for the agent can go in AGENTS.md at the project root.")
			return nil
		},
	}
}
