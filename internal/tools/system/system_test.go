package system

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	"scoutclaw/internal/tools"
)

func tool(name string) tools.Tool {
	for _, t := range Tools() {
		if t.Name() == name {
			return t
		}
	}
	return nil
}

func TestEnvListMasksSecrets(t *testing.T) {
	t.Setenv("MY_APP_PASSWORD", "hunter2hunter2")
	t.Setenv("MY_APP_MODE", "production")
	r, err := tool("env_list").Execute(context.Background(), json.RawMessage(`{"prefix":"MY_APP_"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.Output, "hunter2") || !strings.Contains(r.Output, "MY_APP_PASSWORD=[REDACTED]") || !strings.Contains(r.Output, "MY_APP_MODE=production") {
		t.Fatalf("%q", r.Output)
	}
}

func TestSysInfoAndProcessesOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	r, err := tool("sys_info").Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil || !strings.Contains(r.Output, "memory:") || !strings.Contains(r.Output, "disks:") || !strings.Contains(r.Output, "load average") {
		t.Fatalf("%v %q", err, r.Output)
	}
	r, err = tool("process_list").Execute(context.Background(), json.RawMessage(`{"sort_by":"mem","limit":5}`))
	if err != nil || !strings.Contains(r.Output, "PID") || strings.Count(r.Output, "\n") < 3 {
		t.Fatalf("%v %q", err, r.Output)
	}
	if tool("process_list").Validate(json.RawMessage(`{"sort_by":"name"}`)) == nil || tool("process_list").Validate(json.RawMessage(`{"limit":500}`)) == nil {
		t.Fatal("validation")
	}
}

func TestRuntimesReportsMissingAndPresent(t *testing.T) {
	out := DetectRuntimes(context.Background())
	if !strings.Contains(out, "go ") && !strings.Contains(out, "go\t") && !strings.Contains(out, "not installed") {
		t.Fatalf("%q", out)
	}
}
