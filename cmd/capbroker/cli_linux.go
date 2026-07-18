//go:build linux

package capbroker

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/capinfo"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/procname"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// shutdownDrain bounds how long runBroker waits for in-flight RPCs to
// finish once a shutdown signal arrives.
const shutdownDrain = 5 * time.Second

var brokerLog = logging.Component("cap-broker")

// runBroker is the `triagefactory cap-broker` subcommand body: create the
// socket, serve sandbox.NewHostOps() over it — "executing the same
// hostOps implementation from P0" — until SIGTERM/SIGINT, then drain and
// clean up. Blocks for the process lifetime.
//
// In the container path, docker/entrypoint.sh spawns this process
// directly, before it execs the capability-dropped orchestrator into
// existence — the "container entrypoint holds caps only long enough to
// spawn the cap-broker" half of the exec-time capability drop. The
// dev/bare-metal fallback path (orchestrator_linux.go's Start, when
// nothing is already listening on the socket) still spawns it as a plain
// child too.
func runBroker(args []string) error {
	fs := flag.NewFlagSet("cap-broker", flag.ContinueOnError)
	socketPath := fs.String("socket", DefaultSocketPath, "unix socket path to serve on")
	orchestratorUID := fs.Int("orchestrator-uid", -1, "uid the orchestrator process runs as after the exec-time capability drop; when set (>=0), the control socket is chowned to this uid so that different-uid, unprivileged process can still connect")
	orchestratorGID := fs.Int("orchestrator-gid", -1, "gid the orchestrator process runs as; when set (>=0), run trees are chowned to this group so the orchestrator can materialize checkouts into a run root handed to the sandbox agent, WITHOUT granting WorktreeGID (which a run's sidecar also carries and could otherwise use to reach other runs' trees)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if err := validateOrchestratorUIDFlag(*orchestratorUID); err != nil {
		return err
	}
	if err := validateOrchestratorGIDFlag(*orchestratorGID); err != nil {
		return err
	}

	procname.SetTitle("tf-cap-broker")
	logging.SetProcess("cap-broker")

	l, err := listen(*socketPath)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(*socketPath) }()

	if *orchestratorUID >= 0 {
		// Accept the orchestrator's uid as a legitimate run-tree owner for
		// the ChownRunTree/RemoveRunTree/CaptureRunDelta boundary
		// validation: this process (root) serves those ops against trees
		// the orchestrator created.
		sandbox.SetRunTreeOwnerUID(*orchestratorUID)

		// Chown run trees to the orchestrator's own gid (not WorktreeGID) at
		// the ownership hand-off, so it can materialize checkouts into a run
		// root it has handed to the agent without opening that tree to the
		// per-run sidecar. See sandbox.SetRunTreeGroupGID.
		if *orchestratorGID >= 0 {
			sandbox.SetRunTreeGroupGID(*orchestratorGID)
		}

		// The socket is created owner-only (listen()'s chmod 0600) by
		// whichever uid this process runs as — root, in the default
		// deployment. Once the orchestrator drops to a different,
		// non-root uid (the whole point of the exec-time drop), it can
		// no longer reach a root-owned 0600 socket, so ownership moves
		// to the orchestrator's uid instead. This process (root, via
		// CAP_DAC_OVERRIDE) can still reach it regardless of file
		// ownership, so both sides keep working; no other uid can.
		if err := os.Chown(*socketPath, *orchestratorUID, -1); err != nil {
			return fmt.Errorf("capbroker: chown socket to orchestrator uid %d: %w", *orchestratorUID, err)
		}

		// Hand the socket DIRECTORY to the orchestrator (owner) so it can
		// still reach cap-broker.sock post-drop, but keep it group-owned by
		// WorktreeGID with group write: each run's credential sidecar —
		// unprivileged, a WorktreeGID member — creates its own per-run
		// agenthost socket here, so it needs create (w) + search (x) on the
		// dir. Hence orchestratorUID:WorktreeGID mode 01730 — owner rwx, group
		// wx, no other access, STICKY. The missing read bit preserves the
		// anti-enumeration property (no non-owner can readdir to discover live
		// runs' socket names); the sticky bit is what stops one run's sidecar
		// deleting or replacing cap-broker.sock or a sibling run's socket, since
		// group-write + shared WorktreeGID would otherwise grant unlink/rename
		// authority over every entry (see listen's doc). This process (root,
		// DAC-override) reaches it regardless.
		dir := filepath.Dir(*socketPath)
		if err := os.Chown(dir, *orchestratorUID, sandbox.WorktreeGID); err != nil {
			return fmt.Errorf("capbroker: chown socket dir to orchestrator uid %d gid %d: %w", *orchestratorUID, sandbox.WorktreeGID, err)
		}
		if err := os.Chmod(dir, os.ModeSticky|0o730); err != nil {
			return fmt.Errorf("capbroker: chmod socket dir: %w", err)
		}
	}

	logBootLine(*socketPath)

	srv := NewServer(sandbox.NewHostOps())

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(l) }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case <-ctx.Done():
		_ = l.Close()
		drainCtx, cancel := context.WithTimeout(context.Background(), shutdownDrain)
		defer cancel()
		if err := srv.Shutdown(drainCtx); err != nil {
			return fmt.Errorf("capbroker: shutdown: %w", err)
		}
		<-serveErr
		return nil
	case err := <-serveErr:
		// Accept loop died on its own (not via our shutdown) — surface it.
		return err
	}
}

// validateOrchestratorUIDFlag rejects an --orchestrator-uid inside the
// reserved sidecar uid band. sandbox.SidecarUIDBase's own init() assertion
// only pins the DEFAULT orchestrator uid (10001) as disjoint from that
// band — it can't see this operator-supplied flag. A collision would let
// the orchestrator and some run's sidecar share a uid, which defeats the
// isolation the sidecar's distinct-uid-per-run design relies on entirely
// (same-uid processes can signal each other and, subject to the host's
// Yama ptrace_scope, ptrace each other) — so refuse to start rather than
// silently booting into that. This is the earliest point the actual
// configured value is known; the orchestrator itself repeats the
// equivalent check against its own post-drop os.Getuid() as the
// authoritative backstop (see internal/app/privsep.go), since this flag
// and the drop that setpriv performs are configured independently and
// could, in principle, drift. A negative uid (the flag's default, meaning
// "unset") always passes — this is a validation of a supplied value, not
// a requirement that one be supplied.
func validateOrchestratorUIDFlag(uid int) error {
	if uid >= 0 && sandbox.IsSidecarUID(uid) {
		return fmt.Errorf("capbroker: --orchestrator-uid %d falls inside the reserved sidecar uid band [%d, %d) — the orchestrator and a run's sidecar would share a uid; repoint TF_ORCHESTRATOR_UID outside that range", uid, sandbox.SidecarUIDBase, sandbox.SidecarUIDBase+sandbox.MaxSandboxes)
	}
	return nil
}

// validateOrchestratorGIDFlag refuses a run-tree group that would defeat the
// isolation the group is chosen FOR. The orchestrator's gid becomes the run
// tree's group, so it must be a group no sandboxed principal carries:
// WorktreeGID is carried by every jailed agent AND every per-run sidecar
// (shared over the executor filesystem), so using it would let any run's
// sidecar reach any other run's tree; the sidecar uid band is a sidecar's own
// gid. Either would silently convert the group-write grant into a cross-run
// hole, so the broker refuses to boot rather than serve it.
func validateOrchestratorGIDFlag(gid int) error {
	if gid < 0 {
		return nil
	}
	if gid == sandbox.WorktreeGID {
		return fmt.Errorf("capbroker: --orchestrator-gid %d is WorktreeGID — run trees would become group-writable by every run's sidecar (a WorktreeGID member); repoint TF_ORCHESTRATOR_GID to a group no sandboxed principal carries", gid)
	}
	if sandbox.IsSidecarUID(gid) {
		return fmt.Errorf("capbroker: --orchestrator-gid %d falls inside the reserved sidecar band [%d, %d); repoint TF_ORCHESTRATOR_GID outside that range", gid, sandbox.SidecarUIDBase, sandbox.SidecarUIDBase+sandbox.MaxSandboxes)
	}
	return nil
}

// logBootLine emits one legibility boot line: this process's uid and
// effective capability set, so a security reviewer can turn "trust the
// code" into "read the logs" — cross-check against `ps` /
// `/proc/<pid>/status` rather than trusting that the split is real.
func logBootLine(socketPath string) {
	names, err := capinfo.Effective()
	if err != nil {
		brokerLog.Warn("boot: failed to read own effective capabilities", "error", err)
		return
	}
	brokerLog.Info("boot",
		"uid", os.Getuid(),
		"CapEff", capinfo.Describe(names),
		"socket", socketPath,
	)
}
