//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Per-run memory ceilings, cgroup v2. TF owns the cgroups itself and
// starts each runsc directly inside one via clone3(CLONE_INTO_CGROUP)
// (exec.Cmd CgroupFD) — runsc keeps running with --ignore-cgroups and
// never learns cgroups exist. The limit covers the whole sandbox tree
// (sentry + gofer + everything the app allocates through the sentry's
// memfd), so one pathological run OOM-kills alone instead of taking
// the host down with it.
//
// The container environment needs three one-time steps, all performed
// lazily on the first limited run and all fail-open (a host that can't
// do them runs without ceilings, with one logged warning):
//
//  1. Remount /sys/fs/cgroup read-write. runc mounts it ro even with a
//     private cgroup namespace; with CAP_SYS_ADMIN the remount touches
//     only this mount-namespace's view. The compose service sets
//     `cgroup: private`, so the visible tree is the container's own
//     delegated subtree — never host cgroups.
//  2. Move the container's root-cgroup processes into an init/ leaf.
//     cgroup v2's no-internal-processes rule forbids enabling subtree
//     controllers while the root has member processes.
//  3. Enable the memory controller on the root and on the tf-runs/
//     parent all per-run groups are created under.

const (
	cgroupRoot    = "/sys/fs/cgroup"
	cgroupRunsDir = cgroupRoot + "/tf-runs"
	cgroupInitDir = cgroupRoot + "/init"
)

var (
	cgroupSetupOnce sync.Once
	cgroupSetupErr  error
)

// setupRunCgroups performs the one-time delegation dance. Idempotent
// across restarts: every step tolerates already-done state.
func setupRunCgroups() error {
	cgroupSetupOnce.Do(func() { cgroupSetupErr = doSetupRunCgroups() })
	return cgroupSetupErr
}

func doSetupRunCgroups() error {
	ctrl, err := os.ReadFile(cgroupRoot + "/cgroup.controllers")
	if err != nil {
		return fmt.Errorf("cgroup: read controllers: %w", err)
	}
	if !strings.Contains(string(ctrl), "memory") {
		return fmt.Errorf("cgroup: memory controller not available")
	}

	if err := ensureCgroupRootWritable(); err != nil {
		return err
	}
	if err := os.MkdirAll(cgroupRunsDir, 0o755); err != nil {
		return fmt.Errorf("cgroup: create %s: %w", cgroupRunsDir, err)
	}

	// no-internal-processes: park the container's root procs in init/
	// before enabling subtree controllers. Retried once because a
	// process can fork between the read and the writes.
	if err := os.MkdirAll(cgroupInitDir, 0o755); err != nil {
		return fmt.Errorf("cgroup: create init leaf: %w", err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		procs, err := os.ReadFile(cgroupRoot + "/cgroup.procs")
		if err != nil {
			return fmt.Errorf("cgroup: read root procs: %w", err)
		}
		for _, pid := range strings.Fields(string(procs)) {
			// Failures are fine: the pid exited, or is a kernel thread.
			_ = os.WriteFile(cgroupInitDir+"/cgroup.procs", []byte(pid), 0o644)
		}
		if err = os.WriteFile(cgroupRoot+"/cgroup.subtree_control", []byte("+memory"), 0o644); err == nil {
			break
		} else if attempt == 1 {
			return fmt.Errorf("cgroup: enable memory on root subtree: %w", err)
		}
	}
	if err := os.WriteFile(cgroupRunsDir+"/cgroup.subtree_control", []byte("+memory"), 0o644); err != nil {
		return fmt.Errorf("cgroup: enable memory on tf-runs subtree: %w", err)
	}
	return nil
}

// statReadOnly is ST_RDONLY per statfs(2) — package syscall doesn't
// export it, but the kernel fills Statfs_t.Flags from the same bit as
// MS_RDONLY.
const statReadOnly = 0x1

// ensureCgroupRootWritable remounts /sys/fs/cgroup read-write if the
// current mount is read-only. runc mounts it ro even under a private
// cgroup namespace. Writability is checked directly via statfs rather
// than inferred from a mkdir outcome: on a restart, tf-runs/ can
// already exist from a prior run, and MkdirAll succeeds for an
// existing directory without ever touching the mount — that left the
// remount unfired and made setup fail open even when a remount would
// have fixed it.
func ensureCgroupRootWritable() error {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(cgroupRoot, &stat); err != nil {
		return fmt.Errorf("cgroup: statfs %s: %w", cgroupRoot, err)
	}
	if stat.Flags&statReadOnly == 0 {
		return nil
	}
	if err := syscall.Mount("cgroup", cgroupRoot, "cgroup2",
		syscall.MS_REMOUNT|syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("cgroup: fs read-only and remount failed: %w", err)
	}
	return nil
}

// newRunCgroup creates tf-runs/<name> with a hard memory ceiling and
// returns the dir plus an open dir handle for exec.Cmd's CgroupFD (the
// child starts inside the group via clone3 — no post-start PID write
// to race). The *os.File is returned rather than a raw fd so the
// caller keeps it alive until after Start; teardown closes it. Swap is
// pinned to zero so the ceiling is real: without it the kernel swaps
// the run up to the limit instead of OOM-killing it, and the host pays
// the thrash.
func newRunCgroup(name string, limitMB int) (dir string, f *os.File, err error) {
	dir = filepath.Join(cgroupRunsDir, name)
	if err := os.Mkdir(dir, 0o755); err != nil && !os.IsExist(err) {
		return "", nil, fmt.Errorf("cgroup: create %s: %w", dir, err)
	}
	limit := strconv.FormatInt(int64(limitMB)*1024*1024, 10)
	if err := os.WriteFile(dir+"/memory.max", []byte(limit), 0o644); err != nil {
		_ = os.Remove(dir)
		return "", nil, fmt.Errorf("cgroup: set memory.max: %w", err)
	}
	// Best-effort: swap accounting may be disabled host-wide.
	_ = os.WriteFile(dir+"/memory.swap.max", []byte("0"), 0o644)

	f, err = os.OpenFile(dir, os.O_RDONLY, 0)
	if err != nil {
		_ = os.Remove(dir)
		return "", nil, fmt.Errorf("cgroup: open dir fd: %w", err)
	}
	return dir, f, nil
}

// cgroupOOMKilled reports whether the group's memory.events recorded
// an OOM kill. Read while the cgroup still exists (before teardown).
func cgroupOOMKilled(dir string) bool {
	if dir == "" {
		return false
	}
	data, err := os.ReadFile(dir + "/memory.events")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "oom_kill" {
			n, _ := strconv.Atoi(fields[1])
			return n > 0
		}
	}
	return false
}

// peakUnsupportedOnce keeps the missing-memory.peak warning to one line
// per process. The file's absence is a property of the kernel the box
// boots (< 5.19), not of any single run, so warning per teardown would say
// the same thing once per run forever.
var peakUnsupportedOnce sync.Once

// cgroupRunActuals reads the group's measured cost — peak memory and CPU
// time — while the cgroup still exists. Every caller is on an exit path
// immediately before removeRunCgroup, which is the only moment these
// numbers are readable at all: rmdir takes them with it and nothing can
// reconstruct them afterwards.
//
// Best-effort in both fields independently: an unreadable file yields an
// absent value, never an error, so a teardown never fails over accounting.
func cgroupRunActuals(dir string) RunActuals {
	if dir == "" {
		return RunActuals{}
	}
	return RunActuals{
		PeakMemMB: cgroupPeakMemMB(dir),
		CPUUsec:   cgroupCPUUsec(dir),
	}
}

// cgroupPeakMemMB reports the group's high-water memory use in MiB from
// memory.peak, which the kernel has only carried since 5.19. On an older
// kernel the file is absent and this reports nothing — deliberately NOT a
// memory.current sample, which at exit reads near zero and would record a
// wildly understated peak as though it were measured. An honest NULL is
// worth more than a number nobody can tell is wrong.
//
// Bytes round UP to MiB: a run that peaked under 1 MiB is still a run that
// peaked, and truncating it to 0 would be indistinguishable from a
// never-measured row. The ceiling can't overshoot the profile's budget
// either — the peak is bounded by memory.max, which is a whole number of
// MiB.
func cgroupPeakMemMB(dir string) *int {
	data, err := os.ReadFile(dir + "/memory.peak")
	if err != nil {
		if os.IsNotExist(err) {
			peakUnsupportedOnce.Do(func() {
				sandboxLog.Warn("cgroup memory.peak unavailable; recording no peak-memory actuals (needs kernel >= 5.19)")
			})
			return nil
		}
		sandboxLog.Warn("read cgroup memory.peak failed; recording no peak-memory actual", "cgroup", dir, "error", err)
		return nil
	}
	peakBytes, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || peakBytes <= 0 {
		return nil
	}
	mb := int((peakBytes + 1024*1024 - 1) / (1024 * 1024))
	return &mb
}

// cgroupCPUUsec reports the group's cumulative CPU time (user + system, the
// whole jail) from cpu.stat's usage_usec. cgroup v2 exposes usage_usec on
// every group regardless of whether the cpu controller is enabled on the
// parent's subtree_control, so this needs no counterpart to the memory
// controller's delegation dance in setupRunCgroups.
func cgroupCPUUsec(dir string) *int64 {
	data, err := os.ReadFile(dir + "/cpu.stat")
	if err != nil {
		sandboxLog.Warn("read cgroup cpu.stat failed; recording no cpu-time actual", "cgroup", dir, "error", err)
		return nil
	}
	return parseCPUUsec(data)
}

// parseCPUUsec extracts usage_usec out of a cpu.stat body. Split from the read
// so the teardown read (which warns on failure) and the sampling read (which
// must not) share one parse and can't drift.
func parseCPUUsec(data []byte) *int64 {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "usage_usec" {
			usec, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return nil
			}
			return &usec
		}
	}
	return nil
}

// removeRunCgroup tears the group down. rmdir requires the group to be
// empty; the runsc tree is SIGKILLed before Close runs, but zombie
// reaping can lag, so retry briefly rather than leak the dir (the
// reaper also sweeps stragglers at next boot).
func removeRunCgroup(dir string) error {
	if dir == "" {
		return nil
	}
	var err error
	for i := 0; i < 10; i++ {
		if err = syscall.Rmdir(dir); err == nil || os.IsNotExist(err) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("cgroup: remove %s: %w", dir, err)
}

// reapOrphanRunCgroups removes leftover tf-runs/* groups from a
// previous process. Called from ReapOrphans; best-effort — a non-empty
// group (still-running orphan runsc, itself about to be reaped) just
// stays for the next sweep.
func reapOrphanRunCgroups() {
	entries, err := os.ReadDir(cgroupRunsDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			_ = syscall.Rmdir(filepath.Join(cgroupRunsDir, e.Name()))
		}
	}
}
