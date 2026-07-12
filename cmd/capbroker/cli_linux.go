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
	if err := fs.Parse(args); err != nil {
		return err
	}

	if err := validateOrchestratorUIDFlag(*orchestratorUID); err != nil {
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

		// Hand the socket DIRECTORY to the orchestrator too, tightened to
		// 0700: the same /run/tf holds the agenthost daemon's per-run
		// sockets, which the (now unprivileged) orchestrator creates
		// itself and so needs write+search here for. Owner-only is
		// strictly tighter than listen()'s 0711 default — the only two
		// parties that belong in this directory are the orchestrator
		// (owner) and this process (root, DAC-override); the 0711
		// traverse-by-path concession existed for the orchestrator
		// reaching a root-owned dir, which ownership now makes moot.
		dir := filepath.Dir(*socketPath)
		if err := os.Chown(dir, *orchestratorUID, -1); err != nil {
			return fmt.Errorf("capbroker: chown socket dir to orchestrator uid %d: %w", *orchestratorUID, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
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
