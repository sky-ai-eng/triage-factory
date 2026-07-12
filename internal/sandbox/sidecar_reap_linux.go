//go:build linux

package sandbox

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// reapOrphanSidecars SIGKILLs every host process whose comm matches
// SidecarCommName AND whose real uid falls inside the reserved sidecar
// band (IsSidecarUID) — both checks required, not just the name, so a
// sweep can never SIGKILL an unrelated process that happens to share the
// comm string (comm names aren't namespaced or otherwise exclusive to TF).
// Called once from hostOps.ReapOrphans at TF startup, the same
// boot-time-only guarantee the netns/veth/cgroup sweep above it relies on:
// a genuine match can only be a leftover child of a previous, hard-crashed
// broker process — sidecars are plain exec.Cmd children with no supervisor
// of their own, so a broker crash reparents any live sidecar to init, and
// nothing in THIS fresh broker process's runs registry knows about it.
// Best-effort and per-pid: a process that exits mid-scan, or one this uid
// can't signal, is skipped rather than aborting the sweep.
func reapOrphanSidecars() {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		sandboxLog.Warn("reap orphan sidecars: read /proc", "error", err)
		return
	}
	for _, e := range entries {
		pid, convErr := strconv.Atoi(e.Name())
		if convErr != nil {
			continue // not a pid directory
		}
		comm, readErr := os.ReadFile("/proc/" + e.Name() + "/comm")
		if readErr != nil {
			// Gone since ReadDir, or unreadable — either way not ours to
			// reap.
			continue
		}
		if strings.TrimRight(string(comm), "\n") != SidecarCommName {
			continue
		}
		uid, ok := procRealUID(pid)
		if !ok || !IsSidecarUID(uid) {
			// Comm matches but the uid doesn't (or couldn't be read) — not
			// a process this sweep owns; leave it alone.
			continue
		}
		sandboxLog.Info("reaping orphan sidecar", "pid", pid, "uid", uid)
		if killErr := syscall.Kill(pid, syscall.SIGKILL); killErr != nil && killErr != syscall.ESRCH {
			sandboxLog.Warn("reap orphan sidecar: kill failed", "pid", pid, "error", killErr)
		}
	}
}

// procRealUID reads a process's real uid from /proc/<pid>/status's "Uid:"
// line (format: "Uid:\t<real>\t<effective>\t<saved>\t<fs>"). Returns
// ok=false on any read/parse failure (process already gone, malformed
// line) so the caller treats an unreadable uid as "not confirmed ours"
// rather than guessing.
func procRealUID(pid int) (uid int, ok bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "Uid:" {
			continue
		}
		n, err := strconv.Atoi(fields[1])
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}
