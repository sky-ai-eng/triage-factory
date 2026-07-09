//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

// buildRuntimeCmd is the seam both launch paths (in-process localRun and
// the broker's LaunchSupervised) build the runsc command through. A var,
// not a direct newRunscCommand call, so tests can substitute a stand-in
// for runsc (e.g. a bidirectional NDJSON echo) and exercise the stdio and
// supervision wiring without a real gVisor host.
var buildRuntimeCmd = newRunscCommand

// syncBuf is a minimal concurrency-safe buffer for capturing a runsc
// child's stderr, which os/exec writes from its own goroutine while the
// owner may read it via Stderr() after Wait.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// localRun is the in-process LaunchedRun: a runsc *exec.Cmd whose stdio
// are ordinary pipes the orchestrator reads/writes in the same address
// space. This is the TF_PRIVSEP-off path — behaviorally identical to the
// pre-split StdinPipe/StdoutPipe/Start/Wait dance wrap() used to hand back
// directly.
type localRun struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader
	stderr *syncBuf

	// bundleDir is the per-run OCI bundle this launch built (PrepareBundle);
	// the in-process launcher owns it now that the bundle is constructed
	// here rather than by wrap, so Close removes it.
	bundleDir string

	// cgroupDir is the per-run memory cgroup (empty when no limit); cgroupFD
	// is its dir-fd, needed only for the child's clone3 at Start and closed
	// immediately after.
	cgroupDir string
	cgroupFD  *os.File

	oomOnce   sync.Once
	oom       bool
	closeOnce sync.Once
}

// LaunchRun (in-process) builds the OCI bundle from the validated launch
// params (PrepareBundle — the broker-owned spec construction, run here in
// the single-trust-domain non-privsep path), builds the runsc command for
// it, sets up the per-run memory cgroup (fail-open, as wrap did before this
// split), wires ordinary pipes for stdio, and returns a not-yet-started
// localRun. Start begins execution.
func (hostOps) LaunchRun(ctx context.Context, p LaunchParams) (LaunchedRun, error) {
	bundleDir, err := PrepareBundle(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("sandbox: prepare bundle: %w", err)
	}
	cmd := buildRuntimeCmd(ctx, bundleDir, p.ContainerID)
	lr := &localRun{cmd: cmd, stderr: &syncBuf{}, bundleDir: bundleDir}

	// Per-run memory ceiling. Fail-open by design (a host that can't
	// complete cgroup setup runs without a ceiling and says so once),
	// matching the behavior wrap() had when it did this inline.
	if p.MemoryLimitMB > 0 {
		if dir, f, err := setupAndCreateRunCgroup(p.ContainerID, p.MemoryLimitMB); err != nil {
			logCgroupFailOpenOnce(err)
		} else {
			lr.cgroupDir = dir
			lr.cgroupFD = f
			cmd.SysProcAttr = &syscall.SysProcAttr{
				UseCgroupFD: true,
				CgroupFD:    int(f.Fd()),
			}
		}
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		lr.removeCgroup()
		_ = cleanupBundle(lr.bundleDir)
		return nil, fmt.Errorf("sandbox: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		lr.removeCgroup()
		_ = cleanupBundle(lr.bundleDir)
		return nil, fmt.Errorf("sandbox: stdout pipe: %w", err)
	}
	lr.stdin = stdin
	lr.stdout = stdout
	cmd.Stderr = lr.stderr
	return lr, nil
}

func (l *localRun) Start() error {
	if err := l.cmd.Start(); err != nil {
		return err
	}
	// The cgroup fd is only needed for the clone3 at Start; the child is
	// now inside the group. Drop our copy so we hold no fd past launch.
	if l.cgroupFD != nil {
		_ = l.cgroupFD.Close()
		l.cgroupFD = nil
	}
	return nil
}

func (l *localRun) Stdin() io.WriteCloser { return l.stdin }
func (l *localRun) Stdout() io.Reader     { return l.stdout }
func (l *localRun) Stderr() string        { return l.stderr.String() }

func (l *localRun) Pid() int {
	if l.cmd.Process == nil {
		return 0
	}
	return l.cmd.Process.Pid
}

func (l *localRun) Wait() error {
	err := l.cmd.Wait()
	// Read OOM state now, before Close removes the cgroup.
	l.oomOnce.Do(func() { l.oom = cgroupOOMKilled(l.cgroupDir) })
	return err
}

func (l *localRun) OOMKilled() bool {
	l.oomOnce.Do(func() { l.oom = cgroupOOMKilled(l.cgroupDir) })
	return l.oom
}

// Close tears down the per-run cgroup, the stdio pipes, and the OCI bundle
// this launch built. The runsc child itself is killed by cmd.Cancel (the
// run context) when the orchestrator cancels; Close covers the
// abandoned-bring-up path (Start never ran, so cmd.Wait never closed the
// pipes) and reclaims the cgroup + bundle created at construction. Only
// ever called before Start or after Wait, so closing the pipes never races
// an in-flight read. Idempotent.
func (l *localRun) Close() error {
	l.closeOnce.Do(func() {
		if l.stdin != nil {
			_ = l.stdin.Close()
		}
		if c, ok := l.stdout.(io.Closer); ok {
			_ = c.Close()
		}
		l.removeCgroup()
		_ = cleanupBundle(l.bundleDir)
	})
	return nil
}

func (l *localRun) removeCgroup() {
	if l.cgroupFD != nil {
		_ = l.cgroupFD.Close()
		l.cgroupFD = nil
	}
	_ = removeRunCgroup(l.cgroupDir)
}

// setupAndCreateRunCgroup performs the one-time cgroup delegation dance
// then creates the per-run group, returning its dir and clone3 fd. Split
// out so both the in-process localRun and the broker's LaunchSupervised
// build the ceiling the same way.
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
