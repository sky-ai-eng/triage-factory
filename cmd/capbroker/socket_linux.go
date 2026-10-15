//go:build linux

package capbroker

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// socketDir is the shared parent directory for TF's local unix sockets —
// the same directory cmd/exec/agenthost's per-run socket directories live
// under. listen's explicit chown+chmod (see below) pin the directory's final
// owner/mode regardless of which process's MkdirAll happens to create it
// first.
const socketDir = "/run/tf"

// DefaultSocketPath is the broker's fixed socket path. Unlike agenthost's
// per-run socket, there is exactly one cap-broker per executor host —
// "one broker process per executor (long-lived), spawned once at boot...
// not one per run" — so the path is fixed rather than keyed by run id.
const DefaultSocketPath = socketDir + "/cap-broker.sock"

// socketDirMode is the socket directory's mode: owner-writable, traversable by
// everyone, listable and writable by nobody else. See listen.
const socketDirMode = 0o711

// listen creates the broker's socket following the same anti-race
// ordering as agenthost/socket_linux.go's Start:
//
//  1. MkdirAll + chgrp + Chmod 0711 on the socket's parent directory — see
//     the chown/chmod calls below for why. Takes the parent of socketPath
//     (not the socketDir constant directly) so tests can point listen at an
//     isolated temp directory; production always passes DefaultSocketPath,
//     whose parent is exactly socketDir.
//  2. net.Listen("unix", ...) — creates the socket file under the
//     process umask (typically too permissive on its own; step 1 covers
//     the gap, step 3 closes it for good).
//  3. Chmod 0600 — owner-only. Unlike agenthost's per-run socket, this
//     one is NEVER granted to a jail and never bind-mounted into a sandbox
//     — "the broker socket is host-only." Its owner starts as whichever
//     uid ran listen (root, in production) and, when the caller passes
//     --orchestrator-uid, runBroker chowns it afterward to the
//     orchestrator's (different, unprivileged) uid so that process can
//     still reach it post-drop; the broker itself keeps reaching it
//     regardless of file ownership via CAP_DAC_OVERRIDE. No other uid can
//     reach it either way.
func listen(socketPath string) (net.Listener, error) {
	dir := filepath.Dir(socketPath)
	if err := os.MkdirAll(dir, socketDirMode); err != nil {
		return nil, fmt.Errorf("capbroker: mkdir %s: %w", dir, err)
	}
	// The directory is shared: the broker's own socket lives here, the
	// orchestrator's per-launch stdio sockets live here, and each run's two
	// socket directories live here. Nothing sandboxed may create an entry in
	// it — a run's sidecar creates its sockets inside its own directory, which
	// this process makes for it (PrepareRunSockets) — so the directory is
	// 0711: its owner writes, and everyone else may only traverse to an entry
	// they already know the name of. No read for others keeps live runs' names
	// unlistable; no group write is what stops a sidecar or a jail identity on
	// the host from creating, unlinking or renaming anything here.
	//
	// The group is reset to this process's own explicitly, not left to the
	// create: MkdirAll is a no-op on an existing directory, and one that
	// survived from an earlier deployment carries whatever group it had then.
	// An unprivileged caller — a test, or a broker misconfigured without root,
	// which then can't hold CAP_SYS_ADMIN and can't function anyway — chgrps
	// to its own gid, which is always permitted.
	if err := os.Chown(dir, -1, os.Getgid()); err != nil && !errors.Is(err, os.ErrPermission) {
		return nil, fmt.Errorf("capbroker: chgrp %s to gid=%d: %w", dir, os.Getgid(), err)
	}
	if err := os.Chmod(dir, socketDirMode); err != nil {
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
