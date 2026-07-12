//go:build linux

package capbroker

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// launchSidecarProcess is the seam the broker execs the run-sidecar
// subcommand through — the sidecar analog of launchRuntime. The default
// hands the socket fd to sandbox.LaunchSidecarProcess, which wires it as
// the sidecar's stdin+stdout and closes the broker's copy, exactly like
// launchRuntime does for runsc. A var so tests can substitute a stand-in
// process instead of re-execing the real binary as a real setuid target.
var launchSidecarProcess = func(ctx context.Context, containerID string, uid, gid int, stdio *os.File, stderr io.Writer) (supervisedRuntime, error) {
	return sandbox.LaunchSidecarProcess(ctx, containerID, uid, gid, stdio, stderr)
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

	rt, err := launchSidecarProcess(s.baseCtx, p.ContainerID, p.UID, p.GID, f, os.Stderr)
	_ = conn.Close()
	if err != nil {
		return nil, fmt.Errorf("capbroker: launch sidecar: %w", err)
	}

	entry := &runEntry{rt: rt, done: make(chan struct{})}
	s.runsMu.Lock()
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
