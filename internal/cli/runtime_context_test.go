package cli

import (
	"strings"
	"testing"

	"scoutclaw/internal/session"
)

func TestRecentTaskContextKeepsOnlyThreeBoundedNotes(t *testing.T) {
	tasks := []session.TaskRecord{
		{Prompt: "old task", Summary: "old result"},
		{Prompt: "task two", Summary: "result two"},
		{Prompt: "task three", Summary: "result three", Changes: []string{"file A", "file B", "file C"}},
		{Prompt: "last task", Summary: strings.Repeat("x", 500)},
	}
	got := recentTaskContext(tasks)
	if strings.Contains(got, "old task") || !strings.Contains(got, "task two") || !strings.Contains(got, "last task") {
		t.Fatalf("recent task window is wrong: %s", got)
	}
	if strings.Contains(got, "file C") || len([]rune(got)) > 1200 {
		t.Fatalf("history was not bounded: %d runes", len([]rune(got)))
	}
}
