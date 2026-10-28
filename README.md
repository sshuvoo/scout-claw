# ScoutClaw 🦀⚡

[![Go Version](https://img.shields.io/badge/Go-1.22+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![PRs Welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg)](https://makeapullrequest.com)

> **ScoutClaw** is an autonomous, on-device diagnostic and system task-execution CLI agent powered by OpenAI's structured reasoning loop. It inspects, troubleshoots, and solves OS and platform problems directly from your terminal.

---

## ⚡ Overview

ScoutClaw operates on an iterative **Inspect ➔ Reason ➔ Execute ➔ Solve** loop:

1. **Prompt & Context Injection**: Collects the user's issue alongside host environment metadata (OS, architecture, hostname, working directory).
2. **Autonomous Execution**: The AI analyzes the situation and returns targeted terminal commands for the target platform (Linux, macOS, Windows).
3. **Continuous Feedback**: Executes commands locally, captures `stdout`, `stderr`, and exit codes, and pipes them back to the model for iterative diagnosis.
4. **Conclusion**: When the task is complete, ScoutClaw signals `DONE` and presents the final explanation and solution in clean, high-contrast formatting.

---

## ✨ Features

- 🧠 **Autonomous Decision Loop**: Continuously plans, issues command batches, checks exit codes, and adjusts strategy.
- 💬 **Interactive REPL Mode**: Stays alive across queries with context continuity and helpful rotating tips.
- ⚡ **Direct Argument Mode**: Run one-off queries instantly with `scoutclaw "<task>"`.
- 🔐 **Flexible Configuration**: Reads API keys from `.env` in the current directory, `~/.scoutclaw.env`, `~/.scout.env`, or system environment variables.
- 🎨 **Minimal & Modern Terminal UI**:
  - Soft muted gray for background model reasoning/thoughts.
  - Native-style shell execution logs (`$ command`).
  - High-visibility **bold green** resolution output.
  - Built-in session controls: `reset`, `clear`, and `exit`.

---

## 🚀 Installation

### Prerequisites

- [Go](https://go.dev/dl/) 1.22 or higher installed.
- An [OpenAI API Key](https://platform.openai.com/api-keys).

### From Source

```bash
# Clone the repository
git clone https://github.com/sshuvoo/scout-claw.git
cd scout-claw

# Build and install to your Go bin directory (~/go/bin or $GOBIN)
go install .
```

Or build the standalone binary directly:

```bash
go build -o scoutclaw main.go
sudo mv scoutclaw /usr/local/bin/   # or ~/.local/bin/
```

---

## ⚙️ Configuration

Set your OpenAI API key using any of the following methods:

### Option 1: Local `.env` file (Current Directory)
```bash
cp .env.example .env
# Edit .env and insert your API key:
# OPENAI_API_KEY="sk-..."
```

### Option 2: Global Configuration (Recommended)
Place it in your home directory so `scoutclaw` can be run anywhere:
```bash
echo 'OPENAI_API_KEY="sk-..."' > ~/.scoutclaw.env
```

### Option 3: Shell Environment Variable
```bash
export OPENAI_API_KEY="sk-..."
```

---

## 📖 Usage

### 1. Interactive REPL Mode (Recommended)

Simply launch `scoutclaw` without arguments:

```bash
scoutclaw
```

```text
  ____                  _    ____ _                 
 / ___|  ___ ___  _   _| |_ / ___| | __ ___      __ 
 \___ \ / __/ _ \| | | | __| |   | |/ _` \ \ /\ / / 
  ___) | (_| (_) | |_| | |_| |___| | (_| |\ V  V /  
 |____/ \___\___/ \__,_|\__|\____|_|\__,_| \_/\_/   
   Autonomous On-Device ScoutClaw Diagnostic Agent
   Commands: Type your task | 'reset' to clear context | 'exit' to quit

┌──────────────────────────────────────────────────────────────────┐
│  🤖 SCOUTCLAW AGENT READY                                         │
│  Type your problem or request below to get started:              │
└──────────────────────────────────────────────────────────────────┘
❯ check why port 8080 is blocked and list the process using it
```

#### In-Session Commands:
- `reset`: Clear current session context and memory to start a new task.
- `clear`: Clear the terminal screen and reprint the banner.
- `exit` or `quit`: Exit the REPL.

---

### 2. Direct CLI Argument Mode

Pass your query directly for single-task executions:

```bash
# Inspect process memory usage
scoutclaw "find which processes are consuming more than 1GB of RAM"

# Check disk space bottlenecks
scoutclaw "check what directories in /var/log are taking up the most space"

# Network diagnostics
scoutclaw "check my active network listeners and DNS resolution health"
```

---

## 🛡️ Safety & Guardrails

ScoutClaw incorporates safeguards within its system instructions:
- **Destructive Command Prevention**: Instructed not to run reckless destructive commands (e.g. indiscriminate file system formatting or unverified `rm -rf /`).
- **Output Truncation**: Safeguards against token explosion by truncating command outputs exceeding safe size limits.
- **Iteration Limits**: Default 25-step execution cap per task to avoid runaway loops.

---

## 🛠️ Architecture

```
User Prompt (CLI)
       │
       ▼
┌─────────────────────────┐
│ ScoutClaw Host Engine   │ ◄── Read OS & System Context
└──────────┬──────────────┘
           │
           │ (JSON Schema / Structured Response)
           ▼
┌─────────────────────────┐
│ OpenAI Responses API    │
└──────────┬──────────────┘
           │
     Returns ScoutAction:
     - thought: reasoning
     - status: CONTINUE | DONE
     - commands: ["cmd1", "cmd2"]
           │
           ▼
┌─────────────────────────┐
│ Local Shell Execution   │
│ (sh -c / cmd.exe)       │
└──────────┬──────────────┘
           │
      Captures:
      - stdout / stderr
      - exit code
      - duration
           │
           ▼
Iterates until Status == DONE ➔ Prints Bold Green Resolution
```

---

## 🤝 Contributing

Contributions, issues, and feature requests are welcome!

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'feat: add amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request

---

## 📄 License

This project is licensed under the [MIT License](LICENSE).
