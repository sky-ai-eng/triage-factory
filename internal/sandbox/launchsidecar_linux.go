//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
)

// sidecarSubcommand is the fixed argv the broker execs for a run-sidecar —
// the same triagefactory binary this broker process is itself running from
// (resolved via os.Executable, never a caller-supplied path), invoked with
// its own subcommand. "The broker owns the command" applies here exactly as
// it does for the pinned tool-host entrypoint LaunchRun execs: the
// orchestrator sends only ContainerID (used solely for the sidecar's own
// process-title/log line), never a program or path.
const sidecarSubcommand = "run-sidecar"

// sidecarBinaryPath resolves the running broker's own executable. A var so
// tests can stub it without requiring a real installed binary.
var sidecarBinaryPath = os.Executable

// SidecarRuntime is a started run-sidecar child, supervised by the
// cap-broker the same shape SupervisedRuntime supervises a runsc child
// (Wait/Kill) — but simpler: no cgroup (Phase 1 sets no memory ceiling on
// the sidecar; it does no work yet) and no bundle to remove.
type SidecarRuntime struct {
	cmd *exec.Cmd
}

// LaunchSidecarProcess execs the run-sidecar subcommand as uid/gid (a plain
// syscall.Credential setuid+setgid — the broker is root, so this succeeds
// without CAP_SETUID living anywhere past this exec), with its stdio wired
// to the passed fd exactly as LaunchSupervised wires runsc's — the same
// fd-passthrough discipline: the child inherits a dup, this function closes
// its own copy immediately after Start, so the sidecar's bytes never enter
// the broker's address space. stderr goes to the broker's own stderr (its
// boot-line log + any crash output), never the orchestrator — matching
// LaunchSupervised's treatment of runsc's stderr.
//
// The setuid-from-root transition clears the child's effective/permitted
// capability sets (standard kernel behavior — see
// docs/security/privilege-separation.md's own uid-drop verification), which
// is what makes the sidecar capless. Unlike the orchestrator's own drop
// (docker/entrypoint.sh's `setpriv --bounding-set=-all`), this does NOT
// also clear the child's capability BOUNDING set — that needs an exec-time
// wrapper (setpriv or equivalent) between fork and exec, which Go's
// os/exec has no hook for, the same constraint that keeps the
// orchestrator's own drop external rather than in-process. Accepted
// residual for this phase: the sidecar does no privileged work and holds
// no capability-granting credential, so an empty bounding set closes no
// reachable escalation today; matching the orchestrator's stronger
// bounding-set guarantee is a hardening-phase improvement, not a
// correctness requirement for this harness.
//
// extra are additional descriptors handed to the child past its stdio, landing
// at fd 3 upward in order. Today that is exactly one: the run's shared-origin
// listener, bound by the broker on a privileged port the capless sidecar cannot
// bind itself (see SharedOriginListenerFD). They get the same treatment stdio
// does — the child inherits a dup and this function closes its own copies right
// after Start, whether or not Start succeeded.
func LaunchSidecarProcess(ctx context.Context, containerID string, uid, gid int, stdio *os.File, extra []*os.File, stderr io.Writer) (*SidecarRuntime, error) {
	closeExtra := func() {
		for _, f := range extra {
			if f != nil {
				_ = f.Close()
			}
		}
	}
	binPath, err := sidecarBinaryPath()
	if err != nil {
		_ = stdio.Close()
		closeExtra()
		return nil, fmt.Errorf("sandbox: resolve sidecar binary: %w", err)
	}

	cmd := exec.CommandContext(ctx, binPath, sidecarSubcommand, "--container-id", containerID)
	cmd.ExtraFiles = extra
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// The per-run uid/gid, plus exactly one supplementary group: this
		// run's jail gid (sidecarGroups). The broker is root here, so setting
		// the group succeeds; the setuid transition still clears the child's
		// effective/permitted caps (capless).
		Credential: &syscall.Credential{
			Uid:    uint32(uid),
			Gid:    uint32(gid),
			Groups: sidecarGroups(uid),
		},
	}
	cmd.Stdin = stdio
	cmd.Stdout = stdio
	cmd.Stderr = stderr
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// SIGKILL can't be caught, so there is no graceful variant to
		// prefer here — mirrors newRunscCommand's own Cancel hook.
		return cmd.Process.Signal(syscall.SIGKILL)
	}

	startErr := cmd.Start()
	// Hand-off complete (or failed): drop this process's copy of the fd
	// either way, matching LaunchSupervised's unconditional stdio.Close().
	_ = stdio.Close()
	closeExtra()
	if startErr != nil {
		return nil, fmt.Errorf("sandbox: start sidecar: %w", startErr)
	}
	return &SidecarRuntime{cmd: cmd}, nil
}

// sidecarGroups is the supplementary group set a sidecar at uid runs with: the
// jail gid of the same run, and nothing else.
//
// The sidecar creates and serves the run's agenthost socket and writes its
// gh-injector cert, and both have to reach the jail, which runs at another uid.
// An owner may chgrp its own file only to a group it belongs to, so the sidecar
// carries the jail's gid — this run's, derived from the same subnet index as
// its uid (ValidateSidecarLaunchParams ties the uid to that index), so the
// membership opens this run's files to this run's jail and to no other run.
//
// A uid outside the sidecar band gets an empty set, which still sheds the
// broker's own groups: only a test reaches here with one, since the broker
// refuses such a launch before it gets this far.
func sidecarGroups(uid int) []uint32 {
	if gid, ok := SidecarJailGID(uid); ok {
		return []uint32{uint32(gid)}
	}
	return []uint32{}
}

// Wait blocks until the sidecar exits. oomKilled is always false — Phase 1
// runs the sidecar with no memory ceiling, so there is no cgroup to
// attribute an OOM against.
func (r *SidecarRuntime) Wait() (oomKilled bool, err error) {
	return false, r.cmd.Wait()
}

// Kill SIGKILLs the sidecar. Idempotent — a no-op once the process has
// exited (ESRCH is fine), matching SupervisedRuntime.Kill.
func (r *SidecarRuntime) Kill() error {
	if r.cmd.Process == nil {
		return nil
	}
	return r.cmd.Process.Signal(syscall.SIGKILL)
}
