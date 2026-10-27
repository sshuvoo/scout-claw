package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

// ANSI color and style constants
const (
	colorReset   = "\033[0m"
	colorBold    = "\033[1m"
	colorDim     = "\033[2m"
	colorCyan    = "\033[36m"
	colorGreen   = "\033[32m"
	colorYellow  = "\033[33m"
	colorRed     = "\033[31m"
	colorMagenta = "\033[35m"
	colorBlue    = "\033[34m"
	colorGray    = "\033[90m"
)

// ScoutAction matches the structured JSON returned by the server
type ScoutAction struct {
	Thought      string   `json:"thought"`
	Status       string   `json:"status"` // "CONTINUE" or "DONE"
	Commands     []string `json:"commands"`
	FinalMessage string   `json:"final_message"`
}

const systemInstruction = `You are "ScoutClaw", an autonomous system diagnostic and problem-solving agent running locally via a CLI. You are a parasite of the system with ghama ray capability who can rule the system/platform/os like no one can stop a ghama ray.
Your goal is to investigate, troubleshoot, and fix issues on the user's computer/server by running terminal commands iteratively.

Operational Rules:
1. Examine the user's issue and initial environment info.
2. Formulate a hypothesis and return specific terminal/cmd commands to execute.
3. Keep commands tailored to the client's OS (linux, darwin, or windows).
4. After each command batch executes, the client returns stdout, stderr, and the exit code.
5. If you need more investigation or actions, set status="CONTINUE" and provide the next "commands".
6. When the problem is diagnosed, fixed, or answered, set status="DONE", commands=[], and provide the complete resolution or answer in "final_message".
7. Be cautious: do not run destructive commands (like formatting disks or indiscriminate recursive deletion) without necessity.
8. When inspecting files, prefer concise tools (head, tail, grep) instead of printing huge logs.`

// Global JSON schema for structured responses
var scoutSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"thought": map[string]any{
			"type":        "string",
			"description": "ScoutClaw's internal reasoning and current plan",
		},
		"status": map[string]any{
			"type":        "string",
			"enum":        []string{"CONTINUE", "DONE"},
			"description": "CONTINUE if more commands need to run; DONE when finished",
		},
		"commands": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "string",
			},
			"description": "List of shell commands to execute on the client",
		},
		"final_message": map[string]any{
			"type":        "string",
			"description": "Final user-facing report and explanation when status is DONE",
		},
	},
	"required":             []string{"thought", "status", "commands", "final_message"},
	"additionalProperties": false,
}

func main() {
	loadDotEnv()

	// If user passed arguments directly (e.g. scoutclaw "my problem"), run once
	if len(os.Args) > 1 {
		userPrompt := strings.Join(os.Args[1:], " ")
		runScoutTask(userPrompt, "")
		return
	}

	// Interactive CLI Session (keeps prompt box alive)
	printBanner()

	reader := bufio.NewReader(os.Stdin)
	var lastResponseID string
	promptCount := 0

	for {
		prompt := showPromptBox(reader, promptCount)
		if prompt == "" {
			continue
		}

		// Exit commands
		lower := strings.ToLower(prompt)
		if lower == "exit" || lower == "quit" || lower == "q" {
			fmt.Printf("%s👋 Bye from ScoutClaw!%s\n", colorCyan, colorReset)
			return
		}

		if lower == "clear" {
			fmt.Print("\033[H\033[2J")
			printBanner()
			continue
		}

		if lower == "reset" {
			lastResponseID = ""
			fmt.Printf("%s🔄 Context cleared. Starting fresh task.%s\n\n", colorYellow, colorReset)
			promptCount = 0
			continue
		}

		lastResponseID = runScoutTask(prompt, lastResponseID)
		promptCount++
	}
}

// runScoutTask runs the diagnostic agent loop for a prompt and returns the last response ID
func runScoutTask(userPrompt, previousID string) string {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		fmt.Printf("\n%s❌ Error: OPENAI_API_KEY environment variable is not set.%s\n", colorRed, colorReset)
		fmt.Printf("%s💡 Fix:%s\n", colorYellow, colorReset)
		fmt.Printf("   1. Export it: %sexport OPENAI_API_KEY=\"sk-...\"%s\n", colorCyan, colorReset)
		fmt.Printf("   2. Or put it in %s~/.scoutclaw.env%s, %s~/.scout.env%s, or current dir %s.env%s\n\n", colorCyan, colorReset, colorCyan, colorReset, colorCyan, colorReset)
		return previousID
	}

	client := openai.NewClient()
	ctx := context.Background()

	hostname, _ := os.Hostname()
	currentDir, _ := os.Getwd()

	initialContext := fmt.Sprintf(
		"Client Host Info:\n- OS: %s\n- Architecture: %s\n- Hostname: %s\n- Current Working Directory: %s\n\nUser Request: %s",
		runtime.GOOS,
		runtime.GOARCH,
		hostname,
		currentDir,
		userPrompt,
	)

	var previousResponseID = previousID
	nextInput := initialContext
	step := 1
	maxSteps := 25

	for step <= maxSteps {
		params := responses.ResponseNewParams{
			Model:        openai.ChatModelGPT5_5,
			Instructions: openai.String(systemInstruction),
			Input: responses.ResponseNewParamsInputUnion{
				OfString: openai.String(nextInput),
			},
			Text: responses.ResponseTextConfigParam{
				Format: responses.ResponseFormatTextConfigParamOfJSONSchema("scout_action", scoutSchema),
			},
		}

		if previousResponseID != "" {
			params.PreviousResponseID = openai.String(previousResponseID)
		}

		resp, err := client.Responses.New(ctx, params)
		if err != nil {
			fmt.Printf("%s❌ Error: %v%s\n", colorRed, err, colorReset)
			return previousResponseID
		}

		previousResponseID = resp.ID
		rawOutput := resp.OutputText()

		var action ScoutAction
		if err := json.Unmarshal([]byte(rawOutput), &action); err != nil {
			fmt.Printf("%s❌ Parse error: %v%s\n", colorRed, err, colorReset)
			return previousResponseID
		}

		if action.Thought != "" {
			fmt.Printf("%s\033[38;5;244m%s%s\n\n", colorDim, action.Thought, colorReset)
		}

		if strings.ToUpper(action.Status) == "DONE" {
			if action.FinalMessage != "" {
				fmt.Printf("%s%s%s%s\n", colorBold, colorGreen, action.FinalMessage, colorReset)
			}
			fmt.Println()
			return previousResponseID
		}

		if len(action.Commands) == 0 {
			return previousResponseID
		}

		var executionResults []string

		for _, cmdStr := range action.Commands {
			cmdStr = strings.TrimSpace(cmdStr)
			fmt.Printf("%s$ %s%s\n", colorBold, cmdStr, colorReset)

			output, exitCode, duration := runShellCommand(cmdStr)

			if len(output) > 0 {
				preview := output
				if len(preview) > 500 {
					preview = preview[:500] + "\n... [truncated]"
				}
				fmt.Println(indent(preview, "  "))
			}

			// Format for the server
			resultMsg := fmt.Sprintf(
				"Command: %s\nExit Code: %d\nExecution Time: %s\nOutput:\n%s\n",
				cmdStr,
				exitCode,
				duration.Round(time.Millisecond),
				truncate(output, 4000),
			)
			executionResults = append(executionResults, resultMsg)
		}

		nextInput = fmt.Sprintf(
			"Results of step %d:\n\n%s\nPlease analyze the results and provide the next step.",
			step,
			strings.Join(executionResults, "\n---\n"),
		)

		fmt.Println()
		step++
	}

	fmt.Printf("%s⚠️ Reached maximum step limit (%d).%s\n", colorYellow, maxSteps, colorReset)
	return previousResponseID
}

func printBanner() {
	banner := `
  ____                  _    ____ _                 
 / ___|  ___ ___  _   _| |_ / ___| | __ ___      __ 
 \___ \ / __/ _ \| | | | __| |   | |/ _` + "`" + ` \ \ /\ / / 
  ___) | (_| (_) | |_| | |_| |___| | (_| |\ V  V /  
 |____/ \___\___/ \__,_|\__|\____|_|\__,_| \_/\_/   `
	fmt.Printf("%s%s%s\n", colorBold, colorCyan, banner)
	fmt.Printf("   %sAutonomous On-Device ScoutClaw Diagnostic Agent%s\n", colorDim, colorReset)
	fmt.Printf("   %sCommands: Type your task | 'reset' to clear context | 'exit' to quit%s\n\n", colorGray, colorReset)
}

var scoutTips = []string{
	"Ask ScoutClaw to inspect logs, disk usage, memory, or network ports.",
	"Type 'reset' anytime to wipe conversation history and start fresh.",
	"You can ask ScoutClaw to fix an issue, install dependencies, or restart services.",
	"Type 'clear' to clean up your terminal screen.",
	"Try asking: 'Which process is using the most CPU right now?'",
	"ScoutClaw can safely analyze git repos, environment variables, or config files.",
	"Type 'exit' or 'quit' whenever you're done.",
}

func showPromptBox(reader *bufio.Reader, promptCount int) string {
	if promptCount == 0 {
		boxWidth := 68
		line := strings.Repeat("─", boxWidth-2)
		fmt.Printf("%s┌%s┐%s\n", colorCyan, line, colorReset)
		fmt.Printf("%s│%s  %-64s%s│%s\n", colorCyan, colorBold, "🤖 SCOUTCLAW AGENT READY", colorCyan, colorReset)
		fmt.Printf("%s│%s  %-64s%s│%s\n", colorCyan, colorDim, "Type your problem or request below to get started:", colorCyan, colorReset)
		fmt.Printf("%s└%s┘%s\n", colorCyan, line, colorReset)
	} else {
		tip := scoutTips[(promptCount-1)%len(scoutTips)]
		fmt.Printf("%s💡 Tip: %s%s\n", colorDim, tip, colorReset)
	}

	fmt.Printf("%s❯%s ", colorGreen, colorReset)

	input, err := reader.ReadString('\n')
	if err != nil {
		fmt.Printf("\n%sGoodbye!%s\n", colorCyan, colorReset)
		os.Exit(0)
	}

	trimmed := strings.TrimSpace(input)
	fmt.Println()
	return trimmed
}


func runShellCommand(command string) (string, int, time.Duration) {
	start := time.Now()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd.exe", "/C", command)
	} else {
		cmd = exec.Command("sh", "-c", command)
	}

	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined

	err := cmd.Run()
	duration := time.Since(start)

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
			combined.WriteString(fmt.Sprintf("\nCommand execution error: %v", err))
		}
	}

	return combined.String(), exitCode, duration
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "\n... [Remaining output truncated for length] ..."
}

func indent(s, prefix string) string {
	var sb strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(s))
	for scanner.Scan() {
		sb.WriteString(prefix)
		sb.WriteString(scanner.Text())
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// loadDotEnv checks .env in current directory and ~/.scoutclaw.env / ~/.scout.env in user home
func loadDotEnv() {
	parseEnvFile(".env")

	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		parseEnvFile(filepath.Join(u.HomeDir, ".scoutclaw.env"))
		parseEnvFile(filepath.Join(u.HomeDir, ".scout.env"))
		parseEnvFile(filepath.Join(u.HomeDir, ".env"))
	}
}

func parseEnvFile(path string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			val = strings.Trim(val, `"'`)
			if os.Getenv(key) == "" {
				_ = os.Setenv(key, val)
			}
		}
	}
}
