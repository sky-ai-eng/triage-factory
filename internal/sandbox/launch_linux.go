//go:build linux

package sandbox

import (
	"context"
	"io"
	"os"
	"os/exec"
	"syscall"
)

// buildRuntimeCmd is the seam the broker's LaunchSupervised builds the runsc
// command through. A var, not a direct newRunscCommand call, so tests can
// substitute a stand-in for runsc (e.g. a bidirectional NDJSON echo) and
// exercise the stdio and supervision wiring without a real gVisor host.
var buildRuntimeCmd = newRunscCommand

// setupAndCreateRunCgroup performs the one-time cgroup delegation dance
// then creates the per-run group, returning its dir and clone3 fd. The
// broker's LaunchSupervised builds the ceiling through it.
func setupAndCreateRunCgroup(name string, limitMB int) (dir string, f *os.File, err error) {
	if err := setupRunCgroups(); err != nil {
		return "", nil, err
	}
	return newRunCgroup(name, limitMB)
}

// SupervisedRuntime is a started runsc child owned and supervised by the
// cap-broker. The broker keeps one per in-flight run and reaches it for
// Wait (exit status + OOM) and Kill (cancellation). It holds no fd to the
// run's stdio — LaunchSupervised handed that to the runtime and closed its
// own copy.
type SupervisedRuntime struct {
	cmd       *exec.Cmd
	cgroupDir string
}

// LaunchSupervised is the cap-broker's runtime launcher. It builds the
// runsc command for the broker-built bundle (bundleDir, from PrepareBundle
// — the broker owns the spec), sets up the per-run memory cgroup
// (fail-open), wires the provided socket file as the runtime's stdin AND
// stdout, points the runtime's stderr at the broker's own stderr (gVisor
// boot logs, surfaced to logs — never the agent's payload channel), starts
// runsc, and returns a SupervisedRuntime. The bundle's lifetime is the
// caller's (the broker's launchRun removes it when the run is reaped);
// LaunchSupervised does not touch it.
//
// It takes OWNERSHIP of stdio: after Start the child has inherited a dup,
// so LaunchSupervised closes the caller's *os.File. Combined with the
// broker closing its own dialed connection, the bytes of the agent's
// stream never enter the broker's address space — there is no read on the
// socket fd anywhere on this path, and every broker-side copy is closed
// after the exec. ctx ties the runtime's lifetime to the broker (cancel →
// SIGKILL of the runsc parent).
func LaunchSupervised(ctx context.Context, bundleDir, containerID string, memoryLimitMB int, stdio *os.File, stderr io.Writer) (*SupervisedRuntime, error) {
	cmd := buildRuntimeCmd(ctx, bundleDir, containerID)
	sr := &SupervisedRuntime{cmd: cmd}

	var cgroupFD *os.File
	if memoryLimitMB > 0 {
		if dir, f, err := setupAndCreateRunCgroup(containerID, memoryLimitMB); err != nil {
			logCgroupFailOpenOnce(err)
		} else {
			sr.cgroupDir = dir
			cgroupFD = f
			cmd.SysProcAttr = &syscall.SysProcAttr{
				UseCgroupFD: true,
				CgroupFD:    int(f.Fd()),
			}
		}
	}

	cmd.Stdin = stdio
	cmd.Stdout = stdio
	cmd.Stderr = stderr

	err := cmd.Start()

	// Hand-off complete (or failed): drop our copies of the fds so nothing
	// on the broker side references the run's stdio or its cgroup past
	// launch. On success the child already holds inherited dups.
	_ = stdio.Close()
	if cgroupFD != nil {
		_ = cgroupFD.Close()
	}
	if err != nil {
		_ = removeRunCgroup(sr.cgroupDir)
		return nil, err
	}
	return sr, nil
}

// Wait blocks until the runtime exits, then reports whether it was
// OOM-killed and removes the per-run cgroup. The exit error mirrors
// exec.Cmd.Wait (nil on clean exit). Read the OOM state before removing
// the group so the attribution sees live memory.events.
func (s *SupervisedRuntime) Wait() (oomKilled bool, err error) {
	err = s.cmd.Wait()
	oomKilled = cgroupOOMKilled(s.cgroupDir)
	_ = removeRunCgroup(s.cgroupDir)
	return oomKilled, err
}

// Kill SIGKILLs the runtime (the runsc parent); gVisor propagates the
// signal into the sandboxed init. Idempotent — a no-op once the process
// has exited (ESRCH is fine).
func (s *SupervisedRuntime) Kill() error {
	if s.cmd.Process == nil {
		return nil
	}
	return s.cmd.Process.Signal(syscall.SIGKILL)
}
