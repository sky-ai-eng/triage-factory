//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// sampleRunCgroup reads memory.current and cpu.stat from tf-runs/<containerID>
// — the same group newRunCgroup created and cgroupRunActuals reads at exit.
// Every failure is swallowed to an absent field: see SampleRunCgroup for why
// this path never logs.
func sampleRunCgroup(containerID string) RunSample {
	dir := filepath.Join(cgroupRunsDir, containerID)
	return RunSample{
		MemCurrentMB: cgroupCurrentMemMB(dir),
		CPUUsecCum:   cgroupCPUUsecQuiet(dir),
	}
}

// cgroupCurrentMemMB reports the group's resident memory in MiB from
// memory.current — always present on cgroup v2, no kernel-version floor like
// memory.peak's 5.19.
//
// Bytes round UP to MiB for the same reason the peak does: a jail holding
// under 1 MiB is still holding memory, and truncating it to 0 would be
// indistinguishable from an unobserved row. A read of exactly 0 is refused for
// the same reason — an empty group between fork and first allocation has
// nothing worth plotting, and a plotted zero reads as "the run collapsed".
func cgroupCurrentMemMB(dir string) *int {
	data, err := os.ReadFile(dir + "/memory.current")
	if err != nil {
		return nil
	}
	curBytes, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || curBytes <= 0 {
		return nil
	}
	mb := int((curBytes + 1024*1024 - 1) / (1024 * 1024))
	return &mb
}

// cgroupCPUUsecQuiet is cgroupCPUUsec without its unreadable-file warning —
// the sampling counterpart of the teardown read, and the only way the two
// differ. A teardown that can't open cpu.stat has lost a number nothing can
// reconstruct and says so; a sampler tick that can't has simply raced a run
// that ended, and one warn per finished run is noise, not signal. The parse is
// shared, so a body that reads but yields no usage_usec is silently absent on
// both paths.
func cgroupCPUUsecQuiet(dir string) *int64 {
	data, err := os.ReadFile(dir + "/cpu.stat")
	if err != nil {
		return nil
	}
	return parseCPUUsec(data)
}
