# Plan: coding tools for ScoutClaw

Goal: make ScoutClaw as good at editing code as a dedicated coding agent (Claude Code, Codex, Gemini CLI), while keeping what ScoutClaw already does well: every action goes through policy, gets approved, is backed up and is audited.

## 1. What exists today

| Tool | What it does | Gap for coding work |
|---|---|---|
| `fs_list` | Directory listing, depth 1-3 | Ignores `.gitignore` |
| `fs_read` | Numbered lines, offset/limit, tail mode | One file per call; 16 KB output cap; no version token for stale-edit detection |
| `fs_stat` | Metadata | fine |
| `fs_search` | Name glob plus regex grep (Go RE2) | No context lines, no `.gitignore`, no output modes, slow on big repos |
| `fs_write` | Create/overwrite, diff preview, backup, atomic rename | fine |
| `fs_patch` | Exact `old_string` → `new_string`, unique or `replace_all` | One edit per call; no line-based edits |

You can't copy, move, delete, mkdir, insert or replace lines, edit several places at once, apply a patch across files, or undo, except by going through `shell_exec`. Going through the shell skips the diff preview, the backups and the fine-grained policy.

What we can build on: `tools.Typed`, `Previewer` (diff in the approval panel), `FS.commit` (backup, then atomic write), `UnifiedDiff`, `policy.Scope`, `policy.IsSensitive`, and the `Grant` passed through the executor.

## 2. Design rules for every new tool

1. **Read before edit.** A per-task `FileState` tracker stores `{sha256, mtime, size}` for every file the agent reads. Any edit to an existing file is refused if the agent never read it, or if the file changed since the last read ("file changed on disk since you read it; re-read it"). Creating a new file is exempt. Claude Code does the same, and it prevents most lost-update and hallucinated-content bugs.
2. **Line edits carry a guard.** Line numbers go stale after an earlier edit, so every line-based edit must include `expect`: the current text of the first targeted line (or the whole range). If it doesn't match, the edit is refused and the error shows the actual lines.
3. **No hard deletes.** `fs_delete` and overwrites move the old content to `STATE_DIR/trash/<session>/…` or `STATE_DIR/backups/<session>/…`, so everything can be undone.
4. **Atomic and all-or-nothing.** A multi-edit or multi-file operation either applies completely or not at all (stage, then rename; roll back on failure).
5. **Keep the file's format.** Detect and keep CRLF or LF, a UTF-8 BOM, the trailing newline and the file mode. This matters on Windows.
6. **Preview everything.** Every mutating tool implements `Previewer`, so the user approves a diff, not a description.
7. **The policy engine stays the boundary.** Each tool gets an explicit `Classify` case. Unknown tools are already denied by default.
8. **Mutations show up as evidence.** Set `Result.Mutated` and put `path`/`backup`/`before_sha`/`after_sha` in `Metadata` so `verify.Tracker` and the audit log see them.

## 3. Tool catalogue

Default risk is shown for paths inside the workspace. Any write outside the workspace becomes **High**. Credential files are **Forbidden**, as they are today.

### A. Reading and navigating (Low risk)

| Tool | New / change | Arguments (sketch) | Notes |
|---|---|---|---|
| `fs_read` | upgrade | `path, offset_line, limit_lines` (existing) | Records `FileState`; header shows `sha` and total lines; long lines are cut at 2000 chars; its own output limit `SCOUTCLAW_MAX_READ` (default 64 KB) |
| `fs_read_many` | **new** | `files: [{path, offset_line?, limit_lines?}]` (max 20) | Reads several files or ranges in one round trip; shares one output budget |
| `fs_glob` | **new** | `pattern` (`**/*.go`), `path?`, `max_results?` | Respects `.gitignore`; sorted newest first |
| `fs_grep` | **new** (replaces `fs_search`) | `pattern, path?, glob?, type?` (`go`, `ts`…), `ignore_case?, multiline?, context?` (or `before`/`after`), `output: content\|files\|count`, `max_results?` | Uses `rg` when installed, otherwise the current Go walker plus a `.gitignore` matcher. `fs_search` stays as an alias for one release |
| `fs_list` | upgrade | add `respect_gitignore` (default true) | |
| `code_outline` | **new** (phase 3) | `path` | Lists symbols (func/type/class/method) with line ranges. Uses `go/parser` for Go, `ctags` when present, regex heuristics otherwise. Lets the model jump to a function without reading the whole file. No tree-sitter, to keep builds cgo-free |
| `project_info` | **new** (phase 2) | `path?` | Finds the language, package manager and **build/test/lint/format commands** from `go.mod`, `package.json` scripts, `Makefile`, `pyproject.toml`, `Cargo.toml` and so on. Also VCS state (branch, dirty files). Tells the agent how to verify its change |

### B. Granular text editing (Medium risk)

| Tool | New / change | Arguments (sketch) | Notes |
|---|---|---|---|
| `fs_patch` | upgrade | existing fields, **or** `edits: [{old_string, new_string, replace_all?}]` | Several string replacements in one file, applied in order, atomically, with one diff and one approval. On "not found", show the closest match (whitespace-normalised) so the model can fix its copy |
| `fs_lines` | **new** | `path, ops: [{op: insert\|replace\|delete, start, end?, expect, text?}]` | The line-level tool: **insert** after line N (`0` = top, `-1` = end, so it also covers append), **replace** lines start..end, **delete** lines start..end. Each op is guarded by `expect`. Ops are applied bottom-up so earlier line numbers stay valid. Returns the edited region with new line numbers |
| `fs_write` | keep | | For new files or full rewrites. Overwriting an existing file requires a prior read (rule 1) |
| `fs_apply_patch` | **new** (phase 2) | `patch`: a unified diff, or a Codex-style `*** Begin Patch` block with Add/Update/Delete/Move sections | Multi-file refactors in one atomic, previewed step. Hunks are matched with context (fuzz on whitespace only). Every touched path goes through policy; one approval covers all of them |
| `fs_replace_regex` | **new** (phase 2) | `pattern, replacement, path \| glob, max_files?` | Rename an identifier across files. The preview shows a per-file count and diffs. **High** risk when more than 20 files would change |

### C. File and directory management

| Tool | Arguments (sketch) | Risk | Notes |
|---|---|---|---|
| `fs_mkdir` | `path` (creates parents) | Medium | No-op if it already exists |
| `fs_copy` | `src, dest, overwrite?` | Medium | File or directory (recursive, skips `.git`). Refuses an existing `dest` unless `overwrite`; anything replaced goes to backup |
| `fs_move` | `src, dest, overwrite?` | Medium | Rename or move. Falls back to copy plus trash across devices. Moves `FileState` to the new path. Policy checks **both** paths |
| `fs_delete` | `path, recursive?` | Medium (file) / High (directory or >50 files) | Moves to the session trash, not `unlink`. **Forbidden** targets: the workspace root, `$HOME`, the filesystem root, `.git` itself, credential files |
| `fs_chmod` | `path, mode` | Medium | Phase 3; mostly for `chmod +x` on scripts |

### D. Safety net (Low risk, since these read the session journal)

| Tool | Arguments | Notes |
|---|---|---|
| `fs_changes` | `path?` | Lists every file this session created, edited, moved or deleted, with a combined diff. The agent uses it for the `finish` report and the user can see it with a new `/diff` slash command |
| `fs_undo` | `path?` or `steps?` | Restores from backup or trash using the journal. It is a mutation, so **Medium** risk with a preview. Also exposed as an `/undo` slash command |

For this, `commit` gets a **change journal** (`STATE_DIR/journal/<session>.jsonl`). Each entry records op, paths, backup or trash location, and before/after sha. `fs_changes` and `fs_undo` read it, and the audit log references it.

### E. Dev workflow (phase 3)

- **Background processes**: `proc_start` (dev server or watcher, workspace-scoped, killed on exit or Ctrl-C), `proc_output` (new output since the last read), `proc_stop`. Without these, `npm run dev` blocks the agent until the tool timeout.
- **Git**: no separate tools. `shell_exec` already runs git. Check and extend `policy/command.go` so `git status/diff/log/show` are Low, `add/commit/stash/checkout -b` are Medium, and `push`, `reset --hard`, `clean -fd`, `rebase` and force options are High.

## 4. Runtime changes that make coding work

| Area | Change |
|---|---|
| **Approval mode** | Add `SCOUTCLAW_APPROVAL=edits`: workspace file edits are approved automatically, while shell changes and anything outside the workspace still ask. Claude Code calls this "accept edits"; without it every one-line change interrupts the user |
| **Policy** | `Classify` cases for each new tool. A shared `classifyFSPaths(tool, paths...)` handles copy/move/apply_patch (risk is the highest of all paths). Session "allow" keys per tool |
| **Diff** | Replace the LCS diff (O(n·m), skipped for 4M cells, which is about 2k×2k lines) with Myers O(ND), so diffs of large source files still render |
| **Agent loop** | Run consecutive **read-only** tool calls in parallel (the loop over `resp.ToolCalls` in `agent.go` is sequential). Mutating calls stay sequential |
| **Limits** | Coding tasks need far more steps. Raise the default `MAX_STEPS` to around 80, or derive it from the task. Track context size and add **conversation compaction** later (summarise older turns), because long edit sessions will overflow the context window |
| **System prompt** | Add a CODING section: explore with `fs_glob`/`fs_grep`/`code_outline`; read before editing; prefer `fs_patch`/`fs_lines` to `fs_write`; keep diffs minimal and match the code style; don't create files you don't need; after changes run the build/tests from `project_info` with `purpose="verify"`; summarise with `fs_changes` |
| **TUI** | Coloured diff in the approval panel and in results; compact result cards such as `Edited internal/x.go (+4 −1)`; `/diff` and `/undo` commands |
| **Audit** | One entry per file operation with paths, sha before and after, and the backup location |

## 5. Config and consent: global and workspace level

Today config comes only from flags, env vars and `.env` files. An "always" approval disappears when the session ends, and a project can't ship its own rules. This section follows the layout Claude Code (`~/.claude/settings.json`, `.claude/settings.json`, `.claude/settings.local.json`), Codex (`~/.codex/config.toml`) and Gemini CLI (`~/.gemini/settings.json`, `.gemini/settings.json`) share.

### 5.1 Files

| Layer | Path | Committed? | Who writes it |
|---|---|---|---|
| Managed (later) | `/etc/scout/managed.json` · macOS `/Library/Application Support/Scout/managed.json` · Windows `C:\ProgramData\Scout\managed.json` | – | An admin; users can't override it |
| Workspace, personal | `<workspace>/.scout/config.local.json` | **No**: Scout adds it to `.gitignore` when it creates it | Scout ("always allow in this project"), or the user |
| Workspace, shared | `<workspace>/.scout/config.json` | Yes: team rules shipped with the repo | The team |
| Global (user) | `~/.scout/config.json` (Windows `%USERPROFILE%\.scout\config.json`); the folder can be moved with `SCOUT_CONFIG_DIR` | – | The user, `scout config set --global`, `/permissions` |
| Trust store | `~/.scout/trusted.json` | – | Scout, after the trust prompt (5.5) |

- **Workspace root:** `--workspace` / `SCOUTCLAW_WORKSPACE` if set; otherwise walk up from the current directory to the nearest folder with `.scout/`, then the nearest `.git`, otherwise the current directory.
- **Secrets never go in `config.json`.** The API key stays in env vars and `~/.scoutclaw.env`, and `scout config set apiKey` is refused. Sessions, audit, backups and trash stay in `STATE_DIR`; the config folder holds only config.
- **Instructions files** (related, by convention): load `AGENTS.md` (the cross-tool standard) and `SCOUT.md` from the workspace root, plus `~/.scout/SCOUT.md`, into the task context as project or personal instructions.

### 5.2 Schema (JSON, with a published JSON Schema for editor autocomplete)

```json
{
  "$schema": "https://raw.githubusercontent.com/sshuvoo/scout-claw/main/schema/config.schema.json",
  "model": "gpt-5.5",
  "approval": "edits",
  "maxSteps": 80,
  "toolTimeout": "2m",
  "taskTimeout": "30m",
  "maxOutput": 16384,
  "permissions": {
    "allow": ["fs_patch", "fs_lines", "shell_exec(go test *)", "shell_exec(git status)"],
    "ask":   ["shell_exec(git push *)", "fs_delete"],
    "deny":  ["fs_read(**/.env*)", "fs_*(migrations/**)", "net_http(domain:*.internal)"],
    "additionalDirectories": ["../shared-lib"]
  },
  "instructions": ["AGENTS.md", "docs/CONTRIBUTING.md"]
}
```

- Keys are camelCase. Unknown keys give a **warning** that names the file (so older and newer Scout versions can share configs). Invalid values give an **error** with file, key and expected value, like today's env validation.
- `baseUrl` is accepted **only** in the global file, env or flags. If a repo could set it, it could send your API key to its own server.

### 5.3 Precedence and merging

Highest first: **managed → flags → env vars → workspace personal → workspace shared → global → `.env` files → defaults.** Current behaviour (flags > env > `.env` > defaults) is kept; the new file layers slot in between.

- Plain values (`model`, `approval`, …): the highest layer that sets them wins.
- `permissions.*` lists: **combined** across all layers and de-duplicated.
- `scout config` shows each effective value with its **source** (`approval = edits  (.scout/config.local.json)`), like it does for env vars now.

### 5.4 Permission rules

Syntax: `Tool` or `Tool(specifier)`. `*` in a tool name matches several tools (`fs_*`).

| Tool family | Specifier | Example |
|---|---|---|
| `fs_*` | gitignore-style glob, relative to the workspace root (or absolute / `~/…`) | `fs_write(src/**)`, `fs_read(**/.env*)` |
| `shell_exec` | argv joined by spaces; a trailing ` *` is a prefix match on whole words | `shell_exec(npm run test *)` |
| `shell_script`, `shell_powershell` | none: whole tool only. Scripts can't be pattern-matched safely, so these can be denied or asked but **never allow-listed by pattern** | `deny: ["shell_powershell"]` |
| `net_*` | `domain:` with wildcards | `net_http(domain:api.github.com)` |

Evaluation order, enforced by the policy engine (new `internal/policy/rules.go`), before the approval mode:

1. **Built-in forbidden** (credential files, workspace root deletion, …): always denied, and no rule can lift it.
2. **`deny`** in any layer → denied.
3. **`ask`** in any layer → prompt, even in `auto`/`edits` mode.
4. **`allow`** → runs without a prompt. A Medium action is allowed. A **High** action is allowed only by a rule from the global or personal file, never the shared one.
5. No match → today's risk classification plus approval mode.

Each `Decision` records the rule and file that decided it (`allowed by shell_exec(go test *) in .scout/config.local.json`), and the audit log stores that too.

### 5.5 Workspace trust (protection against a malicious repo)

A cloned repo can commit `.scout/config.json` with `allow: ["fs_write", "shell_exec(curl *)"]` or `approval: auto`. So, as Claude Code, Codex and Gemini CLI do:

- The first time Scout starts in a workspace whose **shared** config loosens anything (allow rules, `approval: auto/edits`, `additionalDirectories`, `instructions`), it shows a trust prompt that lists exactly what would be loosened.
- Trust is stored in `~/.scout/trusted.json` with the workspace path **and a hash of the shared config**. If the file changes, Scout asks again.
- **Untrusted workspace:** tightening settings (`deny`, `ask`, `approval: readonly`, lower limits) still apply. Loosening settings are ignored, and the status bar says so.
- The personal and global files are always trusted, because only the user writes them.
- `scout run` in CI or a non-terminal can't show a prompt, so it treats the workspace as untrusted unless `--trust-workspace` is passed.

### 5.6 Remembering consent from the approval panel

Today the panel offers yes / always for this session (Medium only) / no. It becomes:

```
[y] once   [s] this session   [p] always in this project   [n] no
            would save: shell_exec(go test *)  →  .scout/config.local.json
```

- `p` writes the **suggested rule**, shown before saving, to the personal workspace file. For `shell_exec` the suggestion is the command plus `*` (`go test ./... -run X` → `go test *`); for `fs_*` it is the tool name.
- High-risk actions get only `y`/`n`. To always allow them, the user edits the config file on purpose.
- Writes are atomic and keep the rest of the file. The file is created with `$schema`, and `config.local.json` is added to `.gitignore`.

### 5.7 Commands

| CLI | Chat | What it does |
|---|---|---|
| `scout init` | `/init` | Creates `.scout/config.json` (with `$schema`), offers to create `AGENTS.md`, and adds `config.local.json` to `.gitignore` |
| `scout config` (existing) | `/config` | Effective config with the source of each value |
| `scout config get\|set\|unset <key> [value] [--global\|--project\|--local]` | – | Edits one layer (`--local` by default) |
| `scout permissions [list\|add\|remove] [--global\|--project\|--local]` | `/permissions` | Shows every rule with its file; adds or removes rules |
| `scout trust [--revoke]` | `/trust` | Trusts or untrusts the current workspace |

Config is re-read at the start of every chat task, so edits apply without a restart.

### 5.8 Code layout

- `internal/config`: `files.go` (finds, parses and merges the layers, and tracks the source of each value), `write.go` (atomic, layer-targeted updates), `trust.go`. `Load()` keeps its signature, so callers don't change.
- `internal/policy/rules.go`: parses and matches rules. `Engine` gets `Rules` and `Trusted`, and `Decide` applies 5.4 before the approval mode.
- `internal/executor`: replace the in-memory-only `allowed` map with "session rules plus persisted rules"; the `p` answer calls `config.AddRule(layer, rule)`.
- `internal/cli`: the trust prompt, the 4-option approval panel, the `init`/`config`/`permissions`/`trust` commands.
- `schema/config.schema.json`, plus a test that checks it against the Go struct.
- Tests: merge order, list combining, deny beats allow, untrusted repo can't loosen, `baseUrl` refused from a workspace file, Windows paths in globs, a changed config hash asks for trust again.

## 6. Phases

**Phase 1: core editing** (biggest gain, lowest risk) — **implemented**: single `fs_lines` tool with an `ops` array, and the `edits` approval mode, as recommended in section 7
1. `FileState` tracker plus read-before-edit enforcement in `fs_write`/`fs_patch`
2. Preserving line endings, BOM and trailing newline in `commit`
3. `fs_read` upgrade (sha header, own output limit, long-line cut)
4. `fs_patch` with `edits[]`, and close-match hints
5. `fs_lines` (insert/replace/delete, guarded)
6. `fs_mkdir`, `fs_copy`, `fs_move`, `fs_delete` (to trash)
7. Change journal plus `fs_changes`, `fs_undo`, and `/diff` and `/undo`
8. Policy cases, the `edits` approval mode, CODING section in the prompt, tests (including Windows paths and CRLF)

**Phase 1.5: config and consent** (section 5; needed once edits are frequent)
1. Layered config files, merging and source tracking, `scout config` with sources
2. Permission rules in the policy engine, plus the audit record of the deciding rule
3. Workspace trust prompt and trust store
4. 4-option approval panel that saves to `config.local.json`
5. `scout init`, `config get/set/unset`, `permissions`, `trust`; slash commands; JSON Schema
6. `AGENTS.md` / `SCOUT.md` loaded into the task context

**Phase 2: scale and multi-file**
`fs_glob`, `fs_grep` (with `rg`), `fs_read_many`, `fs_apply_patch`, `fs_replace_regex`, `project_info`, Myers diff, parallel read-only calls, higher step limit.

**Phase 3: IDE-level**
`code_outline`, background processes, git policy refinement, conversation compaction, `fs_chmod`.

## 7. Open decisions

1. **Line tool shape**: one `fs_lines` tool with an `ops` array (recommended: fewer tools for the model to choose from, and atomic multi-op), or separate `fs_insert_lines` / `fs_replace_lines` / `fs_delete_lines`.
2. **ripgrep**: use it when installed and fall back to Go (recommended), or Go only.
3. **`fs_search`**: keep it as an alias of `fs_grep` for one release (recommended), or rename it right away.
4. **`edits` approval mode**: add it (recommended), or keep `ask`/`auto`/`readonly` only.
5. **Global config folder**: `~/.scout/` (recommended: matches your hint and `~/.claude`, `~/.codex`, `~/.gemini`), or XDG `~/.config/scout/`.
6. **File format**: plain JSON with a JSON Schema (recommended, as Claude Code does), JSONC (comments allowed, as Gemini CLI does), or TOML (as Codex does).
7. **"Always" scope from the panel**: project only (recommended), or also an "all projects" option that writes to the global file.
