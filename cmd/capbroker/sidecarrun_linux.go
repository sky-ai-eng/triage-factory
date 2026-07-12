//go:build linux

package capbroker

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// sidecarStdioSocketPath is the per-run unix socket the orchestrator
// listens on and the broker dials to hand the sidecar its stdio — the
// sidecar analog of stdioSocketPath, under the same owner-only /run/tf
// dir. A var so tests can redirect it to a writable temp dir, like
// stdioSocketPath.
var sidecarStdioSocketPath = func(containerID string) string {
	return filepath.Join(socketDir, containerID+"-sidecar-stdio.sock")
}

// LaunchSidecar (broker client) opens the per-sidecar stdio listener,
// issues the LaunchSidecar RPC (which blocks until the broker has dialed
// and handed the fd to the freshly-execed sidecar), accepts this process's
// end of that passed-through socket, and returns a LaunchedSidecar the
// caller closes when the run ends. Unlike LaunchRun/brokerRun there is no
// separate Start step — Phase 1's sidecar does no work a caller needs to
// sequence around, so the launch is synchronous start-to-running.
func (c *IPCClient) LaunchSidecar(ctx context.Context, p sandbox.SidecarLaunchParams) (sandbox.LaunchedSidecar, error) {
	socketPath := sidecarStdioSocketPath(p.ContainerID)
	l, err := listen(socketPath)
	if err != nil {
		return nil, fmt.Errorf("capbroker: listen sidecar stdio socket: %w", err)
	}
	cleanupListener := func() {
		_ = l.Close()
		_ = os.Remove(socketPath)
	}

	params := p
	params.StdioSocketPath = socketPath
	if err := c.call(ctx, methodLaunchSidecar, launchSidecarArgs{Params: params}, nil); err != nil {
		cleanupListener()
		return nil, err
	}

	// The broker dials during the LaunchSidecar RPC (already returned by
	// the time we Accept, exactly like LaunchRun's stdio handoff), so the
	// connection is queued and Accept returns at once; the deadline only
	// guards a broker that reported success without dialing.
	if ul, ok := l.(*net.UnixListener); ok {
		_ = ul.SetDeadline(time.Now().Add(acceptTimeout))
	}
	conn, err := l.Accept()
	cleanupListener()
	if err != nil {
		_ = c.call(context.Background(), methodKillSidecar, killSidecarArgs{ContainerID: p.ContainerID}, nil)
		return nil, fmt.Errorf("capbroker: accept sidecar stdio: %w", err)
	}
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		_ = conn.Close()
		_ = c.call(context.Background(), methodKillSidecar, killSidecarArgs{ContainerID: p.ContainerID}, nil)
		return nil, fmt.Errorf("capbroker: accepted sidecar stdio is not a unix conn")
	}

	return &sidecarHandle{client: c, containerID: p.ContainerID, conn: uc}, nil
}

// sidecarHandle is the orchestrator-side sandbox.LaunchedSidecar for a
// broker-supervised run-sidecar process. Close is the only operation Phase
// 1 needs: kill (SIGKILL, idempotent — the broker's KillRun handler
// no-ops on an already-reaped entry) then wait (drains the broker's
// registry entry, exactly like a normal run's Wait does via WaitRun).
type sidecarHandle struct {
	client      *IPCClient
	containerID string
	conn        *net.UnixConn
	closeOnce   sync.Once
}

var _ sandbox.LaunchedSidecar = (*sidecarHandle)(nil)

func (h *sidecarHandle) Close() error {
	h.closeOnce.Do(func() {
		if h.conn != nil {
			_ = h.conn.Close()
		}
		// Detached context: teardown must complete even if the run's own
		// ctx is already canceled (the common case — Close runs as part of
		// cancellation/cleanup).
		_ = h.client.call(context.Background(), methodKillSidecar, killSidecarArgs{ContainerID: h.containerID}, nil)
		_ = h.client.call(context.Background(), methodWaitSidecar, waitSidecarArgs{ContainerID: h.containerID}, nil)
	})
	return nil
}
