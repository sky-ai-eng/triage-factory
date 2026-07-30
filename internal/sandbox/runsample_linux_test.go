//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCgroupCurrentMemMB_Parsing pins the live memory read: the same byte→MiB
// round-up the peak reader uses (so a sub-MiB jail plots as 1, never as a 0
// indistinguishable from unobserved), and the refusal of a zero — an empty
// group between fork and first allocation would otherwise plot as a collapse.
func TestCgroupCurrentMemMB_Parsing(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    *int
	}{
		{"exact MiB", "268435456\n", ptr(256)},
		{"rounds up", "268435457\n", ptr(257)},
		{"sub-MiB reads as 1, never 0", "4096\n", ptr(1)},
		{"zero is not an observation", "0\n", nil},
		{"garbage", "max\n", nil},
		{"empty", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "memory.current"), []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			got := cgroupCurrentMemMB(dir)
			if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
				t.Errorf("cgroupCurrentMemMB = %v, want %v for %q", deref(got), deref(tt.want), tt.content)
			}
		})
	}
}

// TestCgroupCPUUsecQuiet_SharesTeardownParse pins that the sampling read and
// the teardown read agree on the same cpu.stat body. They differ only in
// logging, and a divergent parse would make the series and the recorded
// end-state disagree for a reason that isn't the sampling cadence.
func TestCgroupCPUUsecQuiet_SharesTeardownParse(t *testing.T) {
	dir := t.TempDir()
	body := "usage_usec 918273\nuser_usec 600000\nsystem_usec 318273\n"
	if err := os.WriteFile(filepath.Join(dir, "cpu.stat"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	quiet, loud := cgroupCPUUsecQuiet(dir), cgroupCPUUsec(dir)
	if quiet == nil || loud == nil || *quiet != *loud || *quiet != 918273 {
		t.Fatalf("quiet = %v, teardown = %v, want both 918273", deref(quiet), deref(loud))
	}
}

// TestSampleRunCgroup_MissingGroupIsUnobserved covers the sampler's routine
// race: a jail that ended between being chosen for sampling and being read has
// no cgroup left, so the sample must carry nothing at all — the caller keys
// "write no row" off exactly that, and a zeroed sample would instead record a
// finished run as idle.
func TestSampleRunCgroup_MissingGroupIsUnobserved(t *testing.T) {
	for name, containerID := range map[string]string{
		"no container id":    "",
		"group already gone": "tf-nonexistent-run-250",
	} {
		if s := SampleRunCgroup(containerID); s.Observed() {
			t.Errorf("%s: sample = %+v, want unobserved", name, s)
		}
	}
}

// TestRunSample_Observed pins the predicate the sampler gates row-writing on:
// either metric present is an observation, and a measured zero CPU counts —
// a jail that has spent no CPU yet is a real reading, not a missing one.
func TestRunSample_Observed(t *testing.T) {
	tests := []struct {
		name string
		s    RunSample
		want bool
	}{
		{"nothing read", RunSample{}, false},
		{"memory only", RunSample{MemCurrentMB: ptr(128)}, true},
		{"cpu only", RunSample{CPUUsecCum: ptr(int64(5))}, true},
		{"measured zero cpu is an observation", RunSample{CPUUsecCum: ptr(int64(0))}, true},
		{"both", RunSample{MemCurrentMB: ptr(128), CPUUsecCum: ptr(int64(5))}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.s.Observed(); got != tt.want {
				t.Errorf("Observed = %v, want %v", got, tt.want)
			}
		})
	}
}
