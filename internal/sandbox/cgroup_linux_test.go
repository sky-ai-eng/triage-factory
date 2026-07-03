//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCgroupOOMKilled_Parsing pins the memory.events reader against
// fixture content — the attribution path (agentproc's "exceeded its
// memory limit" error) rides on this returning true exactly when
// oom_kill is nonzero.
func TestCgroupOOMKilled_Parsing(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"oom kill recorded", "low 0\nhigh 0\nmax 39\noom 1\noom_kill 1\noom_group_kill 0\n", true},
		{"pressure but no kill", "low 0\nhigh 12\nmax 39\noom 0\noom_kill 0\noom_group_kill 0\n", false},
		{"multiple kills", "oom_kill 3\n", true},
		{"empty file", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "memory.events"), []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := cgroupOOMKilled(dir); got != tt.want {
				t.Errorf("cgroupOOMKilled = %v, want %v for %q", got, tt.want, tt.content)
			}
		})
	}
}

// TestCgroupOOMKilled_MissingState covers the fail-safe reads: no dir
// configured (no limit) and a dir whose group is already gone.
func TestCgroupOOMKilled_MissingState(t *testing.T) {
	if cgroupOOMKilled("") {
		t.Error("empty dir (no limit configured) must read as not-OOM-killed")
	}
	if cgroupOOMKilled(filepath.Join(t.TempDir(), "gone")) {
		t.Error("missing cgroup dir must read as not-OOM-killed")
	}
}
