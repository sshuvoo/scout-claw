package policy

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

var readOnlyCommands = setOf(
	"ls", "cat", "head", "tail", "grep", "egrep", "fgrep", "rg", "wc", "sort", "uniq", "cut", "tr", "diff", "cmp",
	"stat", "file", "du", "df", "free", "uname", "uptime", "hostname", "hostnamectl", "whoami", "id", "groups", "date", "pwd",
	"which", "type", "whereis", "ps", "pgrep", "top", "vmstat", "iostat", "mpstat", "lsof", "ss", "netstat", "lsblk",
	"lscpu", "lsmem", "lspci", "lsusb", "lsmod", "dmesg", "journalctl", "printenv", "env", "locale", "timedatectl", "nproc",
	"realpath", "readlink", "basename", "dirname", "tree", "md5sum", "sha256sum", "sha1sum", "ping", "traceroute", "tracepath",
	"dig", "nslookup", "host", "getent", "resolvectl", "nmcli", "ifconfig", "sensors", "smartctl", "findmnt", "ulimit",
	"echo", "true", "false", "test", "sleep", "seq", "jq", "yq", "xxd", "strings", "ldd", "nm", "apt-cache", "dpkg-query", "lsb_release",
)

var wrappers = setOf("env", "nice", "ionice", "nohup", "timeout", "time", "stdbuf", "command")
var privilege = setOf("sudo", "su", "doas", "pkexec", "runuser", "setpriv")
var shells = setOf("sh", "bash", "zsh", "dash", "ksh", "fish", "csh", "tcsh")
var inlineInterp = map[string]string{"python": "-c", "python3": "-c", "node": "-e", "nodejs": "-e", "perl": "-e", "ruby": "-e", "php": "-r"}
var systemHigh = setOf("apt", "apt-get", "aptitude", "dpkg", "dnf", "yum", "rpm", "pacman", "zypper", "apk", "snap", "flatpak",
	"reboot", "shutdown", "poweroff", "halt", "init", "telinit", "passwd", "useradd", "userdel", "usermod", "groupadd", "groupdel",
	"chpasswd", "visudo", "mount", "umount", "fdisk", "parted", "sfdisk", "gdisk", "mkswap", "swapon", "swapoff", "modprobe",
	"insmod", "rmmod", "iptables", "ip6tables", "nft", "ufw", "firewall-cmd", "setenforce", "aa-enforce", "aa-complain", "crontab", "at", "update-grub", "grub-install")
var fileMutators = setOf("rm", "rmdir", "mv", "cp", "mkdir", "touch", "ln", "chmod", "chown", "chgrp", "truncate", "tee", "install", "rsync", "unlink", "sed", "patch")

func setOf(s ...string) map[string]bool {
	m := make(map[string]bool, len(s))
	for _, v := range s {
		m[v] = true
	}
	return m
}

var forbiddenScriptPatterns = []struct {
	re  *regexp.Regexp
	why string
}{
	{regexp.MustCompile(`\brm\s+(-[a-zA-Z-]+\s+)*(/|/\*|~|~/|\$HOME|\$\{HOME\}|/(etc|usr|bin|sbin|lib|lib64|boot|var|home|root|opt))/?(\s|;|&|\||$)`), "recursive deletion of the filesystem root, home or a system directory"},
	{regexp.MustCompile(`\bmkfs(\.\w+)?\b|\bwipefs\b`), "creating or wiping a filesystem"},
	{regexp.MustCompile(`\bdd\b[^\n;|&]*\bof=/dev/`), "raw write to a block device"},
	{regexp.MustCompile(`>\s*/dev/(sd|nvme|hd|vd|xvd|mmcblk)`), "raw write to a block device"},
	{regexp.MustCompile(`:\s*\(\s*\)\s*\{`), "fork bomb"},
	{regexp.MustCompile(`\bch(mod|own)\s+(-[a-zA-Z]+\s+)*-R\b[^\n;|&]*\s/(\s|$)`), "recursive permission change on /"},
	{regexp.MustCompile(`(curl|wget)\b[^\n]*\|\s*(sudo\s+)?(sh|bash|zsh|python3?|perl)\b`), "piping downloaded content into an interpreter"},
	{regexp.MustCompile(`/etc/shadow|/etc/gshadow|\.ssh/id_|\.aws/credentials|\.gnupg|\.git-credentials`), "touches credential or key files"},
}

func forbiddenScript(script string) string {
	for _, p := range forbiddenScriptPatterns {
		if p.re.MatchString(script) {
			return "refused: " + p.why
		}
	}
	return ""
}

func dec(r Risk, reason string) Decision { return Decision{Risk: r, Reason: reason} }

func forbid(reason string) Decision {
	return Decision{Risk: High, Forbidden: true, Reason: "refused: " + reason}
}

func (e *Engine) classifyArgv(argv []string, workdir string) Decision {
	exe := argv[0]
	args := argv[1:]
	name := filepath.Base(exe)
	hasSlash := strings.Contains(exe, "/")

	// Credential files are off limits for every command.
	for _, a := range args {
		cand := a
		if i := strings.Index(a, "="); i > 0 && strings.HasPrefix(a, "-") {
			cand = a[i+1:]
		}
		if cand != "" && (strings.Contains(cand, "/") || strings.HasPrefix(cand, "~") || strings.HasPrefix(cand, ".")) && IsSensitive(e.Scope.Resolve(cand, workdir)) {
			return forbid(fmt.Sprintf("argument %q refers to a credential or key file", a))
		}
	}

	// Wrappers: classify what they run.
	if wrappers[name] && !hasSlash {
		if inner := unwrap(name, args); len(inner) > 0 {
			return e.classifyArgv(inner, workdir)
		}
		return dec(Low, "wrapper without a command")
	}
	if privilege[name] {
		d := dec(High, "privilege escalation ("+name+")")
		d.Effects = "runs a command with elevated privileges"
		d.Recovery = "depends on the command; ScoutClaw never escalates silently"
		if len(args) > 0 {
			if inner := e.classifyArgv(stripFlags(args), workdir); inner.Forbidden {
				return inner
			}
		}
		return d
	}
	if shells[name] {
		for i, a := range args {
			if a == "-c" || (strings.HasPrefix(a, "-") && strings.HasSuffix(a, "c") && !strings.HasPrefix(a, "--")) {
				script := ""
				if i+1 < len(args) {
					script = args[i+1]
				}
				if why := forbiddenScript(script); why != "" {
					return forbid(strings.TrimPrefix(why, "refused: "))
				}
				return dec(High, "inline shell script passed to "+name)
			}
		}
		return dec(Medium, "runs a shell script file")
	}
	if flag, ok := inlineInterp[strings.TrimRight(name, "0123456789.")]; ok && !hasSlash {
		for _, a := range args {
			if a == flag {
				return dec(High, "inline code passed to "+name)
			}
		}
	}

	// Never-approvable operations.
	switch {
	case name == "mkfs" || strings.HasPrefix(name, "mkfs.") || name == "wipefs":
		return forbid("creating or wiping a filesystem")
	case name == "dd":
		for _, a := range args {
			if strings.HasPrefix(a, "of=/dev/") {
				return forbid("raw write to a block device")
			}
		}
	case name == "shred" && anyPrefix(args, "/dev/"):
		return forbid("shredding a block device")
	case name == "rm":
		if isRecursive(args) {
			for _, a := range args {
				if strings.HasPrefix(a, "-") {
					continue
				}
				abs := e.Scope.Resolve(a, workdir)
				home, _ := homeDir()
				topLevelSystem := IsSystemPath(abs) && !strings.Contains(strings.Trim(abs, "/"), "/")
				if abs == "/" || abs == home || topLevelSystem || a == "/*" || a == "~/*" {
					return forbid(fmt.Sprintf("recursive deletion of %s", a))
				}
			}
		}
	case name == "kill" || name == "pkill" || name == "killall":
		for _, a := range args {
			if a == "1" || a == "-1" {
				return forbid("signalling PID 1 / all processes")
			}
		}
	case (name == "chmod" || name == "chown") && isRecursive(args):
		for _, a := range args {
			if a == "/" {
				return forbid("recursive permission change on /")
			}
		}
	}

	// A bare version/help query is harmless for a program found on PATH.
	if !hasSlash && len(args) == 1 {
		switch args[0] {
		case "--version", "-version", "-V", "version", "--help", "-h", "-v":
			return dec(Low, "version/help query")
		}
	}

	sub := firstNonFlag(args)
	switch name {
	case "git":
		return classifyGit(args, sub)
	case "docker", "podman":
		return classifyDocker(args, sub)
	case "systemctl":
		return classifySystemctl(args, sub)
	case "npm", "pnpm", "yarn", "bun":
		return classifyNode(name, sub, args)
	case "go":
		return classifyGo(sub)
	case "cargo":
		if sub == "metadata" || sub == "tree" || sub == "version" {
			return dec(Low, "read-only cargo query")
		}
		return withKey(dec(Medium, "cargo "+sub+" builds or modifies the project"), name, sub)
	case "pip", "pip3":
		if sub == "list" || sub == "show" || sub == "freeze" || sub == "check" || sub == "--version" {
			return dec(Low, "read-only pip query")
		}
		return withKey(dec(Medium, "pip "+sub+" changes installed packages"), name, sub)
	case "make", "cmake", "ninja", "gradle", "gradlew", "mvn", "pytest", "tox", "ruff", "flake8", "eslint", "tsc", "mypy", "black", "prettier":
		return withKey(dec(Medium, name+" executes project build/test tooling"), name, sub)
	case "find":
		for _, a := range args {
			switch a {
			case "-delete":
				return dec(High, "find -delete removes files")
			case "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprintf", "-fls":
				return dec(Medium, "find "+a+" runs commands or writes files")
			}
		}
		return dec(Low, "read-only search")
	case "sed":
		if hasFlag(args, "-i", "--in-place") {
			return withKey(dec(Medium, "sed -i edits files in place"), name, "-i")
		}
		return dec(Low, "stream editing to stdout")
	case "tee":
		return withKey(dec(Medium, "tee writes files"), name, "")
	case "crontab":
		if hasFlag(args, "-l") {
			return dec(Low, "lists cron jobs")
		}
	case "ip":
		if !containsAny(args, "add", "del", "delete", "set", "flush", "change", "replace", "append") {
			return dec(Low, "read-only network query")
		}
		return dec(High, "changes network configuration")
	case "mount":
		if len(args) == 0 {
			return dec(Low, "lists mounts")
		}
	case "iptables", "ip6tables", "nft", "ufw":
		if containsAny(args, "-L", "-S", "-n", "list", "status", "--list") && !containsAny(args, "-A", "-D", "-I", "-F", "add", "delete", "enable", "disable", "reset", "flush") {
			return dec(Low, "lists firewall rules")
		}
		return withRecovery(dec(High, "changes firewall rules"), "re-apply the previous ruleset")
	case "env":
		return dec(Low, "prints environment")
	case "curl", "wget":
		return withKey(dec(Medium, name+" performs arbitrary network requests / downloads"), name, "")
	case "dpkg":
		if containsAny(args, "-l", "-s", "-L", "-S", "--list", "--status", "--listfiles", "--search") {
			return dec(Low, "queries installed packages")
		}
	case "apt", "apt-get", "dnf", "yum":
		if sub == "list" || sub == "search" || sub == "show" || sub == "info" || sub == "policy" {
			return dec(Low, "package metadata query")
		}
	case "snap", "flatpak":
		if sub == "list" || sub == "info" {
			return dec(Low, "package metadata query")
		}
	case "pacman":
		if containsAny(args, "-Q", "-Qi", "-Ql", "-Qs") {
			return dec(Low, "package query")
		}
	}

	if readOnlyCommands[name] && !hasSlash {
		return dec(Low, "read-only inspection")
	}
	if systemHigh[name] {
		d := dec(High, name+" changes system state")
		d.Recovery = "system-level change; reverse manually (e.g. reinstall/restore previous state)"
		return d
	}
	if name == "rm" || name == "rmdir" {
		if isRecursive(args) || name == "rmdir" {
			d := dec(High, "recursive deletion")
			d.Recovery = "deleted files are not recoverable unless backed up"
			return d
		}
		d := dec(Medium, "deletes files")
		d.Recovery = "deleted files are not recoverable unless backed up"
		return d
	}
	if name == "chmod" || name == "chown" || name == "chgrp" {
		if isRecursive(args) {
			return withRecovery(dec(High, "recursive permission/ownership change"), "record current modes first; hard to reverse")
		}
		return withKey(dec(Medium, "changes permissions/ownership"), name, "")
	}
	if name == "kill" || name == "pkill" || name == "killall" {
		return withRecovery(dec(Medium, "terminates processes"), "restart the affected process")
	}
	if fileMutators[name] {
		return withKey(dec(Medium, name+" modifies files"), name, "")
	}
	return withKey(dec(Medium, fmt.Sprintf("unrecognised command %q; side effects unknown", name)), name, sub)
}

func classifyGit(args []string, sub string) Decision {
	switch sub {
	case "status", "log", "diff", "show", "branch", "remote", "rev-parse", "ls-files", "blame", "describe", "config", "shortlog", "tag", "stash", "ls-remote", "reflog", "grep", "check-ignore", "version":
		if (sub == "config" && !hasFlag(args, "--get", "--list", "-l", "--get-all")) ||
			(sub == "branch" && containsAny(args, "-d", "-D", "-m", "-M", "--delete")) ||
			(sub == "tag" && containsAny(args, "-d", "--delete")) ||
			(sub == "stash" && !containsAny(args, "list", "show")) {
			return withKey(dec(Medium, "git "+sub+" modifies repository state"), "git", sub)
		}
		return dec(Low, "read-only git query")
	case "push":
		return withRecovery(dec(High, "git push publishes to a remote"), "remote history may be hard to revert")
	case "reset":
		if containsAny(args, "--hard") {
			return withRecovery(dec(High, "git reset --hard discards uncommitted work"), "recover via git reflog only for committed work")
		}
	case "clean":
		return withRecovery(dec(High, "git clean deletes untracked files"), "untracked files are not recoverable")
	case "checkout", "restore", "switch":
		if containsAny(args, "--force", "-f", ".") {
			return withRecovery(dec(High, "git "+sub+" may discard local changes"), "uncommitted changes are not recoverable")
		}
	}
	return withKey(dec(Medium, "git "+sub+" modifies the repository"), "git", sub)
}

func classifyDocker(args []string, sub string) Decision {
	if sub == "compose" || sub == "container" || sub == "image" || sub == "volume" || sub == "network" || sub == "system" {
		rest := afterFirst(args, sub)
		sub2 := firstNonFlag(rest)
		switch sub2 {
		case "ls", "list", "ps", "logs", "config", "inspect", "df", "events", "top", "images", "version", "info":
			return dec(Low, "read-only docker query")
		case "prune", "rm", "down", "kill":
			return withRecovery(dec(High, "docker "+sub+" "+sub2+" removes containers/images/volumes"), "removed volumes and images may be unrecoverable")
		}
		return withKey(dec(Medium, "docker "+sub+" "+sub2+" changes container state"), "docker", sub+" "+sub2)
	}
	switch sub {
	case "ps", "images", "logs", "inspect", "info", "version", "top", "stats", "events", "port", "diff", "history", "search":
		return dec(Low, "read-only docker query")
	case "rm", "rmi", "prune", "kill":
		return withRecovery(dec(High, "docker "+sub+" removes containers/images"), "removed data may be unrecoverable")
	}
	return withKey(dec(Medium, "docker "+sub+" changes container state"), "docker", sub)
}

func classifySystemctl(args []string, sub string) Decision {
	switch sub {
	case "status", "is-active", "is-enabled", "is-failed", "list-units", "list-unit-files", "list-timers", "list-sockets", "show", "cat", "list-dependencies", "--version":
		return dec(Low, "read-only service query")
	}
	user := hasFlag(args, "--user")
	d := dec(High, "systemctl "+sub+" changes service state")
	d.Effects = "starts, stops or reconfigures a service"
	d.Recovery = "run the inverse systemctl command (e.g. start/stop, enable/disable)"
	if user {
		d.Risk = Medium
		d.Key = "systemctl:user:" + sub
	}
	return d
}

func classifyNode(name, sub string, args []string) Decision {
	switch sub {
	case "config":
		if containsAny(args, "get", "list") {
			return dec(Low, "read-only "+name+" config query")
		}
	case "ls", "list", "outdated", "view", "info", "why", "audit", "root", "bin", "--version", "version", "explain":
		return dec(Low, "read-only "+name+" query")
	case "publish", "unpublish", "deprecate", "login", "adduser", "token", "owner":
		return withRecovery(dec(High, name+" "+sub+" affects a remote registry/account"), "may be irreversible")
	}
	return withKey(dec(Medium, name+" "+sub+" installs packages or runs project scripts"), name, sub)
}

func classifyGo(sub string) Decision {
	switch sub {
	case "version", "env", "list", "doc", "vet":
		return dec(Low, "read-only go query")
	}
	return withKey(dec(Medium, "go "+sub+" compiles or runs project code / modifies modules"), "go", sub)
}

func withKey(d Decision, name, sub string) Decision {
	d.Key = "shell_exec:" + strings.TrimSpace(name+" "+sub)
	return d
}

func withRecovery(d Decision, r string) Decision { d.Recovery = r; return d }

// touchesOutside reports whether any path-like argument resolves outside the workspace.
func (e *Engine) touchesOutside(args []string, workdir string) bool {
	for _, a := range args {
		if a == "" || a == "-" {
			continue
		}
		cand := a
		if strings.HasPrefix(a, "-") {
			i := strings.Index(a, "=")
			if i < 0 {
				continue
			}
			cand = a[i+1:]
		}
		if strings.HasPrefix(cand, "/") || strings.HasPrefix(cand, "~") || strings.HasPrefix(cand, "../") || cand == ".." {
			if !e.Scope.Contains(e.Scope.Resolve(cand, workdir)) {
				return true
			}
		}
	}
	return false
}

func unwrap(name string, args []string) []string {
	i := 0
	switch name {
	case "env":
		for i < len(args) && (strings.HasPrefix(args[i], "-") || strings.Contains(args[i], "=")) {
			if args[i] == "-u" || args[i] == "-C" {
				i++
			}
			i++
		}
	case "timeout":
		for i < len(args) && strings.HasPrefix(args[i], "-") {
			if args[i] == "-s" || args[i] == "-k" {
				i++
			}
			i++
		}
		i++ // duration
	case "nice", "ionice":
		for i < len(args) && strings.HasPrefix(args[i], "-") {
			if args[i] == "-n" || args[i] == "-c" {
				i++
			}
			i++
		}
	case "stdbuf":
		for i < len(args) && strings.HasPrefix(args[i], "-") {
			i++
		}
	default:
		for i < len(args) && strings.HasPrefix(args[i], "-") {
			i++
		}
	}
	if i >= len(args) {
		return nil
	}
	return args[i:]
}

func stripFlags(args []string) []string {
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		i++
	}
	return args[i:]
}

func firstNonFlag(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

func afterFirst(args []string, v string) []string {
	for i, a := range args {
		if a == v {
			return args[i+1:]
		}
	}
	return nil
}

func isRecursive(args []string) bool {
	for _, a := range args {
		if a == "--recursive" || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsAny(a, "rR")) {
			return true
		}
	}
	return false
}

func hasFlag(args []string, flags ...string) bool { return containsAny(args, flags...) }

func containsAny(args []string, vals ...string) bool {
	for _, a := range args {
		for _, v := range vals {
			if a == v {
				return true
			}
		}
	}
	return false
}

func anyPrefix(args []string, p string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, p) {
			return true
		}
	}
	return false
}
