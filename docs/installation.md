# Installing ScoutClaw

ScoutClaw installs as one self-contained binary named **`scout`**. After installation you can run `scout` from any directory.

Supported platform: **Linux** (tested on Ubuntu). macOS and Windows binaries compile but are untested, and the built-in system diagnostics are Linux-only.

> **Upgrading from the first version?** The original single-file build was also installed as `scout` / `scoutclaw`. Installing the new version to the same directory overwrites `scout`; remove a stale `scoutclaw` binary (`rm ~/.local/bin/scoutclaw`) so you don't run the old one by accident. Check with `command -v scout` and `scout version`.

## 1. Install the command

Pick one method.

### A. From source with Make (recommended today)

Requires Go 1.27.1+ (see `go.mod`) and `make`.

```bash
git clone https://github.com/sshuvoo/scout-claw.git
cd scout-claw
make install
```

This builds `bin/scout` and copies it to `~/.local/bin/scout`. For a system-wide install:

```bash
sudo make install PREFIX=/usr/local      # installs /usr/local/bin/scout
```

### B. From source with Go only (no Make)

```bash
git clone https://github.com/sshuvoo/scout-claw.git
cd scout-claw
go install ./cmd/scout                   # installs to $(go env GOPATH)/bin, usually ~/go/bin/scout
```

Or choose the location yourself:

```bash
go build -o ~/.local/bin/scout ./cmd/scout
```

### C. From a GitHub release (no Go needed)

Works once a release has been published (a `vX.Y.Z` tag built with GoReleaser). Linux and macOS, amd64 and arm64:

```bash
curl -fsSL https://raw.githubusercontent.com/sshuvoo/scout-claw/main/install.sh | sh
```

The script downloads the archive for your OS and architecture, verifies its SHA-256 checksum against `checksums.txt`, and installs `scout` to `~/.local/bin`. Options (environment variables):

| Variable | Default | Meaning |
|---|---|---|
| `SCOUT_VERSION` | latest release | Install a specific tag, e.g. `v0.1.0` |
| `SCOUT_INSTALL_DIR` | `~/.local/bin` | Target directory |

Prefer to inspect first? Download `install.sh`, read it, then run `sh install.sh`.

## 2. Make sure the command is on your PATH

```bash
scout version
```

If you get `command not found`, add the install directory to your PATH (use `~/go/bin` for method B):

```bash
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.bashrc
source ~/.bashrc
```

(zsh users: `~/.zshrc`.) Open a new terminal if it still isn't found, and check with `command -v scout`.

## 3. Set your API key

A global command runs from many directories, so do not rely on a project-local `.env`. Store the key in a per-user file ScoutClaw already reads:

```bash
echo 'OPENAI_API_KEY=your_openai_api_key_here' > ~/.scoutclaw.env
chmod 600 ~/.scoutclaw.env
```

Alternatives: `export OPENAI_API_KEY=...` in your shell profile, or `~/.scout.env`, `~/.env`, or `./.env`. Real environment variables win over files. The key is never printed or logged. All other settings are optional; see `.env.example` and the configuration table in the README.

## 4. Verify the installation

```bash
scout doctor            # config, state directory, DNS/TCP to the API host
scout doctor --online   # also sends one tiny request to prove the key and model work
scout run --approval readonly "How much disk space and memory is free?"
```

A healthy `doctor` ends with `all required checks passed`.

## First tasks

```bash
scout                                            # interactive chat
scout run "Why is this machine slow?"            # one-shot task
scout run --workspace ~/myapp "The tests fail, find the cause and fix it"
```

By default ScoutClaw asks before anything beyond read-only inspection and can only modify files in the workspace (the current directory unless `--workspace` is given). Start with `--approval readonly` if you want it to look but never touch.

## Updating

- Source install: `git pull`, then `make install` (or `go install ./cmd/scout`).
- Release install: re-run the install script (optionally with `SCOUT_VERSION`).

## Uninstalling

```bash
make uninstall                    # or: rm ~/.local/bin/scout
```

Optional cleanup of data ScoutClaw created (sessions, audit log, file backups, stored key):

```bash
rm -rf ~/.local/state/scoutclaw
rm -f ~/.scoutclaw.env
```

## Troubleshooting

| Symptom | Fix |
|---|---|
| `scout: command not found` | The install directory is not on `PATH` (step 2). |
| `scout` shows an old banner or ignores `run`/`doctor` | An old build is first on `PATH`; see the upgrade note at the top and `command -v scout`. |
| `OPENAI_API_KEY is not set` | Create `~/.scoutclaw.env` (step 3) or export the variable. |
| `authentication failed (HTTP 401)` | The key is wrong or lacks access to the model; check with `scout doctor --online`. |
| HTTP 400 / model not found | Choose another model: `SCOUTCLAW_MODEL=...` or `--model`. |
| Config errors such as `invalid approval policy` | Run `scout config` to see effective values; valid approval modes are `ask`, `auto`, `readonly`. |
| `cannot open audit log` | The state directory is not writable; set `SCOUTCLAW_STATE_DIR` to a writable path. |
| Approvals are always denied in scripts or CI | No terminal is attached. Use `--approval readonly`, or `--approval auto` for medium-risk actions; high-risk actions always need a person. |
| `make: command not found` | Use method B. |
