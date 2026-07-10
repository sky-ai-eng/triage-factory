//go:build linux

package capbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// dialTimeout caps how long the client waits for the broker to accept a
// connection. The socket is local; if the broker is up, accept happens in
// microseconds. Short so a dead broker fails fast with a clear error
// instead of looking like a slow host operation.
const dialTimeout = 5 * time.Second

// IPCClient is the unix-socket implementation of sandbox.PrivilegedOps.
// Each call dials, sends one frame, reads one frame, and closes — the
// broker's handleConn is one-shot, so reusing a connection across calls
// would EOF on the second read. There is exactly one broker (and so one
// IPCClient) per executor — unlike agenthost's per-run client — so
// multiple runs' concurrent SetupNetwork/EnsureRootfs/etc. calls share
// this client and must not serialize against each other: closed is an
// atomic.Bool rather than a mutex-guarded field precisely so concurrent
// calls dial and round-trip independently (each owns its own net.Conn;
// there is no shared mutable state to protect beyond this flag), and
// Close never blocks behind an in-flight call.
type IPCClient struct {
	socketPath string
	closed     atomic.Bool
}

// Dial returns an IPCClient bound to socketPath. No connection opens
// until the first call.
func Dial(socketPath string) *IPCClient {
	return &IPCClient{socketPath: socketPath}
}

// Close marks the client closed so subsequent calls fail fast. There is no
// persistent connection to release — every call dials fresh — so this
// never blocks on an in-flight call.
func (c *IPCClient) Close() error {
	c.closed.Store(true)
	return nil
}

func (c *IPCClient) call(ctx context.Context, method string, args, result any) error {
	return c.callWithCap(ctx, method, args, result, callTimeout)
}

// callWithCap is call with an explicit connection-deadline budget. A budget
// > 0 caps the round-trip at that duration — the default for the bounded
// host operations (network/rootfs/cgroup). A budget <= 0 imposes NO client
// cap: the call blocks until the broker replies, the caller's ctx deadline
// (if any) fires, or the connection breaks.
//
// WaitRun uses the uncapped form. Its server side deliberately has no
// timeout because it blocks until the supervised run exits (potentially the
// whole run), and the one-shot Run path can invoke Wait before the runsc
// child has actually exited (its stream reader returns on the terminal
// result, not on EOF). A fixed client cap would spuriously time that out and
// drop the exit's OOM attribution; matching the server's no-timeout design
// keeps the wait bounded only by the run itself (which the cancellation
// watcher kills when the caller's context ends).
func (c *IPCClient) callWithCap(ctx context.Context, method string, args, result any, budget time.Duration) error {
	if c.closed.Load() {
		return errors.New("capbroker: client closed")
	}

	dialer := net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return fmt.Errorf("capbroker: dial %s: %w", c.socketPath, err)
	}
	defer func() { _ = conn.Close() }()

	deadline, ok := ctx.Deadline()
	if budget > 0 {
		capAt := time.Now().Add(budget)
		if !ok || capAt.Before(deadline) {
			deadline, ok = capAt, true
		}
	}
	if ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return fmt.Errorf("capbroker: set deadline: %w", err)
		}
	}
	// budget <= 0 with no ctx deadline → leave the connection deadline unset:
	// block until the broker replies or the connection breaks (e.g. broker
	// exit), matching the server's no-timeout WaitRun dispatch.

	argsJSON, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("capbroker: marshal %s args: %w", method, err)
	}
	req := request{Version: ProtocolVersion, Method: method, Args: argsJSON}
	if err := writeFrame(conn, req, maxFrameSize); err != nil {
		return err
	}

	var resp response
	if err := readFrame(conn, &resp, responseFrameSize); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("capbroker: broker closed connection during %s: %w", method, err)
		}
		return err
	}

	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	if result == nil || len(resp.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(resp.Result, result); err != nil {
		return fmt.Errorf("capbroker: decode %s result: %w", method, err)
	}
	return nil
}

// Ping is a no-op round-trip used to confirm the broker is up and
// speaking the current protocol version — the orchestrator's readiness
// check after spawning the broker subprocess (see Start in
// orchestrator.go). Not part of sandbox.PrivilegedOps.
func (c *IPCClient) Ping(ctx context.Context) error {
	return c.call(ctx, methodPing, emptyArgs{}, nil)
}

// --- sandbox.PrivilegedOps implementation ---
//
// The IPCClient is the full sandbox.SandboxOps the orchestrator installs:
// the privileged operations below plus LaunchRun (brokerrun_linux.go).
var _ sandbox.SandboxOps = (*IPCClient)(nil)

func (c *IPCClient) SetupNetwork(ctx context.Context, runID string, subnetIdx uint8) (sandbox.NetworkState, error) {
	var res setupNetworkResult
	if err := c.call(ctx, methodSetupNetwork, setupNetworkArgs{RunID: runID, SubnetIdx: subnetIdx}, &res); err != nil {
		return sandbox.NetworkState{}, err
	}
	return res.State, nil
}

func (c *IPCClient) TeardownNetwork(ctx context.Context, state sandbox.NetworkState) error {
	return c.call(ctx, methodTeardownNetwork, teardownNetworkArgs{State: state}, nil)
}

func (c *IPCClient) EnsureRootfs(ctx context.Context, selector sandbox.RootfsSelector) (string, error) {
	var res ensureRootfsResult
	if err := c.call(ctx, methodEnsureRootfs, ensureRootfsArgs{Selector: selector}, &res); err != nil {
		return "", err
	}
	return res.Path, nil
}

func (c *IPCClient) ReapOrphans(ctx context.Context) error {
	return c.call(ctx, methodReapOrphans, emptyArgs{}, nil)
}

func (c *IPCClient) ChownRunTree(ctx context.Context, root, subpath string) error {
	return c.call(ctx, methodChownRunTree, chownRunTreeArgs{Root: root, Subpath: subpath}, nil)
}

func (c *IPCClient) RemoveRunTree(ctx context.Context, path string) error {
	return c.call(ctx, methodRemoveRunTree, removeRunTreeArgs{Path: path}, nil)
}

// CaptureRunDelta uses captureTimeout (not callTimeout) as its budget,
// mirroring the server-side dispatch exception: git bundling a large
// worktree is legitimately slower than any of the bounded host
// operations the default budget is sized for.
func (c *IPCClient) CaptureRunDelta(ctx context.Context, worktree string) ([]byte, error) {
	var res captureRunDeltaResult
	if err := c.callWithCap(ctx, methodCaptureRunDelta, captureRunDeltaArgs{Worktree: worktree}, &res, captureTimeout); err != nil {
		return nil, err
	}
	return res.Delta, nil
}
