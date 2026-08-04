//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
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

// runscStateDeleteTimeout bounds one state-clearing runsc invocation. It is
// a metadata operation — unlink a state file, tear down whatever a killed
// predecessor left behind — so it finishes in milliseconds; the bound is
// only there so a wedged runsc can't hold a launch or a boot sweep hostage.
const runscStateDeleteTimeout = 15 * time.Second

// buildRuntimeDeleteCmd is the seam DeleteRuntimeState builds its runsc
// invocation through — the sibling of launch_linux.go's buildRuntimeCmd,
// and a var for the same reason: tests substitute a stand-in and assert what
// would have been deleted without a gVisor host.
var buildRuntimeDeleteCmd = newRunscDeleteCommand

// newRunscDeleteCommand builds `runsc delete --force <containerID>`, the
// state-clearing counterpart of newRunscCommand's `runsc run`.
//
// No global flags, unlike the run command. Only --root decides where runsc
// keeps container state, and neither command sets it, so both resolve the
// same default root; platform/network/host-uds describe how to RUN a
// sandbox and have nothing to say about deleting one's state.
//
// --force because the state left by a killed runsc still says "running",
// which a plain delete refuses.
func newRunscDeleteCommand(ctx context.Context, containerID string) *exec.Cmd {
	return exec.CommandContext(ctx, "runsc", "delete", "--force", containerID)
}

// DeleteRuntimeState clears runsc's persisted state for one container id.
//
// `runsc run` removes that state itself only on a graceful exit. A SIGKILLed
// runsc — how every cancellation and every stop tears a jail down — leaves
// it behind, and runsc refuses to create a container over existing state.
// Container ids are deterministic in (run, subnet index), so the next
// engagement of the same conversation on a quiet executor mints the
// identical id and lands on the corpse. Clearing state before every launch
// and after every reap is what makes that reuse safe.
//
// Best-effort by contract, and quiet about the two non-failures: a host with
// no runsc has no state to clear, and an id runsc has never heard of is the
// expected case on the pre-launch call. Anything else comes back for the
// caller to log.
func DeleteRuntimeState(ctx context.Context, containerID string) error {
	if containerID == "" {
		return nil
	}
	// Detached from the caller's cancellation: the reap path calls this
	// precisely because the run's context was canceled, which is exactly
	// when the state most needs clearing. The timeout above is the only
	// bound that applies.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runscStateDeleteTimeout)
	defer cancel()

	cmd := buildRuntimeDeleteCmd(ctx, containerID)
	if cmd.Err != nil {
		// exec.Command records a PATH lookup failure here rather than at
		// Run. No runsc on this host means no state root and nothing to
		// clear — a local-mode process or a unit test, not a fault.
		return nil
	}
	out, err := cmd.CombinedOutput()
	if err == nil || isUnknownContainerOutput(out) {
		return nil
	}
	return fmt.Errorf("sandbox: runsc delete %s: %w: %s", containerID, err, strings.TrimSpace(string(out)))
}

// isUnknownContainerOutput reports whether a failed delete failed only
// because there was nothing to delete. runsc words this differently across
// versions (and between the state file and its lock), so match on the
// substrings all of them share rather than on one exact message.
func isUnknownContainerOutput(out []byte) bool {
	s := strings.ToLower(string(out))
	return strings.Contains(s, "does not exist") ||
		strings.Contains(s, "not found") ||
		strings.Contains(s, "no such file or directory")
}

// runscStateRoot mirrors gVisor's own default resolution of --root, which
// is where it persists per-container state: $XDG_RUNTIME_DIR/runsc when that
// is set (the rootless case), /var/run/runsc otherwise — which is what the
// broker, running as root, gets.
//
// A coupling to gVisor's default, deliberately: neither the launch nor the
// delete passes --root, so reading the same env var gVisor reads is what
// keeps the boot sweep pointed at the same directory the runs use. If a
// future gVisor moved its default, the sweep degrades to finding nothing —
// the pre-launch delete still clears state for every id actually relaunched.
func runscStateRoot() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "runsc")
	}
	return "/var/run/runsc"
}

// runtimeStateExtensions are the suffixes runsc appends to a container's
// entries under its state root. Trimmed by name rather than with
// filepath.Ext so a container id that itself contains a dot isn't cut short.
var runtimeStateExtensions = []string{".state", ".lock", ".json"}

// tfContainerIDRE matches exactly the container ids this package mints:
// tf-<run id fragment>-<subnet index>, where the fragment is a RunID cut to
// containerIDRunFragmentMax and the index is one byte in decimal. Strict for
// the same reason the netns reaper's regex is — the sweep deletes what it
// matches, and the state root is shared with whatever else on the host runs
// runsc, some of which may well use tf-shaped ids of its own.
//
// Built from the mint's own bound rather than a hand-picked ceiling: a
// matcher wider than what Wrap can produce is reach the sweep has no use
// for, and a matcher narrower than it would strand our own state. Erring
// wide is the worse direction — a missed entry is a leak the next launch of
// that id still clears, while an over-match deletes a container we do not
// own.
var tfContainerIDRE = regexp.MustCompile(fmt.Sprintf(`^tf-[A-Za-z0-9._-]{1,%d}-\d{1,3}$`, containerIDRunFragmentMax))

// containerIDFromRuntimeStateEntry recovers the container id from one entry
// name under runsc's state root, or reports that the entry isn't ours.
//
// Two layouts, because the on-disk shape has changed across gVisor releases
// and the sweep should not care which one this host writes: a file named
// "<container id>_sandbox:<sandbox id>.<ext>", or a directory named for the
// container id alone. Both reduce to the id.
func containerIDFromRuntimeStateEntry(name string) (string, bool) {
	id := name
	if i := strings.Index(id, "_sandbox:"); i >= 0 {
		id = id[:i]
	}
	for _, ext := range runtimeStateExtensions {
		if trimmed, ok := strings.CutSuffix(id, ext); ok {
			id = trimmed
			break
		}
	}
	if !tfContainerIDRE.MatchString(id) {
		return "", false
	}
	return id, true
}

// reapOrphanRuntimeState delete-forces every tf-* container runsc still
// holds state for. Boot-only, alongside the netns / cgroup / bundle sweeps,
// and the one death mode the pre-launch delete can't reach: a crashed
// executor's leftovers are keyed to subnet indices the next boot allocates
// in a different order, so nothing would ever collide with them and
// therefore nothing would ever clean them.
func reapOrphanRuntimeState(ctx context.Context) {
	root := runscStateRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		// No state root at all — runsc has never run on this host.
		return
	}
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		id, ok := containerIDFromRuntimeStateEntry(e.Name())
		if !ok || seen[id] {
			// One container can own several entries (state + lock);
			// delete-force once per id.
			continue
		}
		seen[id] = true

		sandboxLog.Info("reaping orphan runsc container state", "container", id)
		if err := DeleteRuntimeState(ctx, id); err != nil {
			sandboxLog.Warn("reap runsc container state failed", "container", id, "error", err)
		}
	}
}
