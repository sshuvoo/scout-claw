package policy

import (
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
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
	// macOS
	"sw_vers", "system_profiler", "vm_stat", "mdfind", "mdls", "otool", "fs_usage", "shasum",
	// Windows
	"where", "tasklist", "ipconfig", "systeminfo", "ver", "findstr", "getmac", "driverquery",
)

var wrappers = setOf("env", "nice", "ionice", "nohup", "timeout", "time", "stdbuf", "command")
var privilege = setOf("sudo", "su", "doas", "pkexec", "runuser", "setpriv")
var shells = setOf("sh", "bash", "zsh", "dash", "ksh", "fish", "csh", "tcsh", "pwsh", "powershell", "cmd")
var inlineInterp = map[string]string{"python": "-c", "python3": "-c", "node": "-e", "nodejs": "-e", "perl": "-e", "ruby": "-e", "php": "-r"}
var systemHigh = setOf("apt", "apt-get", "aptitude", "dpkg", "dnf", "yum", "rpm", "pacman", "zypper", "apk", "snap", "flatpak",
	"reboot", "shutdown", "poweroff", "halt", "init", "telinit", "passwd", "useradd", "userdel", "usermod", "groupadd", "groupdel",
	"chpasswd", "visudo", "mount", "umount", "fdisk", "parted", "sfdisk", "gdisk", "mkswap", "swapon", "swapoff", "modprobe",
	"insmod", "rmmod", "iptables", "ip6tables", "nft", "ufw", "firewall-cmd", "setenforce", "aa-enforce", "aa-complain", "crontab", "at", "update-grub", "grub-install",
	// macOS
	"launchctl", "diskutil", "csrutil", "spctl", "nvram", "systemsetup", "dscl", "pmset", "softwareupdate", "installer", "tmutil", "kextload", "kextunload",
	// Windows
	"reg", "sc", "netsh", "schtasks", "winget", "choco", "scoop", "msiexec", "bcdedit", "takeown", "cipher", "wsl", "dism", "sfc", "vssadmin", "wbadmin", "wevtutil", "setx")
var fileMutators = setOf("rm", "rmdir", "mv", "cp", "mkdir", "touch", "ln", "chmod", "chown", "chgrp", "truncate", "tee", "install", "rsync", "unlink", "sed", "patch")

// windowsNative are Windows programs that take "/X" switches. On Windows
// their switches must not be mistaken for rooted paths ("/PID" is not C:\PID).
var windowsNative = setOf("taskkill", "tasklist", "icacls", "takeown", "sc", "reg", "netsh", "schtasks", "ipconfig", "findstr",
	"where", "robocopy", "xcopy", "attrib", "shutdown", "systeminfo", "driverquery", "cipher", "wevtutil", "dism", "sfc", "vssadmin", "wbadmin", "bcdedit", "msiexec", "cmd")

var windowsSwitch = regexp.MustCompile(`^/[A-Za-z?][A-Za-z0-9:_-]*$`)

// withoutSwitches returns argv's arguments minus Windows "/X" switches when
// argv runs a Windows-native program on Windows.
func withoutSwitches(argv []string) []string {
	if runtime.GOOS != "windows" || !windowsNative[commandName(argv[0])] {
		return argv[1:]
	}
	var out []string
	for _, a := range argv[1:] {
		if !windowsSwitch.MatchString(a) {
			out = append(out, a)
		}
	}
	return out
}

// criticalWindowsProcs crash or log off the system when terminated.
var criticalWindowsProcs = setOf("system", "csrss", "wininit", "winlogon", "lsass", "smss", "services", "svchost")

// commandName normalises an executable for classification: its base name,
// lower-cased where the filesystem ignores case, without Windows' .exe.
func commandName(exe string) string {
	name := foldCase(filepath.Base(exe))
	if runtime.GOOS == "windows" {
		name = strings.TrimSuffix(strings.TrimSuffix(name, ".exe"), ".com")
	}
	return name
}

// classifyShellInvocation handles a shell run directly through shell_exec. An
// inline script is classified like the matching script tool.
func (e *Engine) classifyShellInvocation(name string, args []string, workdir string) Decision {
	switch name {
	case "pwsh", "powershell":
		script := strings.Join(args, " ")
		if why := forbiddenPowerShell(" " + script); why != "" {
			return forbid(strings.TrimPrefix(why, "refused: "))
		}
		if containsFold(args, "-file", "-f") {
			return dec(Medium, "runs a PowerShell script file")
		}
		if len(args) == 1 && oneOfFold(args[0], "-version", "-v", "--version", "-help", "-?") {
			return dec(Low, "version/help query")
		}
		return dec(High, "inline PowerShell passed to "+name)
	case "cmd":
		for i, a := range args {
			if strings.EqualFold(a, "/c") || strings.EqualFold(a, "/k") {
				if why := forbiddenPowerShell(" " + strings.Join(args[i+1:], " ")); why != "" {
					return forbid(strings.TrimPrefix(why, "refused: "))
				}
				return dec(High, "inline command passed to cmd.exe")
			}
		}
		return dec(Medium, "runs cmd.exe")
	}
	for i, a := range args {
		if a == "-c" || (strings.HasPrefix(a, "-") && strings.HasSuffix(a, "c") && !strings.HasPrefix(a, "--")) {
			script := ""
			if i+1 < len(args) {
				script = args[i+1]
			}
			if d := e.classifyBash(script, workdir); d.Forbidden {
				return d
			}
			return dec(High, "inline shell script passed to "+name)
		}
	}
	return dec(Medium, "runs a shell script file")
}

func oneOf(s string, vals ...string) bool {
	for _, v := range vals {
		if s == v {
			return true
		}
	}
	return false
}

func oneOfFold(s string, vals ...string) bool {
	for _, v := range vals {
		if strings.EqualFold(s, v) {
			return true
		}
	}
	return false
}

func containsFold(args []string, vals ...string) bool {
	for _, a := range args {
		if oneOfFold(a, vals...) {
			return true
		}
	}
	return false
}

func anyPrefixFold(args []string, prefixes ...string) bool {
	for _, a := range args {
		for _, p := range prefixes {
			if len(a) >= len(p) && strings.EqualFold(a[:len(p)], p) {
				return true
			}
		}
	}
	return false
}

func anyContains(args []string, sub string) bool {
	for _, a := range args {
		if strings.Contains(a, sub) {
			return true
		}
	}
	return false
}

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
	name := commandName(exe)
	hasSlash := strings.ContainsAny(exe, `/\`)

	// Credential files are off limits for every command.
	for _, a := range args {
		cand := a
		if i := strings.Index(a, "="); i > 0 && strings.HasPrefix(a, "-") {
			cand = a[i+1:]
		}
		if cand != "" && (strings.ContainsAny(cand, `/\`) || strings.HasPrefix(cand, "~") || strings.HasPrefix(cand, ".") || filepath.IsAbs(cand)) && IsSensitive(e.Scope.Resolve(cand, workdir)) {
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
		return e.classifyShellInvocation(name, args, workdir)
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
				if isProtectedDir(e.Scope.Resolve(a, workdir)) || a == "/*" || a == "~/*" {
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
	case name == "diskpart" || (name == "format" && runtime.GOOS == "windows"):
		return forbid("partitioning or formatting a disk")
	case name == "diskutil" && (strings.Contains(strings.ToLower(firstNonFlag(args)), "erase") || containsAny(args, "zeroDisk", "randomDisk", "secureErase", "partitionDisk", "reformat")):
		return forbid("erasing or repartitioning a disk")
	case (name == "vssadmin" || name == "wbadmin") && containsFold(args, "delete"):
		return forbid("deleting backups or shadow copies")
	case name == "taskkill":
		for i, a := range args {
			next := ""
			if i+1 < len(args) {
				next = strings.ToLower(args[i+1])
			}
			if strings.EqualFold(a, "/pid") && next == "4" || strings.EqualFold(a, "/im") && criticalWindowsProcs[strings.TrimSuffix(next, ".exe")] {
				return forbid("terminating a critical Windows system process")
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
	case "sysctl":
		if hasFlag(args, "-w") || anyContains(args, "=") {
			return withRecovery(dec(High, "sysctl changes kernel parameters"), "set the previous value again")
		}
		return dec(Low, "reads kernel parameters")
	// macOS
	case "launchctl":
		if oneOf(sub, "list", "print", "print-disabled", "blame", "procinfo", "version", "managerpid", "hostinfo") {
			return dec(Low, "read-only launchd query")
		}
		return withRecovery(dec(High, "launchctl "+sub+" changes service state"), "run the inverse launchctl command (bootstrap/bootout, enable/disable)")
	case "diskutil":
		if oneOf(sub, "list", "info", "activity", "listFilesystems") || (sub == "apfs" && containsAny(args, "list")) {
			return dec(Low, "read-only disk query")
		}
	case "brew":
		if oneOf(sub, "list", "ls", "info", "search", "outdated", "doctor", "config", "deps", "uses", "leaves", "home", "desc", "--prefix", "--cellar") || (sub == "services" && containsAny(args, "list", "info")) {
			return dec(Low, "read-only brew query")
		}
		return withKey(dec(Medium, "brew "+sub+" changes installed packages or services"), name, sub)
	case "defaults":
		if oneOf(sub, "read", "read-type", "domains", "find", "export") {
			return dec(Low, "reads preferences")
		}
		return withKey(dec(Medium, "defaults "+sub+" changes preferences"), name, sub)
	case "csrutil", "spctl":
		if containsAny(args, "status", "--status") {
			return dec(Low, "reads security status")
		}
	case "pmset":
		if hasFlag(args, "-g") {
			return dec(Low, "reads power settings")
		}
	case "softwareupdate":
		if hasFlag(args, "-l", "--list", "--history") {
			return dec(Low, "lists updates")
		}
	case "log":
		if oneOf(sub, "show", "stream", "stats") {
			return dec(Low, "reads system logs")
		}
	// Windows (flags are case-insensitive)
	case "reg":
		if strings.EqualFold(sub, "query") {
			return dec(Low, "reads the registry")
		}
		return withRecovery(dec(High, "reg "+sub+" changes the registry"), "export the key first (reg export) and re-import it to undo")
	case "sc":
		if oneOfFold(sub, "query", "queryex", "qc", "qdescription", "qfailure", "qtriggerinfo", "getdisplayname", "getkeyname", "enumdepend", "sdshow") {
			return dec(Low, "read-only service query")
		}
		return withRecovery(dec(High, "sc "+sub+" changes service state"), "run the inverse sc command (start/stop, config)")
	case "netsh":
		if containsFold(args, "show") && !containsFold(args, "set", "add", "delete", "reset", "import") {
			return dec(Low, "read-only network query")
		}
		return withRecovery(dec(High, "netsh changes network or firewall configuration"), "re-apply the previous configuration")
	case "schtasks":
		if containsFold(args, "/query") {
			return dec(Low, "lists scheduled tasks")
		}
	case "winget", "choco", "scoop":
		if oneOfFold(sub, "list", "search", "show", "info", "export", "outdated", "status") {
			return dec(Low, "package metadata query")
		}
	case "wsl":
		listOnly := len(args) > 0
		for _, a := range args {
			listOnly = listOnly && oneOfFold(a, "-l", "--list", "-v", "--verbose", "-q", "--quiet", "--all", "--running", "-o", "--online", "--status", "--version")
		}
		if listOnly {
			return dec(Low, "lists WSL distributions")
		}
	case "icacls":
		if !anyPrefixFold(args, "/grant", "/deny", "/remove", "/reset", "/setowner", "/inheritance", "/restore", "/setintegritylevel", "/substitute") {
			return dec(Low, "reads permissions")
		}
		if containsFold(args, "/t") {
			return withRecovery(dec(High, "recursive permission change"), "save ACLs first (icacls /save) and restore them")
		}
		return withKey(dec(Medium, "changes permissions"), name, "")
	case "taskkill":
		return withRecovery(dec(Medium, "terminates processes"), "restart the affected process")
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
		if strings.HasPrefix(cand, "/") || strings.HasPrefix(cand, "~") || strings.HasPrefix(cand, "../") || strings.HasPrefix(cand, `..\`) || cand == ".." || filepath.IsAbs(cand) {
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
