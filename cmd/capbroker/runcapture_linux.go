//go:build linux

package capbroker

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// captureJail is the seam the broker's capture handler runs the actual capture
// through. The default hands the dialed socket fd straight to
// sandbox.LaunchCaptureJail, which assigns it as the jail's stdout — the
// manifest never needs a broker-side copy. A var (like launchRuntime for
// LaunchRun) so tests can substitute a stand-in that needs no gVisor host,
// while still exercising the RPC and socket-passthrough wiring.
var captureJail = sandbox.LaunchCaptureJail

// captureRunTree is the broker's streaming capture handler: it dials the
// orchestrator's per-capture stdout socket (StdoutSocketPath, opened by
// IPCClient.CaptureRunTree before it issues this RPC), takes the fd, and
// hands it to the capture jail as stdout — the same dial-take-fd-close-own-copy
// shape launchRun uses for the run's live stdio, applied here so the manifest
// bypasses the broker's address space; the large members go to the staging
// stream the jail inherits. Like launchRun, it bypasses s.ops entirely: it is
// a broker-owned handler, not a PrivilegedOps method.
func (s *Server) captureRunTree(ctx context.Context, a captureRunTreeArgs) (any, error) {
	// Boundary gate, same treatment as LaunchParams.Worktree: reject a
	// malformed path before dialing anything. Dialing carries no privilege
	// delta (broker and orchestrator share a uid), so shape validation is
	// sufficient — there is no allowlist to check this against. The tree, the
	// checkouts and the staging file are validated by the launch itself.
	if err := sandbox.ValidateCaptureStdoutSocketPath(a.StdoutSocketPath); err != nil {
		return nil, err
	}

	// The slot a run launch holds, for the same reason: a capture is a jail,
	// and the broker bounds how many it runs at once.
	if err := s.acquireLaunchSlot(ctx); err != nil {
		return nil, err
	}
	defer s.releaseLaunchSlot()

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
	// File() returns a dup of the connection's fd; the jail inherits it as its
	// stdout and the launch closes this dup once the jail has its own. Close
	// our remaining reference to the connection itself right away — the dup is
	// now the only handle to the socket this process needs.
	f, err := uc.File()
	_ = conn.Close()
	if err != nil {
		return nil, fmt.Errorf("capbroker: take capture stdout fd: %w", err)
	}

	stderrTail, err := captureJail(ctx, sandbox.CaptureParams{
		Root:       a.Root,
		Checkouts:  a.Checkouts,
		StagingDir: a.StagingDir,
	}, f)
	if err != nil {
		if stderrTail != "" {
			return nil, fmt.Errorf("capbroker: capture run tree: %w (stderr: %s)", err, stderrTail)
		}
		return nil, fmt.Errorf("capbroker: capture run tree: %w", err)
	}
	return captureRunTreeResult{StderrTail: stderrTail}, nil
}

// freezeRun pauses or resumes a run's jail by container id. Only a run this
// broker launched and still holds is reached; an id it holds nothing for is a
// jail that has exited, which has nothing to freeze, so the call succeeds.
// The sidecar shares the registry and is refused by its id's suffix: it holds
// credentials and is an ordinary host process, which runsc knows nothing of.
func (s *Server) freezeRun(ctx context.Context, a freezeRunArgs, op func(context.Context, string) error) (any, error) {
	if strings.HasSuffix(a.ContainerID, sandbox.SidecarContainerIDSuffix) {
		return nil, fmt.Errorf("capbroker: %q is a credential sidecar, not a jail", a.ContainerID)
	}
	s.runsMu.Lock()
	entry := s.runs[a.ContainerID]
	s.runsMu.Unlock()
	if entry == nil {
		return emptyResult{}, nil
	}
	select {
	case <-entry.done:
		return emptyResult{}, nil
	default:
	}
	if err := op(ctx, a.ContainerID); err != nil {
		return nil, err
	}
	return emptyResult{}, nil
}
