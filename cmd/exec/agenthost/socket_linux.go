//go:build linux

package agenthost

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// hostSocketRoot is the per-host directory that holds the per-run
// unix sockets. Created with mode 0700 the first time a run needs it
// — owner-only access defends against a hostile second tenant
// scanning /run for sockets to dial while a sandbox is mid-construction
// (between net.Listen and chown/chmod).
//
// `/run/` requires root to create on most distros. When this daemon's
// process runs as root (the dev/bare-metal case where the binary is run
// directly, no exec-time drop) it creates it itself; in the container
// deployment the cap-broker (root) creates this same shared directory for
// its own control socket and hands its ownership to the orchestrator's uid
// at boot (cmd/capbroker's runBroker), so this process — unprivileged
// after the exec-time capability drop — finds it already writable-by-owner.
// The two arrangements are why the MkdirAll below tolerates the directory
// already existing with either owner.
const hostSocketRoot = "/run/tf"

// HostDaemon is the per-run socket + server lifecycle the spawner
// owns. Start() creates the socket, chowns it to the sandbox UID,
// spawns the daemon goroutine, and returns. Close() unblocks the
// accept loop, drains in-flight handlers, and removes the socket
// file.
//
// One HostDaemon per delegated run. The spawner's deferred-cleanup
// chain looks like:
//
//	hd, mount, err := Start(stores, info, bundleFunc)
//	if err != nil { ... }
//	defer hd.Close()
//	// add `mount` to sandbox.Config.ExtraMounts; sb.Wrap; cmd.Wait
type HostDaemon struct {
	listener net.Listener
	sockPath string
	server   *Server
	doneCh   chan struct{}
}

// Start creates the per-run unix socket, grants the sandbox identity
// access to it, and spawns the daemon goroutine. The agent in the
// sandbox sees the socket at /run/tf.sock by way of the returned bind
// mount.
//
// Property B / fs-permissions invariant (load-bearing):
//
//  1. Parent dir mkdir 0700 — any race window between socket-create
//     and the grant is unreachable from a second tenant (the dir is
//     owner-only). Without this, an attacker on the host could
//     enumerate /run for sockets that haven't been chmod'd yet.
//
//  2. net.Listen("unix", ...) — creates the socket file with the
//     process umask applied (typically 0666 or 0755); either is too
//     permissive on its own, but step 1 locks the parent so nobody
//     can reach it yet.
//
//  3. The sandbox-identity grant, shaped by what this process may do:
//     running as root (dev/bare-metal, no exec-time drop), chown the
//     socket to sandbox.WorktreeUID and chmod 0600 — owner-only RW,
//     exactly the original arrangement. Running unprivileged (the
//     container default: the orchestrator post-exec-drop holds no
//     CAP_CHOWN and can never give a file away to another uid), keep
//     ownership and instead
//     chgrp the socket to sandbox.WorktreeGID + chmod 0660 — an
//     owner-legal group grant, possible because the container image
//     makes the orchestrator user a member of the sandbox group
//     (tf-sandbox, gid 10000) and the entrypoint's setpriv carries
//     exactly that supplementary group through the drop. The connect
//     check passes for the sandbox process the same way (uid 10000
//     runs with gid 10000); every other unprivileged host uid is
//     still excluded by the missing other-bits and the 0700/0-search
//     parent directory.
//
// Caller MUST invoke .Close() exactly once (typically via defer)
// regardless of how the surrounding sandbox terminates — the listener
// close drains the daemon goroutine, the os.Remove cleans up the
// socket file.
//
// proxyCreds points the daemon's gh/jira verbs at the run's credential-sidecar
// REST proxies (non-nil only on TF_ROLE=executor — the spawner passes nil on
// TF_ROLE=all and local mode); threaded straight into NewServer so the verbs
// hold only per-run placeholders and the sidecar injects the real credential
// upstream, instead of reading the (disabled, on an executor) secret store.
// See Server.proxyCreds.
func Start(stores db.Stores, info RunInfo, proxyCreds *ProxyCredentials) (*HostDaemon, sandbox.Mount, error) {
	if info.RunID == "" {
		return nil, sandbox.Mount{}, fmt.Errorf("agenthost: RunInfo.RunID required")
	}
	return StartWithServer(NewServer(stores, info, proxyCreds), info.RunID)
}

// StartWithServer creates the per-run socket for an already-built Server, grants
// it to the sandbox identity, spawns the accept loop, and returns the bind mount
// the agent reaches it through. This is the seam the relocation turns on: the
// executor's credential sidecar builds a relay-backed Server (NewServerWithRuntime)
// and calls this so the hostile-input verb parser runs INSIDE the capless per-run
// jail rather than the orchestrator; Start (above) calls it with a stores-backed
// Server for all/local and the orchestrator's own runs. The socket path + the
// chgrp+0660 grant are identical either way — only the process that owns them
// moves. On the sidecar the grant works because the sidecar is launched as a
// member of the sandbox group (see internal/sandbox's sidecar launch).
func StartWithServer(server *Server, runID string) (*HostDaemon, sandbox.Mount, error) {
	if runID == "" {
		return nil, sandbox.Mount{}, fmt.Errorf("agenthost: RunID required")
	}
	if err := os.MkdirAll(hostSocketRoot, 0o700); err != nil {
		return nil, sandbox.Mount{}, fmt.Errorf("agenthost: mkdir %s: %w", hostSocketRoot, err)
	}
	sockPath := filepath.Join(hostSocketRoot, sanitizeSocketName(runID)+".sock")

	// Remove any stale socket file from a previous crash. net.Listen
	// would otherwise EADDRINUSE on a path that's actually unused.
	// Stale files in /run/tf/ are by definition from a previous TF
	// process — the only writer is this codepath — so removal is safe.
	_ = os.Remove(sockPath)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, sandbox.Mount{}, fmt.Errorf("agenthost: listen %s: %w", sockPath, err)
	}
	if err := grantSocketToSandbox(sockPath); err != nil {
		_ = listener.Close()
		_ = os.Remove(sockPath)
		return nil, sandbox.Mount{}, err
	}

	hd := &HostDaemon{
		listener: listener,
		sockPath: sockPath,
		server:   server,
		doneCh:   make(chan struct{}),
	}
	go func() {
		defer close(hd.doneCh)
		if err := server.Serve(listener); err != nil {
			agenthostLog.Error("daemon serve failed", "error", err)
		}
	}()

	mount := sandbox.Mount{
		Source:      sockPath,
		Destination: DefaultSocketPath,
		// rw is the default; the sandbox needs to both connect (read
		// the daemon's response) and send requests (write to the
		// socket). No `ro` here.
	}
	return hd, mount, nil
}

// grantSocketToSandbox makes the freshly-listened socket connectable by
// the sandbox identity (uid/gid 10000) — step 3 of Start's invariant.
// Two shapes, by what this process is allowed to do:
//
//   - root (dev/bare-metal, no exec-time drop): chown to WorktreeUID +
//     chmod 0600. Byte-identical to the original arrangement.
//   - unprivileged (the container-default post-exec-drop orchestrator,
//     which holds no CAP_CHOWN and cannot give a file away to another
//     uid): chgrp to WorktreeGID + chmod 0660. An owner-legal group
//     change — valid only
//     because this process is a member of the sandbox group (the image's
//     tf-sandbox, gid 10000, carried through the drop by the
//     entrypoint's setpriv --groups) — so a clear, actionable error
//     names that requirement when the chgrp is refused.
func grantSocketToSandbox(sockPath string) error {
	if os.Getuid() == 0 {
		if err := os.Chown(sockPath, sandbox.WorktreeUID, sandbox.WorktreeGID); err != nil {
			return fmt.Errorf("agenthost: chown %s to uid=%d: %w", sockPath, sandbox.WorktreeUID, err)
		}
		if err := os.Chmod(sockPath, 0o600); err != nil {
			return fmt.Errorf("agenthost: chmod %s: %w", sockPath, err)
		}
		return nil
	}
	if err := os.Chown(sockPath, -1, sandbox.WorktreeGID); err != nil {
		return fmt.Errorf("agenthost: chgrp %s to gid=%d: %w (an unprivileged orchestrator must be a member of the sandbox group — the provided image's tf-sandbox — for this owner-legal group grant)", sockPath, sandbox.WorktreeGID, err)
	}
	if err := os.Chmod(sockPath, 0o660); err != nil {
		return fmt.Errorf("agenthost: chmod %s: %w", sockPath, err)
	}
	return nil
}

// Close shuts down the daemon goroutine and removes the socket file.
// Idempotent. Order matters — close the listener first to unblock
// Serve's accept loop, then wait briefly for in-flight handlers to
// drain, then remove the file so a post-crash startup scan doesn't
// see a dangling socket pointing at a dead inode.
func (h *HostDaemon) Close() error {
	if h == nil {
		return nil
	}
	// Stop accepting and wait for the accept loop to exit. The
	// listener close interrupts Accept; the daemon goroutine notices
	// net.ErrClosed and returns.
	_ = h.listener.Close()
	select {
	case <-h.doneCh:
	case <-time.After(2 * time.Second):
		agenthostLog.Warn("daemon accept-loop drain timed out", "sock", h.sockPath)
	}
	// Drain in-flight handlers up to a bounded budget. RPCs that are
	// mid-DB-write commit on the host process and don't need network
	// access; the timeout just caps how long we wait for them to
	// release the conn and complete their last write.
	drainCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.server.Shutdown(drainCtx); err != nil {
		agenthostLog.Warn("daemon shutdown drain failed", "error", err)
	}
	_ = os.Remove(h.sockPath)
	return nil
}

// SocketPath returns the host-side path of the per-run socket. Useful
// for tests that need to point an IPCClient at the daemon without
// going through the bind-mount-into-sandbox shape.
func (h *HostDaemon) SocketPath() string { return h.sockPath }

// SocketPathFor returns the host-side path of runID's agenthost socket.
// Deterministic so the sidecar (which creates + serves it on the executor
// path) and the orchestrator (which bind-mounts it into the jail) agree on the
// path without a round trip.
func SocketPathFor(runID string) string {
	return filepath.Join(hostSocketRoot, sanitizeSocketName(runID)+".sock")
}

// SocketMountFor returns the bind mount exposing runID's socket to the agent at
// DefaultSocketPath. On the executor path the SIDECAR creates + serves the
// socket, so the orchestrator adds THIS mount to the sandbox spec without
// starting a Server of its own — the socket already exists (the sidecar bound
// it during bring-up, before the sandbox launches).
func SocketMountFor(runID string) sandbox.Mount {
	return sandbox.Mount{Source: SocketPathFor(runID), Destination: DefaultSocketPath}
}

// sanitizeSocketName trims a run id to a fs-safe form. RunIDs are
// already UUIDs in production callers so this is a defense-in-depth
// guard against future callers using a less-constrained shape (the
// integration test uses "itest..." strings, for instance).
func sanitizeSocketName(s string) string {
	r := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-' || r == '_':
			return r
		default:
			return '-'
		}
	}, s)
	if len(r) > 64 {
		r = r[:64]
	}
	return r
}
