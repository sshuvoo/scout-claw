package builtin

import (
	"testing"

	"scoutclaw/internal/policy"
	"scoutclaw/internal/tools/filesystem"
	"scoutclaw/internal/verify"
)

func TestFSOptions(t *testing.T) {
	st := filesystem.NewState("")
	tests := []struct {
		name     string
		opts     Options
		wantRead int
	}{
		{"read limit defaults to the output limit", Options{MaxOutput: 4096}, 4096},
		{"explicit read limit wins", Options{MaxOutput: 4096, MaxRead: 65536}, 65536},
	}
	for _, tt := range tests {
		if got := FS(tt.opts).MaxReadLen; got != tt.wantRead {
			t.Errorf("%s: MaxReadLen = %d, want %d", tt.name, got, tt.wantRead)
		}
	}
	if FS(Options{Files: st, TrashDir: "/t"}).State != st || FS(Options{TrashDir: "/t"}).TrashDir != "/t" {
		t.Error("state and trash dir must be passed through")
	}
}

func TestRegistryHasEveryFileTool(t *testing.T) {
	reg, _ := New(Options{Scope: policy.NewScope(t.TempDir()), MaxOutput: 4096, Tracker: &verify.Tracker{}})
	for _, name := range []string{"fs_list", "fs_read", "fs_stat", "fs_search", "fs_write", "fs_patch", "fs_lines",
		"fs_mkdir", "fs_copy", "fs_move", "fs_delete", "fs_changes", "fs_undo", "fs_glob", "fs_grep", "fs_read_many", "fs_apply_patch", "project_info"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("%s is not registered", name)
		}
	}
}
