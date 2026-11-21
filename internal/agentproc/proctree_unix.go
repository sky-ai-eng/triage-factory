//go:build unix

package agentproc

import (
	"os"
	"os/exec"
	"syscall"
)

// procTree is the platform handle that lets a cancel take down every
// process an agent started, not only the one TF spawned. On unix the process
// group does that work, so the handle carries no state.
type procTree struct{}

// configureProcessTree puts the child in its own process group and makes
// cancelling the command SIGKILL the whole group, so children the agent
// spawned go down with it.
func configureProcessTree(cmd *exec.Cmd) *procTree {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Process is non-nil here because the watcher only fires after
		// Start has succeeded. ESRCH is fine — it just means the
		// process group already exited on its own between Wait
		// returning and the cancel watcher reading runCtx.Done(),
		// which is a race exec handles internally.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return &procTree{}
}

// attach is a no-op: the process group is established by the kernel at fork.
func (t *procTree) attach(*os.Process) error { return nil }

// close is a no-op: nothing is held beyond the process group.
func (t *procTree) close() {}

// waitOutcome is the error Wait reports for the command: unchanged on unix,
// where no WaitDelay is set and so no wait-delay error can arise.
func waitOutcome(_ *exec.Cmd, err error) error { return err }
