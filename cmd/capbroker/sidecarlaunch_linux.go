//go:build linux

package capbroker

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// launchSidecarProcess is the seam the broker execs the run-sidecar
// subcommand through — the sidecar analog of launchRuntime. The default
// hands the socket fd to sandbox.LaunchSidecarProcess, which wires it as
// the sidecar's stdin+stdout and closes the broker's copy, exactly like
// launchRuntime does for runsc. A var so tests can substitute a stand-in
// process instead of re-execing the real binary as a real setuid target.
var launchSidecarProcess = func(ctx context.Context, containerID string, uid, gid int, stdio *os.File, extra []*os.File, stderr io.Writer) (supervisedRuntime, error) {
	return sandbox.LaunchSidecarProcess(ctx, containerID, uid, gid, stdio, extra, stderr)
}

// sharedOriginListenAddr is the address the broker binds a run's shared origin
// on: the host side of that run's veth, on the fixed shared-origin port. Derived
// from the subnet index the launch params carry (and which
// ValidateSidecarLaunchParams has already tied to the uid), never named by the
// caller.
//
// A var only so a test can retarget it to loopback — binding a real veth IP
// needs the run's network to exist and the port needs root, neither of which a
// unit test has.
var sharedOriginListenAddr = func(subnetIdx uint8) string {
	return net.JoinHostPort(sandbox.HostVethIP(subnetIdx), strconv.Itoa(sandbox.SharedOriginPort))
}

// bindSharedOrigin binds the run's fake-GHE origin listener on
// HostVethIP(subnetIdx):SharedOriginPort and returns it as a file to hand the
// sidecar. The broker does this because 443 is privileged and the sidecar is
// capless — the same reason the broker owns the netns and the veth themselves.
// It is a *grant*, not a traffic role: the broker never accepts a connection on
// this listener, never reads a byte through it, and hands over its only copy.
//
// The address is derived from the validated subnet index, never named by the
// caller. Best-effort: a bind failure returns nil and the sidecar falls back to
// an ephemeral port of its own, which costs gh's repo inference and nothing
// else — never the run.
func bindSharedOrigin(subnetIdx uint8) *os.File {
	addr := sharedOriginListenAddr(subnetIdx)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		brokerLog.Warn("bind shared-origin listener failed; the run's gh channel will use an ephemeral port and lose repo inference",
			"addr", addr, "error", err)
		return nil
	}
	// File() dups the descriptor, so closing the listener afterwards leaves the
	// socket alive on the dup we pass down — and leaves the broker holding
	// nothing that could accept on it.
	tcp, ok := ln.(*net.TCPListener)
	if !ok {
		_ = ln.Close()
		return nil
	}
	f, err := tcp.File()
	_ = tcp.Close()
	if err != nil {
		brokerLog.Warn("take shared-origin listener fd failed; the run's gh channel will use an ephemeral port and lose repo inference",
			"addr", addr, "error", err)
		return nil
	}
	return f
}

// launchSidecar is the broker's spec-owning launch for one run's
// credential-sidecar process — the sibling of launchRun for the per-run
// sidecar harness. It validates the params at the RPC boundary (in
// particular the uid/gid band — the one check standing between a
// compromised orchestrator and an arbitrary setuid target), dials the
// orchestrator's per-sidecar stdio socket, hands its fd to the sidecar
// process as stdin+stdout, execs+supervises it, and registers it in the
// SAME s.runs table LaunchRun uses (keyed by the sidecar's own distinct
// ContainerID) so the existing waitRun/killRun handlers reap it without
// any sidecar-specific registry logic.
//
// Unlike launchRun, this does NOT take an in-flight launch slot
// (s.acquireLaunchSlot): the sidecar costs the broker a fork+setuid+exec,
// none of the expensive privileged setup (bundle prep, netns, rootfs) the
// semaphore exists to queue. It is still 1:1 with runs, so it inherits the
// same effective ceiling as the run's own launch without needing a second
// counter.
func (s *Server) launchSidecar(ctx context.Context, a launchSidecarArgs) (any, error) {
	p := a.Params

	// Boundary gate, same posture as launchRun's: reject a param outside
	// the allowlist BEFORE dialing or execing anything.
	if err := sandbox.ValidateSidecarLaunchParams(p); err != nil {
		return nil, err
	}

	// Fast-path duplicate check: the run registry is shared with LaunchRun's
	// own entries, keyed by ContainerID, so registering over a live entry
	// would leak its supervision state (the previous process becomes
	// unreachable — no wait, no kill — while still holding a pid). Checked
	// again, atomically, right before insertion below; this first check just
	// avoids a wasted dial+exec in the common (non-racing) case.
	if s.sidecarRegistered(p.ContainerID) {
		return nil, fmt.Errorf("capbroker: sidecar container id %q is already registered", p.ContainerID)
	}

	dialer := net.Dialer{Timeout: dialStdioTimeout}
	conn, err := dialer.DialContext(ctx, "unix", p.StdioSocketPath)
	if err != nil {
		return nil, fmt.Errorf("capbroker: dial sidecar stdio socket %s: %w", p.StdioSocketPath, err)
	}
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("capbroker: sidecar stdio socket is not a unix conn")
	}
	f, err := uc.File()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("capbroker: take sidecar stdio fd: %w", err)
	}

	// The run's shared-origin listener, bound here because only this process
	// can. nil when the bind failed — LaunchSidecarProcess takes an empty
	// ExtraFiles and the sidecar falls back on its own ephemeral port.
	var extra []*os.File
	if origin := bindSharedOrigin(p.SubnetIdx); origin != nil {
		extra = append(extra, origin)
	}

	rt, err := launchSidecarProcess(s.baseCtx, p.ContainerID, p.UID, p.GID, f, extra, os.Stderr)
	_ = conn.Close()
	if err != nil {
		return nil, fmt.Errorf("capbroker: launch sidecar: %w", err)
	}

	entry := &conversationEntry{rt: rt, done: make(chan struct{})}
	s.runsMu.Lock()
	if _, exists := s.runs[p.ContainerID]; exists {
		s.runsMu.Unlock()
		// Lost a race against a concurrent duplicate launch that registered
		// between the fast-path check above and here — kill and reap what
		// this call just started rather than leaving it unsupervised, and
		// report the same duplicate error the fast path would have.
		_ = rt.Kill()
		go func() { _, _ = rt.Wait() }()
		return nil, fmt.Errorf("capbroker: sidecar container id %q is already registered", p.ContainerID)
	}
	s.runs[p.ContainerID] = entry
	s.runsMu.Unlock()

	go func() {
		oom, werr := rt.Wait()
		entry.oom = oom
		entry.waitErr = werr
		close(entry.done)
	}()

	return emptyResult{}, nil
}

// sidecarRegistered reports whether containerID already has a live entry in
// the shared run registry.
func (s *Server) sidecarRegistered(containerID string) bool {
	s.runsMu.Lock()
	defer s.runsMu.Unlock()
	_, exists := s.runs[containerID]
	return exists
}
