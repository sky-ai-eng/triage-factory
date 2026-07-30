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

// TestCgroupPeakMemMB_Parsing pins the byte→MiB conversion, including the
// round-up that keeps a sub-MiB peak from recording as 0 (which would be
// indistinguishable from "never measured") and the refusal to report a
// nonsense value.
func TestCgroupPeakMemMB_Parsing(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    *int
	}{
		{"exact MiB", "4194304\n", ptr(4)},
		{"rounds up", "4194305\n", ptr(5)},
		{"sub-MiB peaks at 1, never 0", "512000\n", ptr(1)},
		{"zero is not a peak", "0\n", nil},
		{"garbage", "not-a-number\n", nil},
		{"empty", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "memory.peak"), []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			got := cgroupPeakMemMB(dir)
			if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
				t.Errorf("cgroupPeakMemMB = %v, want %v for %q", deref(got), deref(tt.want), tt.content)
			}
		})
	}
}

// TestCgroupCPUUsec_Parsing pins the usage_usec extraction out of cpu.stat's
// multi-line format.
func TestCgroupCPUUsec_Parsing(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    *int64
	}{
		{"full stat", "usage_usec 1234567\nuser_usec 900000\nsystem_usec 334567\nnr_periods 0\n", ptr(int64(1234567))},
		{"usage not first", "user_usec 900000\nusage_usec 42\n", ptr(int64(42))},
		{"zero usage is a real measurement", "usage_usec 0\n", ptr(int64(0))},
		{"no usage_usec line", "user_usec 900000\n", nil},
		{"garbage value", "usage_usec abc\n", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "cpu.stat"), []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			got := cgroupCPUUsec(dir)
			if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
				t.Errorf("cgroupCPUUsec = %v, want %v for %q", deref(got), deref(tt.want), tt.content)
			}
		})
	}
}

// TestCgroupRunActuals_PartialAndMissing covers the two fail-open shapes the
// recorded actuals must survive: a pre-5.19 kernel (no memory.peak, so CPU
// alone is recorded and the peak stays absent) and a group that is already
// gone / was never created.
func TestCgroupRunActuals_PartialAndMissing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cpu.stat"), []byte("usage_usec 7000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := cgroupRunActuals(dir)
	if got.PeakMemMB != nil {
		t.Errorf("PeakMemMB = %d with no memory.peak file, want absent", *got.PeakMemMB)
	}
	if got.CPUUsec == nil || *got.CPUUsec != 7000 {
		t.Errorf("CPUUsec = %v, want 7000", deref(got.CPUUsec))
	}

	for name, dir := range map[string]string{
		"no limit configured": "",
		"group already gone":  filepath.Join(t.TempDir(), "gone"),
	} {
		if a := cgroupRunActuals(dir); a.PeakMemMB != nil || a.CPUUsec != nil {
			t.Errorf("%s: actuals = %+v, want both absent", name, a)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// deref renders a maybe-absent number for test failure messages.
func deref[T any](p *T) any {
	if p == nil {
		return "absent"
	}
	return *p
}
