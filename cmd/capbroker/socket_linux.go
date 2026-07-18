//go:build linux

package capbroker

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// socketDir is the shared parent directory for TF's local unix sockets —
// the same directory cmd/exec/agenthost uses for its per-run sockets.
// Reusing it is fine: MkdirAll is idempotent, and listen's own explicit
// chown+chmod (see below) pin the directory's final owner/mode regardless
// of which package's MkdirAll call happens to create it first —
// agenthost's own request for 0700 there is a no-op once this directory
// already exists.
const socketDir = "/run/tf"

// DefaultSocketPath is the broker's fixed socket path. Unlike agenthost's
// per-run socket, there is exactly one cap-broker per executor host —
// "one broker process per executor (long-lived), spawned once at boot...
// not one per run" — so the path is fixed rather than keyed by run id.
const DefaultSocketPath = socketDir + "/cap-broker.sock"

// listen creates the broker's socket following the same anti-race
// ordering as agenthost/socket_linux.go's Start:
//
//  1. MkdirAll + chgrp + Chmod 01731 on the socket's parent directory — see
//     the chown/chmod calls below for why 01731 (root:WorktreeGID, sticky)
//     rather than agenthost's 0700. Takes the parent of socketPath (not the socketDir
//     constant directly) so tests can point listen at an isolated temp
//     directory; production always passes DefaultSocketPath, whose parent is
//     exactly socketDir.
//  2. net.Listen("unix", ...) — creates the socket file under the
//     process umask (typically too permissive on its own; step 1 covers
//     the gap, step 3 closes it for good).
//  3. Chmod 0600 — owner-only. Unlike agenthost's per-run socket, this
//     one is NEVER chowned to the sandbox UID and never bind-mounted into
//     a sandbox — "the broker socket is host-only." Its owner starts as
//     whichever uid ran listen (root, in production) and, when the
//     caller passes --orchestrator-uid, runBroker chowns it afterward to
//     the orchestrator's (different, unprivileged) uid so that process
//     can still reach it post-drop; the broker itself keeps reaching it
//     regardless of file ownership via CAP_DAC_OVERRIDE. No other uid
//     can reach it either way.
func listen(socketPath string) (net.Listener, error) {
	dir := filepath.Dir(socketPath)
	if err := os.MkdirAll(dir, os.ModeSticky|0o731); err != nil {
		return nil, fmt.Errorf("capbroker: mkdir %s: %w", dir, err)
	}
	// This dir is shared: the broker's own socket lives here, and each run's
	// credential sidecar — unprivileged, carrying WorktreeGID as a supplementary
	// group — creates its per-run agenthost socket here too. Creating a file
	// needs write+search on the dir, so the sidecar needs group w; hence
	// root:WorktreeGID mode 01731. Group is wx (create + traverse) with no r,
	// and other is x only, so no non-owner can readdir to enumerate live runs'
	// socket names — the anti-enumeration property the prior 0711 had, extended
	// just far enough to let the sidecar write. Other keeps x so the (different,
	// unprivileged) orchestrator can still traverse to reach cap-broker.sock.
	//
	// The STICKY bit (the leading 1) is load-bearing, not hygiene: group-write
	// makes the directory writable by every run's sidecar, and WorktreeGID is
	// shared across ALL runs — so without it, POSIX unlink/rename authority (a
	// property of the PARENT directory, not the entry) would let any one run's
	// sidecar delete or replace cap-broker.sock (a fixed path — a broker DoS) or
	// a sibling run's <runID>.sock before that run's daemon binds (cross-run
	// interference). Sticky restricts unlink/rename to each entry's own owner:
	// per-run sockets are owned by their run's distinct sidecar uid and the
	// broker's by root, so each is protected from every other run. Mirrors /tmp's
	// 1777. Explicit chown+chmod, not the create mode: MkdirAll is a no-op on an
	// existing dir (agenthost's StartWithServer also MkdirAlls this path), so the
	// final owner/mode is pinned regardless of who created it first. The chgrp
	// needs privilege the real (root) broker holds. An unprivileged caller — a
	// test, or a broker misconfigured without root, which then can't hold
	// CAP_SYS_ADMIN and can't function anyway — gets EPERM; tolerate that (the dir
	// stays owner-writable, enough for that caller's own socket) but surface any
	// other chown error.
	if err := os.Chown(dir, -1, sandbox.WorktreeGID); err != nil && !errors.Is(err, os.ErrPermission) {
		return nil, fmt.Errorf("capbroker: chgrp %s to gid=%d: %w", dir, sandbox.WorktreeGID, err)
	}
	if err := os.Chmod(dir, os.ModeSticky|0o731); err != nil {
		return nil, fmt.Errorf("capbroker: chmod %s: %w", dir, err)
	}
	// Remove a stale socket file left by a previous crash — net.Listen
	// would otherwise EADDRINUSE on a path nothing is listening on. Only
	// ever removes an actual unix socket: --socket is operator/test
	// supplied, so a misconfigured path pointing at an unrelated file
	// must error rather than silently delete it.
	if info, err := os.Lstat(socketPath); err == nil {
		if info.Mode().Type() != os.ModeSocket {
			return nil, fmt.Errorf("capbroker: %s exists and is not a socket; refusing to remove it", socketPath)
		}
		if err := os.Remove(socketPath); err != nil {
			return nil, fmt.Errorf("capbroker: remove stale socket %s: %w", socketPath, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("capbroker: stat %s: %w", socketPath, err)
	}

	l, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("capbroker: listen %s: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = l.Close()
		_ = os.Remove(socketPath)
		return nil, fmt.Errorf("capbroker: chmod %s: %w", socketPath, err)
	}
	return l, nil
}
