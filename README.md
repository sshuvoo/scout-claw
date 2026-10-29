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

```bash
git clone https://github.com/sshuvoo/scout-claw.git && cd scout-claw
make install                                   # installs the global `scout` command (~/.local/bin)
echo 'OPENAI_API_KEY=your_key_here' > ~/.scoutclaw.env && chmod 600 ~/.scoutclaw.env
scout doctor --online                          # verify key, model and connectivity
scout run --approval readonly "How much disk space and memory is free?"
```

## Installation

You need an OpenAI API key (`OPENAI_API_KEY`, Responses API, default model `gpt-5.5`) and one of:

| Method | Needs | Command |
|---|---|---|
| Make | Go 1.27.1+, make | `make install` (installs `~/.local/bin/scout`; `sudo make install PREFIX=/usr/local` for system-wide) |
| Go only | Go 1.27.1+ | `go install ./cmd/scout` (installs `~/go/bin/scout`) |
| Release script | curl, a published release | `curl -fsSL https://raw.githubusercontent.com/sshuvoo/scout-claw/main/install.sh \| sh` |

Make sure the install directory is on your `PATH`, then check with `scout version`. For a global command, keep the API key in `~/.scoutclaw.env` (or export it): a project-local `./.env` only applies when you run `scout` from that directory.

Full guide, including PATH setup, upgrading from the first version, updating, uninstalling and troubleshooting: **[docs/installation.md](docs/installation.md)**.

## Configure

Precedence: **flags > environment variables > `.env` files > defaults**. `.env` files are read from `./.env`, `~/.scoutclaw.env`, `~/.scout.env`, `~/.env` (unchanged from before) and never override variables that are already set. Copy `.env.example` and fill in the key; real `.env` files are git-ignored.

| Variable | Default | Meaning |
|---|---|---|
| `OPENAI_API_KEY` | – (required) | Credentials. Never printed or logged. |
| `OPENAI_BASE_URL` | provider default | Alternative endpoint (must support the Responses API). |
| `SCOUTCLAW_MODEL` / `--model` | `gpt-5.5` | Model name. |
| `SCOUTCLAW_WORKSPACE` / `--workspace` | current dir | The only place edits are allowed without extra approval. |
| `SCOUTCLAW_APPROVAL` / `--approval` | `ask` | `ask`, `auto` or `readonly` (see below). |
| `SCOUTCLAW_MAX_STEPS` / `--max-steps` | `25` | Model iterations per task (1–200). |
| `SCOUTCLAW_TOOL_TIMEOUT` | `2m` | Per-command timeout. |
| `SCOUTCLAW_TASK_TIMEOUT` | `30m` | Whole-task timeout. |
| `SCOUTCLAW_MAX_OUTPUT` | `16384` | Bytes of tool output kept per call. |
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
| `chat` | Interactive session; follow-ups keep the conversation. In-chat: `help`, `clear`, `reset` (new session), `exit`. |
| `doctor [--online]` | Checks config, state dir, optional tools, DNS/TCP to the API host; `--online` sends one tiny request to prove key and model work. |
| `status` | Version, platform, limits, session counts, last session, audit log — no secrets. |
| `config` | Effective configuration with the variable each value comes from. |
| `history [id]` | List sessions; with an id, show its tasks and every recorded action, policy decision and approval. |
| `version` | Version information. |

Chat/interactive tips: Ctrl-C cancels the running task (children are terminated); a second Ctrl-C exits immediately.

## What the agent can do

Tools exposed to the model (all go through the same pipeline):

- **Filesystem:** `fs_list`, `fs_read` (line ranges, tail mode), `fs_stat`, `fs_search` (regex + name glob), `fs_write`, `fs_patch` (exact-match edit). Edits show a diff before approval, keep a backup, and write atomically.
- **Shell:** `shell_exec` (one executable + argument array, **no shell**), `shell_script` (`sh -c`, always high risk).
- **System (Linux):** `sys_info` (OS, kernel, load, memory, swap, disk and inode usage), `process_list` (CPU/memory top, secrets in command lines masked), `env_list` (secret values masked), `dev_runtimes`. Service state and logs use `shell_exec` with `systemctl status` / `journalctl`.
- **Network:** `net_dns`, `net_connect` (TCP), `net_http` (GET/HEAD, no credentials, bounded).
- **Bookkeeping:** `update_plan`, `finish`.

## Safety model (enforced in code)

**Risk classification** is based on the actual executable, subcommand, arguments and target paths (`internal/policy`):

| Risk | Examples | `ask` (default) | `auto` | `readonly` |
|---|---|---|---|---|
| Low | `ls`, `df`, `git status`, `systemctl status`, `docker ps`, reading files, bounded network probes | runs | runs | runs |
| Medium | `go test`, `npm install`, `docker restart`, edits inside the workspace, unknown commands | asks (`a` = allow this kind for the session) | runs | denied |
| High | writes outside the workspace, `rm -r`, `systemctl restart`, `apt`, `git push`, `sudo`, firewall/permission changes, any `shell_script` | asks (y/n only) | asks | denied |
| Refused | `rm -rf /` or `~`, `mkfs`/`dd` to devices, killing PID 1, piping downloads into a shell, reading SSH/GPG/cloud credential files and `/etc/shadow` | never runs | never | never |

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
