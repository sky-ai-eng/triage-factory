//go:build linux

package capbroker

import (
	"fmt"
	"net"
	"os"
)

// socketDir is the shared parent directory for TF's local unix sockets —
// the same directory cmd/exec/agenthost uses for its per-run sockets.
// Reusing it is fine: MkdirAll is idempotent and both are owner-only
// (0700) directories created by the same process.
const socketDir = "/run/tf"

// DefaultSocketPath is the broker's fixed socket path. Unlike agenthost's
// per-run socket, there is exactly one cap-broker per executor host —
// "one broker process per executor (long-lived), spawned once at boot...
// not one per run" — so the path is fixed rather than keyed by run id.
const DefaultSocketPath = socketDir + "/cap-broker.sock"

// listen creates the broker's socket following the same anti-race
// ordering as agenthost/socket_linux.go's Start:
//
//  1. MkdirAll 0700 — closes the race window between socket-create and
//     chmod; no other host user can enumerate /run/tf for a
//     not-yet-restricted socket.
//  2. net.Listen("unix", ...) — creates the socket file under the
//     process umask (typically too permissive on its own; step 1 covers
//     the gap, step 3 closes it for good).
//  3. Chmod 0600 — owner-only. Unlike agenthost's per-run socket, this
//     one is NEVER chowned to the sandbox UID and never bind-mounted into
//     a sandbox — "the broker socket is host-only." Only the orchestrator
//     process (same user that started the broker) can reach it.
func listen(socketPath string) (net.Listener, error) {
	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		return nil, fmt.Errorf("capbroker: mkdir %s: %w", socketDir, err)
	}
	// Remove any stale socket file from a previous crash — net.Listen
	// would otherwise EADDRINUSE on a path nothing is listening on.
	_ = os.Remove(socketPath)

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
