# Architecture notes

## Dependency direction

```
cli → agent → executor → (policy, tools registry, audit, redact, verify)
        ↓          ↓
       llm      tools/{shell,filesystem,system,network,task}
```

`agent` depends on `executor` and `llm` only; it has no access to `os/exec`. `policy` knows nothing about tool
implementations (it classifies from tool name + raw JSON arguments), so the tools can be tested without it and
vice-versa. Tool packages import `policy` only for `Scope` (path resolution).

## Decisions

- **Provider conversation state.** The original implementation used the Responses API with `previous_response_id`.
  That is kept: the provider holds history, the client sends only new input (user text and tool outputs). The
  `session` file stores the last response id and any tool outputs the provider still expects, which is what makes
  `--resume` and Ctrl-C-then-continue consistent (an unanswered function call would otherwise make the next request invalid).
- **Native tool calling** (function tools) replaces the old `{status, commands[]}` JSON protocol. Arguments are decoded
  strictly (unknown fields rejected) and validated before policy sees them. Malformed or unknown calls become error
  observations; the model can recover.
- **No shell by default.** `shell_exec` takes an executable and an argument array, which removes injection through
  quoting. `shell_script` (bash) and, on Windows, `shell_powershell` exist for genuine shell syntax and are always
  high risk. Like other coding agents, ScoutClaw uses the real shell rather than an embedded interpreter: system bash on
  Linux/macOS, Git Bash on Windows (never `System32\bash.exe`, the WSL launcher), and PowerShell 7 before 5.1.
- **Bash scripts are parsed, not pattern-matched.** `mvdan.cc/sh` parses each `shell_script` (and every `bash -c`
  argument) into an AST; every command, including ones in pipelines, functions and `$(...)`, goes through the same
  argv classifier as `shell_exec`, after quote removal (`r''m` is `rm`). Redirection targets are checked for credential
  files. PowerShell has no Go parser, so its scripts are checked with patterns only.
- **Process trees.** Timeouts and Ctrl-C kill the whole tree: a process group on Unix, a Job Object on Windows (which
  also catches grandchildren whose parent already exited).
- **Policy is heuristic but fail-safe.** Unknown commands are medium, unknown tools are refused, forbidden operations
  cannot be approved. Wrappers (`env`, `timeout`, `nice`, …) are unwrapped and the inner command is classified; `sudo`
  is high and its inner command is checked for forbidden operations.
- **Verification is a runtime fact.** `finish` is an ordinary tool, but its status is computed from evidence in
  `verify.Tracker` (changes vs. `purpose=verify` commands that ran afterwards). A model cannot talk its way to "verified".
- **Audit before trust.** If the audit log cannot be opened, the runtime refuses to start rather than act unrecorded.
- **Persistence.** Sessions are JSON files (atomic writes, 0600). SQLite was not introduced: nothing needs queries yet and
  avoiding cgo keeps cross-compilation simple. The `session` package is the seam to replace.

## Not done yet (see README for limitations)

MCP client (should register tools into the same registry so policy applies), user-approved persistent memory, context
summarisation beyond provider-side truncation, streaming output, TUI, macOS/Windows diagnostics.
