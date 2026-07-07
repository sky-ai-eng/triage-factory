//go:build linux

package sandbox

import (
	"context"
	"os/exec"
	"syscall"
)

// newRunscCommand constructs the runsc invocation matching the
// validated probe (precns-test.sh line 66) plus the systrap platform
// choice (benchmark — 27% faster sustained syscalls than
// ptrace on the same Fly Machine, same cold-start).
//
//	runsc --platform=systrap --ignore-cgroups --network=sandbox \
//	      --host-uds=open run --bundle <bundleDir> <containerID>
//
// --host-uds=open is load-bearing, not optional: it grants the
// jailed process permission to connect() to host unix-domain sockets
// bind-mounted into its filesystem. Without it gVisor defaults to
// host-uds=none — the gofer still exposes the socket inode (so a stat sees
// ModeSocket) but refuses the connect with ECONNREFUSED, which breaks the
// per-run agenthost IPC socket (/run/tf.sock) every `triagefactory exec`
// subcommand rides on. "open", not "create"/"all": the agent only ever
// connects (pure client) and must never be able to *bind* a host socket.
//
// Security scope: host-uds is sandbox-wide, so the reachable set is exactly
// the host sockets in the jail's mount tree — today the single per-run
// daemon socket (a single-file bind of /run/tf/<run_id>.sock; the /run/tf
// dir is not mounted, so runs can't reach each other), and nothing else
// mounted is a socket. The flag is permissive-but-inert without a mount, so
// it grants nothing a run wasn't already handed a socket for. Standing
// invariant it relies on: never bind-mount a sensitive host socket (or a
// broad host dir containing one) into the jail, and never widen to
// create/all. This is orthogonal to network egress (the forward proxy +
// applyEgressPolicy L3 policy on sb.HostIP) — host-uds opens no network
// path. See internal/sandbox/doc.go (Property B) and the socket lifecycle
// in cmd/exec/agenthost/socket_linux.go.
//
// cmd.Cancel SIGKILLs the runsc parent; gVisor's supervision
// propagates the signal into the sandboxed init. No Setpgid —
// runsc manages its own process tree.
func newRunscCommand(ctx context.Context, bundleDir, containerID string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "runsc",
		"--platform=systrap",
		"--ignore-cgroups",
		"--network=sandbox",
		"--host-uds=open",
		"run",
		"--bundle", bundleDir,
		containerID,
	)
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// SIGKILL the runsc process itself. gVisor sandbox-init is
		// runsc's child; killing the parent tears down the sandbox.
		// ESRCH is fine — process already exited between Wait
		// returning and the cancel watcher reading ctx.Done().
		return cmd.Process.Signal(syscall.SIGKILL)
	}
	return cmd
}
