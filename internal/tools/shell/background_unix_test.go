//go:build !windows

package shell

import (
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// On Windows the Job Object tree kill is covered by TestTimeoutKillsJobTree.
func TestProcStopKillsTheWholeTree(t *testing.T) {
	_, m := newBackground(t)
	r, err := bgCall(t, m["proc_start"], with(helperArgs("spawn"), "wait_for", `child \d+`, "wait_sec", 20))
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	for _, f := range strings.Fields(r.Output) {
		if n, err := strconv.Atoi(f); err == nil && strings.Contains(r.Output, "child "+f) {
			pid = n
		}
	}
	if pid == 0 {
		t.Fatalf("no child pid in %q", r.Output)
	}
	if _, err := bgCall(t, m["proc_stop"], map[string]any{"id": r.Metadata["id"]}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("grandchild %d survived proc_stop", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
