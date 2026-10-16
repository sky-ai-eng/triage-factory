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
// do them runs without ceilings, with one logged warning). The steps
// reorganize whatever hierarchy /sys/fs/cgroup shows, so they run only
// when that is this process's own private cgroup namespace
// (checkPrivateCgroupNamespace): in the host's namespace the same steps
// would move the host's processes and reconfigure its root cgroup.
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

	// initialCgroupNamespaceIno is the inode of the initial cgroup
	// namespace, PROC_CGROUP_INIT_INO in the kernel's include/linux/proc_ns.h.
	// A process whose /proc/self/ns/cgroup has this inode shares the host's
	// view of the hierarchy.
	initialCgroupNamespaceIno = 0xEFFFFFFB
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
	if err := requirePrivateCgroupNamespace(); err != nil {
		return err
	}
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

// requirePrivateCgroupNamespace reads the inputs of checkPrivateCgroupNamespace
// from this process.
func requirePrivateCgroupNamespace() error {
	fi, err := os.Stat("/proc/self/ns/cgroup")
	if err != nil {
		return fmt.Errorf("cgroup: stat own cgroup namespace: %w", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cgroup: own cgroup namespace has no inode")
	}
	self, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return fmt.Errorf("cgroup: read own cgroup: %w", err)
	}
	// The mount is read only when the namespace check has already passed, so
	// an initial-namespace process never opens a file under it.
	var procs []byte
	if st.Ino != initialCgroupNamespaceIno {
		if path, err := ownCgroupPath(self); err == nil {
			procs, _ = os.ReadFile(filepath.Join(cgroupRoot, path, "cgroup.procs"))
		}
	}
	return checkPrivateCgroupNamespace(st.Ino, self, procs, os.Getpid())
}

// checkPrivateCgroupNamespace decides whether /sys/fs/cgroup is the root of a
// cgroup namespace this process owns, so that reorganizing it cannot reach the
// host. Two things must hold:
//
//   - the process is not in the initial cgroup namespace (nsIno), whose
//     hierarchy is the host's; and
//   - the mount at /sys/fs/cgroup shows this namespace: the group the process
//     is in, named by procSelfCgroup, lists the process in its cgroup.procs
//     there (procsAtOwnPath). A namespace made with `unshare --cgroup` and no
//     remount fails this, because the mount still shows the host's view. A
//     process the setup already moved into init/ passes, so a broker
//     respawned inside the container is not refused.
//
// A pure function of its inputs so the decision is testable without root.
func checkPrivateCgroupNamespace(nsIno uint64, procSelfCgroup, procsAtOwnPath []byte, pid int) error {
	if nsIno == initialCgroupNamespaceIno {
		return fmt.Errorf("cgroup: in the host's cgroup namespace; per-run memory ceilings need a private one (compose cgroup: private)")
	}
	path, err := ownCgroupPath(procSelfCgroup)
	if err != nil {
		return err
	}
	want := strconv.Itoa(pid)
	for _, f := range strings.Fields(string(procsAtOwnPath)) {
		if f == want {
			return nil
		}
	}
	return fmt.Errorf("cgroup: %s does not show this process's cgroup namespace (%s does not list pid %d); mount a cgroup2 filesystem from inside it (compose cgroup: private)", cgroupRoot, path, pid)
}

// ownCgroupPath returns the cgroup path of the cgroup v2 line ("0::<path>")
// in a /proc/<pid>/cgroup body, relative to the reader's cgroup namespace
// root. A path that leaves the namespace root ("/../…") is refused: it names
// a group the mount at /sys/fs/cgroup cannot show.
func ownCgroupPath(procSelfCgroup []byte) (string, error) {
	for _, line := range strings.Split(string(procSelfCgroup), "\n") {
		path, ok := strings.CutPrefix(line, "0::")
		if !ok {
			continue
		}
		if !strings.HasPrefix(path, "/") || strings.Contains(path, "/../") || strings.HasSuffix(path, "/..") {
			return "", fmt.Errorf("cgroup: own cgroup %q is outside the namespace root", path)
		}
		return path, nil
	}
	return "", fmt.Errorf("cgroup: no cgroup v2 entry in /proc/self/cgroup")
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

// parseCPUUsec extracts usage_usec out of a cpu.stat body, reporting nothing
// for a body that has no usage_usec line or an unparseable value. Split from
// the read so the teardown read and the sampling read share one parse and
// can't drift; the two differ only in whether an unreadable FILE warns
// (teardown does, sampling must not) — a body that reads but doesn't parse is
// silent on both paths.
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
// previous process. Called from ReapOrphans; does nothing outside a private
// cgroup namespace, where tf-runs/ is not this package's to sweep.
// Best-effort — a non-empty group (still-running orphan runsc, itself about
// to be reaped) just stays for the next sweep.
func reapOrphanRunCgroups() {
	if requirePrivateCgroupNamespace() != nil {
		return
	}
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
