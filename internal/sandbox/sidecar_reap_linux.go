//go:build linux

package sandbox

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// sidecarCommName is the kernel-visible name (procname.SetTitle's target,
// /proc/<pid>/comm) every run-sidecar process sets at startup. TASK_COMM_LEN
// truncates to 15 usable bytes; "tf-sidecar" fits with room to spare and
// deliberately carries no per-run suffix — reapOrphanSidecars runs once, at
// TF startup, before any run exists, so it needs no per-run discriminator to
// tell "mine" from "not mine": everything still wearing this name at that
// moment predates this process.
const sidecarCommName = "tf-sidecar"

// reapOrphanSidecars SIGKILLs every host process whose comm matches
// sidecarCommName. Called once from hostOps.ReapOrphans at TF startup, the
// same boot-time-only guarantee the netns/veth/cgroup sweep above it
// relies on: a match can only be a leftover child of a previous,
// hard-crashed broker process — sidecars are plain exec.Cmd children with
// no supervisor of their own, so a broker crash reparents any live sidecar
// to init, and nothing in THIS fresh broker process's runs registry knows
// about it. Best-effort and per-pid: a process that exits mid-scan, or one
// this uid can't signal, is skipped rather than aborting the sweep.
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
		if strings.TrimRight(string(comm), "\n") != sidecarCommName {
			continue
		}
		sandboxLog.Info("reaping orphan sidecar", "pid", pid)
		if killErr := syscall.Kill(pid, syscall.SIGKILL); killErr != nil && killErr != syscall.ESRCH {
			sandboxLog.Warn("reap orphan sidecar: kill failed", "pid", pid, "error", killErr)
		}
	}
}
