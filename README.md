# ScoutClaw

ScoutClaw is an autonomous diagnostic and repair agent for the computer or server it runs on. You describe a problem in plain language; it **inspects** the system, forms and tests **hypotheses**, applies the **smallest justified fix** (asking you first for anything risky) and **verifies** the result. It is under your control at all times: every action passes a policy engine in code, risky actions need your approval, and Ctrl-C stops the task and every process it started.

> Status: first production-oriented milestone. Linux (tested on Ubuntu) is the supported platform. The code builds for macOS and Windows, but built-in diagnostics are Linux-only and nothing has been tested there — see [Known limitations](#known-limitations).

## How it works

```
 you ──task──▶ agent loop ──▶ model (OpenAI Responses API, native tool calls)
                  │                │ proposes a tool call
                  ▼                ▼
        executor:  validate args → policy (risk) → approval → run with limits
                  │      → redact + truncate output → audit log → verification evidence
                  ▼
        observation back to the model → replan → … → finish (checked against evidence)
```

The model only *proposes*. The runtime validates, authorizes, executes and verifies. The agent package never runs commands itself.

## Quick start

Install the `scout` command (prebuilt binary, checksum-verified, no Go needed):

**macOS / Linux**

```bash
curl -fsSL https://raw.githubusercontent.com/sshuvoo/scout-claw/main/install.sh | sh
```

**Windows (PowerShell)**

```powershell
irm https://raw.githubusercontent.com/sshuvoo/scout-claw/main/install.ps1 | iex
```

**Windows (CMD)**

```bat
powershell -NoProfile -ExecutionPolicy Bypass -Command "irm https://raw.githubusercontent.com/sshuvoo/scout-claw/main/install.ps1 | iex"
```

Then, on any OS:

```bash
scout setup                                    # paste your OpenAI API key once (hidden input)
scout doctor --online                          # verify key, model, shells and connectivity
scout run --approval readonly "How much disk space and memory is free?"
```

## Installation

You need an OpenAI API key (`OPENAI_API_KEY`, Responses API, default model `gpt-5.5`) and one of:

| Method | OS | Needs | Command |
|---|---|---|---|
| Release script | macOS, Linux | curl | `curl -fsSL https://raw.githubusercontent.com/sshuvoo/scout-claw/main/install.sh \| sh` |
| Release script | Windows | PowerShell 5.1+ | `irm https://raw.githubusercontent.com/sshuvoo/scout-claw/main/install.ps1 \| iex` |
| Make | macOS, Linux | Go 1.27.1+, make | `make install` (installs `~/.local/bin/scout`; `sudo make install PREFIX=/usr/local` for system-wide) |
| Go only | any | Go 1.27.1+ | `go install ./cmd/scout` (installs `~/go/bin/scout`, `%USERPROFILE%\go\bin\scout.exe` on Windows) |

Where the scripts install:

| | macOS / Linux (`install.sh`) | Windows (`install.ps1`) |
|---|---|---|
| Binary | `~/.local/bin/scout` | `%USERPROFILE%\.local\bin\scout.exe` |
| PATH | prints the line to add to `~/.zshrc` / `~/.bashrc` if needed | adds the folder to your user PATH (opt out with `SCOUT_NO_MODIFY_PATH=1`) |
| Options | `SCOUT_VERSION=v0.6.2`, `SCOUT_INSTALL_DIR=...` | same: `$env:SCOUT_VERSION`, `$env:SCOUT_INSTALL_DIR` |

Both scripts download the archive for your OS and CPU (amd64 or arm64) from the latest GitHub release and refuse to install if its SHA-256 does not match `checksums.txt`. Prefer to read a script before running it? Download it, inspect it, then run `sh install.sh` or `powershell -File install.ps1`.

On Windows, also install [Git for Windows](https://git-scm.com/downloads/win) so the agent can run bash scripts (`shell_script`) in addition to PowerShell (`shell_powershell`); `scout doctor` shows which shells were found.

Open a new terminal, check with `scout version`, then run `scout setup` to store your API key (saved to `~/.scoutclaw.env`, mode 600, works from any directory). A project-local `./.env` only applies when you run `scout` from that directory. Later, `scout update` upgrades in place on every OS.

Full guide, including PATH setup, upgrading from the first version, updating, uninstalling and troubleshooting: **[docs/installation.md](docs/installation.md)**.

## Configure

Precedence: **flags > environment variables > `.env` files > defaults**. `.env` files are read from `./.env`, `~/.scoutclaw.env`, `~/.scout.env`, `~/.env` (unchanged from before) and never override variables that are already set. Copy `.env.example` and fill in the key; real `.env` files are git-ignored.

| Variable | Default | Meaning |
|---|---|---|
| `OPENAI_API_KEY` | – (required) | Credentials. Never printed or logged. |
| `OPENAI_BASE_URL` | provider default | Alternative endpoint (must support the Responses API). |
| `SCOUTCLAW_MODEL` / `--model` | `gpt-5.5` | Model name. |
| `SCOUTCLAW_WORKSPACE` / `--workspace` | current dir | The only place edits are allowed without extra approval. |
| `SCOUTCLAW_APPROVAL` / `--approval` | `ask` | `ask`, `edits`, `auto` or `readonly` (see below). |
| `SCOUTCLAW_MAX_STEPS` / `--max-steps` | `100` | Model iterations per task (1–500). Coding tasks need far more than diagnostics; when the limit is hit, ScoutClaw asks the model for an honest status report, and in chat you can say "continue". |
| `SCOUTCLAW_TOOL_TIMEOUT` | `2m` | Per-command timeout. |
| `SCOUTCLAW_TASK_TIMEOUT` | `30m` | Whole-task timeout. |
| `SCOUTCLAW_MAX_OUTPUT` | `16384` | Bytes of tool output kept per call. |
| `SCOUTCLAW_MAX_READ` | `65536` | Bytes of file content `fs_read` may return per call (4096–1048576). |
| `SCOUTCLAW_COMPACT_AT` | `200000` | When the conversation reaches this many tokens, the provider compacts earlier turns (OpenAI Responses `context_management`), and ScoutClaw restates the task and plan to the model. `0` turns it off; servers that do not support it are detected and it is skipped. Minimum 20000. `/status` shows the current conversation size. |
| `SCOUTCLAW_STATE_DIR` | `~/.local/state/scoutclaw` | Sessions, audit log, file backups (mode 0600/0700). |

Invalid values fail at startup with a clear message. `scout config` shows the effective values (key masked).

## Usage

```bash
scout run "Find out why my application is not starting and fix it"
scout "why is my disk full?"            # same as run (backward compatible)
scout                                   # interactive chat (also: scout chat)
scout run --approval readonly "Why is this machine slow?"   # look, never touch
scout run --workspace ~/myapp "The tests fail — find the root cause and fix it"
scout run --resume last "Now also check the logs"            # continue a session
```

| Command | What it does |
|---|---|
| `run <task>` | Run one task and exit. Exit code is non-zero if the task was cancelled, hit a limit, failed or ended unresolved. |
| `chat` | Full-screen chat with the input box pinned to the bottom, a live status bar (model, approvals, workspace, tokens, spinner with the running command), scrollback (PgUp/PgDn, mouse wheel), prompt history (↑/↓), slash-command autocomplete (Tab) and an inline approval panel. Ctrl+J inserts a newline, Ctrl+C cancels the task, Ctrl+D quits. `--plain` (or a non-terminal) uses the simple line-based mode. Interactive session; follow-ups keep the conversation. Slash commands inside chat: `/help`, `/config`, `/status`, `/model [name]`, `/approval [ask\|auto\|readonly]`, `/key` (set API key), `/history`, `/clear`, `/reset`, `/exit`. `/model` and `/approval` last for the session. |
| `update [--check] [--version vX.Y.Z]` | Update scout in place: downloads the release for your OS/arch, verifies its SHA-256 and replaces the running executable. |
| `setup` | Enter your API key once (hidden input); saved for all future runs. |
| `doctor [--online]` | Checks config, state dir, optional tools, DNS/TCP to the API host; `--online` sends one tiny request to prove key and model work. |
| `status` | Version, platform, limits, session counts, last session, audit log — no secrets. |
| `config` | Effective configuration with the variable each value comes from. |
| `history [id]` | List sessions; with an id, show its tasks and every recorded action, policy decision and approval. |
| `version` | Version information. |

Chat/interactive tips: Ctrl-C cancels the running task (children are terminated); a second Ctrl-C exits immediately.

## What the agent can do

Tools exposed to the model (all go through the same pipeline):

- **Filesystem (read):** `fs_list`, `fs_read` (line ranges, tail mode), `fs_read_many` (several files in one call), `fs_stat`, `fs_glob` (file patterns, newest first), `fs_grep` (regex or fixed-string search with context, file types and files/count output). Searches respect `.gitignore` and skip binary and credential files.
- **Filesystem (edit):** `fs_write` (create or overwrite), `fs_patch` (exact-text replacement; `edits[]` applies several to one file atomically), `fs_lines` (insert, replace or delete by line number, each guarded by the expected current text), `fs_mkdir`, `fs_copy`, `fs_move`, `fs_delete`. Every edit shows a diff (or a description) before approval, keeps a backup and writes atomically. A file must have been read with `fs_read` before it can be edited, and an edit is refused if the file changed since that read. BOM, CRLF/LF line endings, the trailing newline and file mode are kept. `fs_delete` and anything replaced by an overwrite go to a per-session trash in `SCOUTCLAW_STATE_DIR/trash`, never `unlink`.
- **Multi-file patches:** `fs_apply_patch` applies one patch that adds, updates, renames and deletes files atomically (all or nothing, rolled back on failure). It accepts the `*** Begin Patch` format of OpenAI's Codex CLI and standard unified diffs; hunks are found by context, tolerating small whitespace differences.
- **Project discovery:** `project_info` reads manifests (go.mod, package.json, pyproject.toml, Cargo.toml, Maven/Gradle, .NET, Ruby, PHP) and reports the build/test/lint/format commands, package manager, Makefile targets, CI workflows, instruction files (AGENTS.md, CONTRIBUTING.md, …), git branch and nested projects. It never runs anything.
- **Background processes:** `proc_start` runs dev servers, watchers and databases in the background (one executable, no shell; optionally waits for a readiness line such as `listening on`), `proc_output` returns what they printed since the last check, `proc_stop` stops a process and everything it spawned, `proc_list` lists them. All of them stop when ScoutClaw exits; in chat, `/procs` lists them and `/procs stop <id>` stops one.
- **Change journal:** `fs_changes` lists this session's file changes with a combined diff per file; `fs_undo` reverts the latest ones (and refuses if a file was changed again afterwards). In chat: `/diff` and `/undo [n]`.
- **Shell:** `shell_exec` (one executable + argument array, **no shell**), `shell_script` (bash: the system bash on Linux/macOS, Git Bash on Windows; always high risk), `shell_powershell` (Windows only: PowerShell 7, falling back to Windows PowerShell 5.1; always high risk). Override discovery with `SCOUT_BASH_PATH` / `SCOUT_POWERSHELL_PATH`; `scout doctor` shows what was found.
- **System (Linux):** `sys_info` (OS, kernel, load, memory, swap, disk and inode usage), `process_list` (CPU/memory top, secrets in command lines masked), `env_list` (secret values masked), `dev_runtimes`. Service state and logs use `shell_exec` with `systemctl status` / `journalctl`.
- **Network:** `net_dns`, `net_connect` (TCP), `net_http` (GET/HEAD, no credentials, bounded).
- **Bookkeeping:** `update_plan`, `finish`.

## Safety model (enforced in code)

**Risk classification** is based on the actual executable, subcommand, arguments and target paths (`internal/policy`):

| Risk | Examples | `ask` (default) | `auto` | `readonly` |
|---|---|---|---|---|
| Low | `ls`, `df`, `git status`, `systemctl status`, `docker ps`, reading files, bounded network probes | runs | runs | runs |
| Medium | `go test`, `npm install`, `docker restart`, edits inside the workspace, unknown commands | asks (`a` = allow this kind for the session) | runs | denied |
| High | writes outside the workspace, `rm -r`, `systemctl restart`, `apt`, `git push`, `sudo`, firewall/permission changes, any `shell_script` / `shell_powershell` | asks (y/n only) | asks | denied |
| Refused | `rm -rf /` or `~`, `mkfs`/`dd` to devices, killing PID 1, piping downloads into a shell, reading SSH/GPG/cloud credential files and `/etc/shadow`; on Windows/macOS also `diskpart`/`format`/`diskutil erase*`, deleting shadow copies, `iex` of downloaded content, encoded PowerShell, killing critical system processes | never runs | never | never |

`edits` behaves like `ask`, except that medium-risk file edits inside the workspace (`fs_write`, `fs_patch`, `fs_lines`, `fs_apply_patch`, `fs_mkdir`, `fs_copy`, `fs_move`, `fs_undo`) run without a prompt; a patch that deletes files still asks. Deletes, shell commands and anything outside the workspace still ask.

Further guarantees:

- Runs as the current user. `sudo`/`su`/`pkexec` are always high risk and need explicit approval; nothing escalates silently.
- Without a terminal (pipes, CI) approvals are **denied**, not assumed. High-risk actions can never be pre-approved by a flag.
- Paths are resolved (including symlinks) before judging scope, so `../` and symlink tricks do not escape the workspace. Tools re-check scope themselves as a second layer.
- Commands run in their own process group with timeouts, output caps (head and tail are kept) and cancellation; the whole group is killed. The API key is stripped from child environments.
- Secrets (API keys, tokens, passwords, private keys, `user:pass@` URLs, secret-named variables) are redacted from model context, terminal output and the audit log.
- File contents, logs, web responses and command output are untrusted data; the system prompt tells the model so, and the policy does not depend on the model complying.
- Identical failed calls are not re-executed; repeated failures stop the task; step and time budgets are enforced; when a budget runs out the model is asked for an honest status report.
- **Verification is checked, not trusted:** the model marks commands `purpose=verify`; a result can only be reported as *verified* if such a command succeeded **after** the last change. Otherwise the report is rejected once and then downgraded to *unverified* (and says so).
- Every request, policy decision, approval, result and task outcome is appended to `audit.jsonl` (redacted, mode 0600). `scout history <id>` renders it.

## Project layout

```
cmd/scoutclaw/        entry point
internal/cli/         Cobra commands, terminal UI, approvals, runtime wiring
internal/agent/       the agent loop, system prompt, limits, wrap-up
internal/llm/         provider interface, OpenAI Responses implementation, retry, test double
internal/tools/       tool interface + registry; shell/ filesystem/ system/ network/ task/ builtin/
internal/policy/      risk classification, scope/path resolution, approval modes
internal/executor/    validate → policy → approve → run → redact → audit → evidence
internal/verify/      evidence-based verification verdicts
internal/audit/       redacted JSONL audit trail
internal/session/     JSON session records, resume metadata
internal/redact/      secret redaction
internal/config/      env/.env/defaults + validation
internal/platform/    OS detection
tests/integration/    tests against the compiled binary
```

See [docs/architecture.md](docs/architecture.md) for design decisions and [docs/installation.md](docs/installation.md) for installation.

## Testing

```bash
make test              # go test ./...  (no API key or network required)
make lint              # gofmt check + go vet
make race              # go test -race (requires cgo / a C compiler)
go test ./tests/...    # integration tests on the built binary
```

Agent tests use a scripted fake model; execution tests use temp directories only.

## Known limitations

- Platform: only Linux is implemented and tested. macOS/Windows build, but `sys_info`/`process_list` are unavailable there, process-group termination is Unix-only, and no support is claimed.
- Provider: only OpenAI's Responses API is implemented (via the `llm.Provider` interface). Conversation history is held by the provider (`previous_response_id`, `truncation: auto`), so `--resume` depends on the provider still retaining that response. Streaming is not implemented.
- Command classification is heuristic. Unknown commands are medium risk and you see them before they run, but a pre-approved (`auto`) unknown binary can do anything the user can. Prefer `ask`.
- Persistence is JSON files, not SQLite; there is no cross-session user memory yet. No MCP client, TUI, or CI release automation has been exercised (`.goreleaser.yaml` is provided but untested).
- `fs_*` write/patch are text-only (≤ 2 MiB). `fs_read` of a path outside the workspace is allowed for diagnostics (logs, `/etc` config) except credential files.
- The race detector could not be run in the development environment (no C compiler).

## License

MIT
