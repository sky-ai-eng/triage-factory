//go:build linux

package agenthost

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// hostSocketRoot is the per-host directory that holds every run's socket
// directory. The cap-broker creates it at boot and keeps it writable by its
// owner alone (cmd/capbroker's listen), and creates each run's directory
// inside it for each engagement (sandbox.PrepareRunSockets): owned by the
// run's sidecar uid, with the run's jail gid as its group and no access for
// anyone else. A sidecar therefore creates its socket and CA bundle in the one
// directory that is its own, and in nothing shared.
const hostSocketRoot = "/run/tf"

// agentHostSocketName and caBundleName are the files a run's sidecar creates
// in its socket directory. internal/sandbox derives the same paths
// (TrustedAgentHostSocketPath, TrustedCABundlePath) to validate the launch's
// mounts; a drift test cross-checks the two derivations.
const (
	agentHostSocketName = "agenthost.sock"
	caBundleName        = "ca-bundle.crt"
)

// HostDaemon is the per-run socket + server lifecycle. StartWithServer
// creates the socket, grants it to the run's jail gid, spawns the daemon
// goroutine, and returns. Close() unblocks the accept loop, drains in-flight
// handlers, and removes the socket file.
//
// One HostDaemon per delegated run, served from the run's credential sidecar:
//
//	hd, mount, err := StartWithServer(srv, conversationID, jailGID)
//	if err != nil { ... }
//	defer hd.Close()
//	// the orchestrator adds `mount` (SocketMountFor) to the jail's mounts
type HostDaemon struct {
	listener net.Listener
	sockPath string
	server   *Server
	doneCh   chan struct{}
}

// StartWithServer creates the per-run socket for an already-built Server,
// grants it to the run's jail, spawns the accept loop, and returns the bind
// mount the agent reaches it through. The executor's credential sidecar builds
// a relay-backed Server (NewServerWithRuntime) and calls this, so the
// hostile-input verb parser runs inside the capless per-run process rather than
// the orchestrator.
//
// jailGID is the run's jail gid — the gid the run's jail runs with, which the
// sidecar derives from its own uid (sandbox.SidecarJailGID) without carrying
// it. The socket is granted to it and to nothing else.
//
// Property B / fs-permissions invariant (load-bearing):
//
//  1. The run's socket directory exists before this runs and admits only the
//     sidecar and the run's jail gid, so the window between net.Listen and the
//     grant is unreachable from any other run or host process. It is setgid
//     to the jail gid, so the socket is created in that group.
//
//  2. net.Listen("unix", ...) — creates the socket file with the process
//     umask applied (typically 0755); too permissive on its own, but step 1
//     covers the gap.
//
//  3. chmod 0660, once the socket is confirmed to be in jailGID's group. The
//     jail's connect passes on the group bits; no other uid on the host has a
//     way in.
//
// Caller MUST invoke .Close() exactly once (typically via defer) regardless of
// how the surrounding sandbox terminates — the listener close drains the
// daemon goroutine, the os.Remove cleans up the socket file.
func StartWithServer(server *Server, conversationID string, jailGID int) (*HostDaemon, sandbox.Mount, error) {
	if conversationID == "" {
		return nil, sandbox.Mount{}, fmt.Errorf("agenthost: ConversationID required")
	}
	hd, err := listenAndServe(server, SocketPathFor(conversationID), func(sockPath string) error {
		return grantToJail(sockPath, jailGID, 0o660)
	})
	if err != nil {
		return nil, sandbox.Mount{}, err
	}
	mount := sandbox.Mount{
		Source:      hd.sockPath,
		Destination: DefaultSocketPath,
		// rw is the default; the sandbox needs to both connect (read
		// the daemon's response) and send requests (write to the
		// socket). No `ro` here.
	}
	return hd, mount, nil
}

// StartLocal starts the per-run daemon for a LOCAL sandboxed run: a
// bubblewrap mount namespace rather than a gVisor jail, on a socket under the
// state root rather than under /run/tf.
//
// Two things differ from the jail's socket, and both follow from there being
// only one identity here. The path differs because /run/tf is created and
// owned by the cap-broker, which a local unprivileged process has no
// counterpart to — paths.AgentHostSocketPath puts it in the state root this
// process already owns. The grant differs because the agent inside the local
// namespace runs as THIS uid, so there is no second identity to hand the
// socket to: owner-only 0600 is both the tightest and the only correct mode,
// where a chown to the sandbox uid would fail and a group grant would widen
// access to no one who needs it.
//
// Downstream of the socket the agent's exec verbs work as they do in the
// jail: they dial it as a pure RPC client, hold no database handle and no
// credential, and resolve their identity from the daemon's own
// LookupConversation. Two answers differ (see Server.sharesNamespace): the
// agent sees the run root at its host path rather than /work, and a
// `workspace add` checkout is already the agent's, so it is not handed to the
// sandbox uid.
//
// signing is what the run's commits are signed with, nil for a run that does
// not sign. Its daemon signs in this process, with the key from the secret
// store.
//
// The caller supplies the path (from internal/paths) rather than this package
// deriving it, so the ".triagefactory" literal stays where the lint guard
// keeps it. Caller MUST invoke .Close() exactly once, as with Start.
func StartLocal(stores db.Stores, info ConversationInfo, sockPath string, signing *LocalCommitSigning) (*HostDaemon, error) {
	if info.ConversationID == "" {
		return nil, fmt.Errorf("agenthost: ConversationInfo.ConversationID required")
	}
	if sockPath == "" {
		return nil, fmt.Errorf("agenthost: socket path required")
	}
	if err := ensureOwnerOnlyDir(filepath.Dir(sockPath)); err != nil {
		return nil, err
	}
	srv := NewServer(stores, info, nil)
	srv.sharesNamespace = true
	if dr, ok := srv.rt.(*directRuntime); ok {
		dr.commitSigning = signing
	}
	return listenAndServe(srv, sockPath, grantSocketToOwnerOnly)
}

// ensureOwnerOnlyDir pins the LOCAL socket root to 0700, creating it if it is
// not there yet. The chmod is the point: MkdirAll's mode applies only to a
// directory it actually creates, so on the second and every later run of a
// given install the mode is whatever the first run's umask left — and an
// operator with a permissive umask would get a socket root other accounts on
// the machine could list.
//
// Local-only, deliberately. The jail's root and each run's directory in it
// belong to the cap-broker's layout (see listenAndServe); a chmod there would
// fail with EPERM, and is not this process's to make.
func ensureOwnerOnlyDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("agenthost: mkdir %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("agenthost: chmod %s: %w", dir, err)
	}
	return nil
}

// listenAndServe is the shared socket lifecycle both entry points above run:
// ensure the parent dir, clear a stale file, listen, grant, serve. grant is
// what distinguishes them — see StartLocal.
//
// Property B / fs-permissions invariant (load-bearing):
//
//  1. The parent directory is closed to non-owners before the socket exists,
//     so the window between net.Listen and the grant is unreachable — without
//     it, another uid on the host could enumerate the socket root and dial a
//     run's socket in the moment before it is narrowed. WHICH directory mode
//     delivers that differs by who creates the root, and the MkdirAll below
//     pins neither: it is a no-op on an existing directory, and every
//     directory here exists before this runs.
//
//     For a jail's socket the directory is the run's own, created by the
//     cap-broker for the engagement: the sidecar's uid, the run's jail gid,
//     mode 02750. For the local root StartLocal owns it, at 0700 — one uid,
//     nothing to share — enforced by ensureOwnerOnlyDir rather than by this
//     MkdirAll.
//
//  2. net.Listen("unix", ...) — creates the socket file with the
//     process umask applied (typically 0666 or 0755); either is too
//     permissive on its own, but step 1 locks the parent so nobody
//     can reach it yet.
//
//  3. The grant callback narrows it to exactly the identity that must
//     connect.
func listenAndServe(server *Server, sockPath string, grant func(string) error) (*HostDaemon, error) {
	// A no-op wherever the directory was made for this process — the jail's
	// by the cap-broker, the local one by ensureOwnerOnlyDir — leaving its
	// owner and mode to whoever pinned them (step 1 above).
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		return nil, fmt.Errorf("agenthost: mkdir %s: %w", filepath.Dir(sockPath), err)
	}

	// Remove any stale socket file from a previous crash. net.Listen
	// would otherwise EADDRINUSE on a path that's actually unused. A jail's
	// directory is made fresh for each engagement, so there this finds
	// nothing; the local root is reused across runs, and has one owner.
	_ = os.Remove(sockPath)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("agenthost: listen %s: %w", sockPath, err)
	}
	if err := grant(sockPath); err != nil {
		_ = listener.Close()
		_ = os.Remove(sockPath)
		return nil, err
	}

	hd := &HostDaemon{
		listener: listener,
		sockPath: sockPath,
		server:   server,
		doneCh:   make(chan struct{}),
	}
	// No recover: this goroutine is the accept loop and nothing else — every
	// request is dispatched into its own goroutine, which recovers (see
	// Server.serveConn). Accept, an errors.Is, and a log call cannot panic.
	go func() {
		defer close(hd.doneCh)
		if err := server.Serve(listener); err != nil {
			agenthostLog.Error("daemon serve failed", "error", err)
		}
	}()
	return hd, nil
}

// grantSocketToOwnerOnly narrows a freshly-listened socket to this uid alone.
// It is the whole grant for a local sandboxed run, whose agent shares this
// uid: no chown (there is nobody to give it to), no group bit (no group would
// name only the right process), just the umask-independent 0600 that keeps
// every OTHER user on the machine out.
func grantSocketToOwnerOnly(sockPath string) error {
	if err := os.Chmod(sockPath, 0o600); err != nil {
		return fmt.Errorf("agenthost: chmod %s: %w", sockPath, err)
	}
	return nil
}

// grantToJail opens a file the run's sidecar created to the run's jail: mode as
// given (0660 for the socket — connect(2) needs write — and 0640 for the cert
// the jail only reads), on a file that must already be in jailGID's group. The
// run's socket directory is setgid to that gid, so everything the sidecar
// creates there is born in it; the sidecar is not a member and could not chgrp
// a file into it, so one that came out in another group is refused here
// rather than served unreachable.
func grantToJail(path string, jailGID int, mode os.FileMode) error {
	if !sandbox.IsJailUID(jailGID) {
		return fmt.Errorf("agenthost: %d is not a jail gid; refusing to grant %s to it", jailGID, path)
	}
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		return fmt.Errorf("agenthost: stat %s: %w", path, err)
	}
	if int(st.Gid) != jailGID {
		return fmt.Errorf("agenthost: %s was created in group %d, not the run's jail gid %d (the run's socket directory must be setgid to it)", path, st.Gid, jailGID)
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("agenthost: chmod %s: %w", path, err)
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

// SocketPathFor returns the host-side path of conversationID's agenthost socket,
// inside the run's socket directory. Deterministic so the sidecar (which
// creates + serves it on the executor path) and the orchestrator (which
// bind-mounts it into the jail) agree on the path without a round trip.
func SocketPathFor(conversationID string) string {
	return filepath.Join(runSocketDir(conversationID), agentHostSocketName)
}

// runSocketDir is conversationID's socket directory under hostSocketRoot —
// sandbox.RunSocketDir's derivation, mirrored because the broker side keeps its
// root redirectable for its own tests and this side keeps its root a constant.
func runSocketDir(conversationID string) string {
	return filepath.Join(hostSocketRoot, sanitizeSocketName(conversationID))
}

// SocketMountFor returns the bind mount exposing conversationID's socket to the agent at
// DefaultSocketPath. On the executor path the SIDECAR creates + serves the
// socket, so the orchestrator adds THIS mount to the sandbox spec without
// starting a Server of its own — the socket already exists (the sidecar bound
// it during bring-up, before the sandbox launches).
func SocketMountFor(conversationID string) sandbox.Mount {
	return sandbox.Mount{Source: SocketPathFor(conversationID), Destination: DefaultSocketPath}
}

// CABundlePathFor returns the host-side path of conversationID's CA bundle,
// beside the run's socket in its socket directory: the trust file every TLS
// client in the jail reads. Deterministic so the sidecar (which writes it) and
// the orchestrator (which bind-mounts it into the jail) agree without a round
// trip — the same contract as SocketPathFor. The cap-broker independently
// re-derives this as TrustedCABundlePath and validates a launch's mount source
// against it; a drift test cross-checks the two derivations agree.
func CABundlePathFor(conversationID string) string {
	return filepath.Join(runSocketDir(conversationID), caBundleName)
}

// WriteCABundle writes conversationID's CA bundle (public certificates only;
// no private key leaves the sidecar) to CABundlePathFor(conversationID) and
// grants the run's jail gid read access, the same grant the socket gets.
// Called by the sidecar during bring-up of every run, before the sandbox
// launches (the OCI spec references the source path, which must exist).
func WriteCABundle(conversationID string, bundlePEM []byte, jailGID int) error {
	path := CABundlePathFor(conversationID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("agenthost: mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := writeFreshCABundle(path, bundlePEM); err != nil {
		return err
	}
	return grantToJail(path, jailGID, 0o640)
}

// writeFreshCABundle creates the bundle as a NEW file, removing whatever is
// already at the path first — the same remove-before-create rule
// listenAndServe applies to the socket. The run's socket directory is made
// fresh for each engagement, so in a jail's cell this normally finds nothing;
// creating the file new rather than truncating in place makes it belong to
// whoever is serving it either way.
func writeFreshCABundle(path string, bundlePEM []byte) error {
	_ = os.Remove(path)
	if err := os.WriteFile(path, bundlePEM, 0o640); err != nil {
		return fmt.Errorf("agenthost: write CA bundle %s: %w", path, err)
	}
	return nil
}

// sanitizeSocketName trims a conversation id to a fs-safe form. ConversationIDs are
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
