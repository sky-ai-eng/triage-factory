//go:build linux

package capbroker

import (
	"context"
	"fmt"
	"net"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// captureRunDeltaTo is the seam the broker's capture handler runs the
// actual capture through. The default hands the dialed socket fd straight
// to sandbox.CaptureRunDeltaTo, which assigns it directly as the capture
// child's stdout — the run's bytes never enter this process. A var (like
// launchRuntime/prepareBundle for LaunchRun) so tests can substitute a
// stand-in that doesn't need the uid-drop/netns capabilities the real
// capture needs, while still exercising the RPC/socket-passthrough wiring.
var captureRunDeltaTo = sandbox.CaptureRunDeltaTo

// captureRunDelta is the broker's streaming capture handler: it dials the
// orchestrator's per-capture stdout socket (StdoutSocketPath, opened by
// IPCClient.CaptureRunDelta before it issues this RPC), takes the fd, and
// hands it straight to the capture child as stdout — the same
// dial-take-fd-close-own-copy shape launchRun uses for the run's live
// stdio, applied here so the park-time delta's bytes never enter the
// broker's address space either. Like launchRun, this bypasses s.ops
// entirely: it is a broker-owned handler, not a PrivilegedOps method — the
// PrivilegedOps.CaptureRunDelta interface (buffered) still exists solely
// for hostOps's unprivileged in-process fallback.
func (s *Server) captureRunDelta(ctx context.Context, a captureRunDeltaArgs) (any, error) {
	// Boundary gate, same treatment as LaunchParams.Worktree: reject a
	// malformed path before dialing anything. Dialing carries no privilege
	// delta (broker and orchestrator share a uid), so shape validation is
	// sufficient — there is no allowlist to check this against.
	if err := sandbox.ValidateCaptureStdoutSocketPath(a.StdoutSocketPath); err != nil {
		return nil, err
	}

	dialer := net.Dialer{Timeout: dialStdioTimeout}
	conn, err := dialer.DialContext(ctx, "unix", a.StdoutSocketPath)
	if err != nil {
		return nil, fmt.Errorf("capbroker: dial capture stdout socket %s: %w", a.StdoutSocketPath, err)
	}
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("capbroker: capture stdout socket is not a unix conn")
	}
	// File() returns a dup of the connection's fd; captureRunDeltaTo
	// inherits it as the capture child's stdout and closes this dup once
	// the child has its own (see CaptureRunDeltaTo's doc). Close our
	// remaining reference to the connection itself right away — the dup
	// is now the only handle to the socket this process needs.
	f, err := uc.File()
	_ = conn.Close()
	if err != nil {
		return nil, fmt.Errorf("capbroker: take capture stdout fd: %w", err)
	}

	stderrTail, err := captureRunDeltaTo(ctx, a.Worktree, f)
	if err != nil {
		return nil, fmt.Errorf("capbroker: capture run delta: %w (stderr: %s)", err, stderrTail)
	}
	return captureRunDeltaResult{StderrTail: stderrTail}, nil
}
