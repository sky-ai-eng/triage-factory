//go:build linux

package sandbox

import (
	"context"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
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

	// containerID is runsc's own key for this jail — what its persisted
	// container state is filed under, and so what Wait delete-forces once
	// the child is reaped.
	containerID string

	// tap forwards the child's stderr to the broker's stderr and keeps a
	// bounded tail of it; stderrTail is that tail, captured by Wait and read
	// by StderrTail afterwards, on the same
	// written-before-Wait-returns/read-after contract actuals has.
	tap        *stderrTap
	stderrTail string

	// actuals is captured by Wait, in the instant between the child's exit
	// and the cgroup's removal, and read by Actuals afterwards. Written and
	// read on the broker's single supervising goroutine, so the
	// Wait-then-Actuals ordering is the whole synchronization.
	actuals RunActuals
}

// stderrTailBytes bounds the tail of a jail's stderr the launch retains for
// diagnostics. runsc's own create/boot failures are a line or two; the
// ceiling is what keeps a chatty jail from turning one error message into a
// dump.
const stderrTailBytes = 8 * 1024

// stderrDrainGrace bounds how long the tail read waits for the forwarding
// goroutine to see EOF after the child exits. EOF arrives the instant every
// holder of the write end is gone, so this only covers a process that
// inherited fd 2 and outlived runsc — which forfeits the last few bytes of
// its tail rather than delaying the reap.
const stderrDrainGrace = 2 * time.Second

// stderrTap forwards the runsc child's stderr to the broker's own stderr —
// where gVisor's boot log has always gone — while retaining a bounded tail
// the launch can hand back alongside the exit status. That tail is what
// makes a jail that dies before connecting a one-line diagnosis instead of
// an empty "stderr:" in the orchestrator's error.
//
// It owns its own pipe rather than assigning an io.MultiWriter to
// cmd.Stderr: os/exec turns a non-*os.File writer into a pipe whose copy
// cmd.Wait() then blocks on, so any process that inherited fd 2 and outlived
// runsc would hold the reap open. Handing the child a real *os.File keeps
// the exec fast path (dup2, no copy goroutine inside os/exec) and leaves
// Wait's timing exactly as it was.
//
// What crosses it is diagnostics, never the run's payload — that is the
// NDJSON stream on the passed-through stdio socket, which no broker-side
// code reads.
type stderrTap struct {
	mu   sync.Mutex
	tail tailBuffer
	done chan struct{}
}

// newStderrTap returns the tap and the write end to hand the child as its
// stderr. The caller owns that file: close it once the child has inherited
// its own copy, or the forwarding goroutine never sees EOF.
func newStderrTap(sink io.Writer) (*stderrTap, *os.File, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	t := &stderrTap{done: make(chan struct{})}
	t.tail.max = stderrTailBytes
	go func() {
		defer close(t.done)
		defer func() { _ = r.Close() }()
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				t.mu.Lock()
				_, _ = t.tail.Write(buf[:n])
				t.mu.Unlock()
				if sink != nil {
					_, _ = sink.Write(buf[:n])
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return t, w, nil
}

// Tail returns what the child last wrote to stderr, after waiting out the
// drain grace for the forwarding goroutine to finish. Call it once the child
// has exited; nil taps (a pipe this host wouldn't give us) report nothing.
func (t *stderrTap) Tail() string {
	if t == nil {
		return ""
	}
	select {
	case <-t.done:
	case <-time.After(stderrDrainGrace):
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.tail.String()
}

// LaunchSupervised is the cap-broker's runtime launcher. It clears any
// runsc state a dead predecessor left under this container id, builds the
// runsc command for the broker-built bundle (bundleDir, from PrepareBundle
// — the broker owns the spec), sets up the per-run memory cgroup
// (fail-open), wires the provided socket file as the runtime's stdin AND
// stdout, forwards the runtime's stderr to the broker's own stderr while
// keeping a bounded tail (gVisor boot logs, surfaced to logs — never the
// agent's payload channel), starts runsc, and returns a SupervisedRuntime.
// The bundle's lifetime is the caller's (the broker's launchRun removes it
// when the run is reaped); LaunchSupervised does not touch it.
//
// It takes OWNERSHIP of stdio: after Start the child has inherited a dup,
// so LaunchSupervised closes the caller's *os.File. Combined with the
// broker closing its own dialed connection, the bytes of the agent's
// stream never enter the broker's address space — there is no read on the
// socket fd anywhere on this path, and every broker-side copy is closed
// after the exec. ctx ties the runtime's lifetime to the broker (cancel →
// SIGKILL of the runsc parent).
func LaunchSupervised(ctx context.Context, bundleDir, containerID string, memoryLimitMB int, stdio *os.File, stderr io.Writer) (*SupervisedRuntime, error) {
	// Pre-launch hygiene. runsc refuses to create a container over existing
	// state, and a killed predecessor always leaves some — under an id the
	// next engagement of the same run re-mints exactly, because ids are
	// deterministic in (run, subnet index). Clearing first is the standard
	// container-runtime idiom, and the one that also self-heals the deaths
	// no reap saw: a crashed broker, a host that died mid-run.
	if err := DeleteRuntimeState(ctx, containerID); err != nil {
		sandboxLog.Warn("clearing stale runsc container state failed; the launch may collide with it", "container", containerID, "error", err)
	}

	cmd := buildRuntimeCmd(ctx, bundleDir, containerID)
	sr := &SupervisedRuntime{cmd: cmd, containerID: containerID}

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

	// Stderr goes through the tap, which forwards to the broker's sink (where
	// gVisor's boot log has always gone) while keeping a bounded tail for the
	// launch result. Fail open to the bare sink if this host won't give us a
	// pipe: a missing diagnostic tail is never a reason to refuse a launch.
	tap, tapFile, tapErr := newStderrTap(stderr)
	if tapErr != nil {
		sandboxLog.Warn("stderr tail unavailable for this jail", "container", containerID, "error", tapErr)
		cmd.Stderr = stderr
	} else {
		sr.tap = tap
		cmd.Stderr = tapFile
	}

	err := cmd.Start()

	// Hand-off complete (or failed): drop our copies of the fds so nothing
	// on the broker side references the run's stdio or its cgroup past
	// launch. On success the child already holds inherited dups. The tap's
	// write end goes with them — while this process holds a copy the
	// forwarding goroutine can never see EOF.
	_ = stdio.Close()
	if tapFile != nil {
		_ = tapFile.Close()
	}
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
// OOM-killed, captures what the run actually cost (see Actuals), and removes
// the per-run cgroup. The exit error mirrors exec.Cmd.Wait (nil on clean
// exit).
//
// Every cgroup read happens before the group is removed — the OOM
// attribution needs live memory.events, the actuals need memory.peak /
// cpu.stat — because rmdir destroys all of it and nothing downstream can
// reconstruct the numbers. This process owns the cgroup lifecycle, so it is
// also the only one that can read them race-free.
func (s *SupervisedRuntime) Wait() (oomKilled bool, err error) {
	err = s.cmd.Wait()
	s.stderrTail = s.tap.Tail()
	oomKilled = cgroupOOMKilled(s.cgroupDir)
	s.actuals = cgroupRunActuals(s.cgroupDir)
	_ = removeRunCgroup(s.cgroupDir)

	// Eager state clear, the sibling of the cgroup removal above: the id's
	// runsc state goes away the moment the jail is reaped rather than
	// lingering until some later launch happens to reuse the id. On a
	// graceful exit runsc has already removed it and this is a no-op; on a
	// killed one — every stop, every cancellation — this is what returns the
	// id to a usable state. The pre-launch delete remains the backstop for
	// the deaths this path never runs for.
	if derr := DeleteRuntimeState(context.Background(), s.containerID); derr != nil {
		sandboxLog.Warn("clearing runsc container state after reap failed", "container", s.containerID, "error", derr)
	}
	return oomKilled, err
}

// StderrTail reports the last bytes the jail wrote to stderr — gVisor's own
// create/boot errors, and whatever the jailed process put on fd 2 — bounded
// to stderrTailBytes. Valid only after Wait, exactly like Actuals, and for
// the same reason: it is captured on the one exit path every death funnels
// through.
func (s *SupervisedRuntime) StderrTail() string { return s.stderrTail }

// Actuals reports what the run consumed, as captured by Wait. Valid only
// after Wait; the zero value (both fields absent) before it, and equally
// when the run had no cgroup to measure or the host's kernel offered
// nothing to read.
//
// A separate accessor rather than extra Wait returns because the sidecar
// runtime the broker supervises through the same registry is an ordinary
// host process with no cgroup — it has no actuals to report, and shouldn't
// have to pretend otherwise.
func (s *SupervisedRuntime) Actuals() RunActuals { return s.actuals }

// Kill SIGKILLs the runtime (the runsc parent); gVisor propagates the
// signal into the sandboxed init. Idempotent — a no-op once the process
// has exited (ESRCH is fine).
func (s *SupervisedRuntime) Kill() error {
	if s.cmd.Process == nil {
		return nil
	}
	return s.cmd.Process.Signal(syscall.SIGKILL)
}
