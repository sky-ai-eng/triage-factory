//go:build linux

package capbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
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
	deadlineCap := time.Now().Add(callTimeout)
	if !ok || deadlineCap.Before(deadline) {
		deadline = deadlineCap
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return fmt.Errorf("capbroker: set deadline: %w", err)
	}

	argsJSON, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("capbroker: marshal %s args: %w", method, err)
	}
	req := request{Version: ProtocolVersion, Method: method, Args: argsJSON}
	if err := writeFrame(conn, req); err != nil {
		return err
	}

	var resp response
	if err := readFrame(conn, &resp); err != nil {
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

var _ sandbox.PrivilegedOps = (*IPCClient)(nil)

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

// SetupRunCgroup RPCs the broker to create the cgroup (the privileged
// mkdir + memory.max/memory.swap.max writes) and gets back the directory
// path — never the fd, which can't cross the wire (see setupRunCgroupResult's
// doc). It then opens that same path itself, exactly how hostOps's own
// newRunCgroup obtains its fd (os.OpenFile, no elevated privilege needed to
// open a directory the caller can already see). This works today because
// the orchestrator still holds capabilities too — the broker keeps them,
// and the orchestrator does not drop them yet — and both processes see the
// same cgroupfs. The fd is then usable for THIS process's own
// exec.Cmd.CgroupFD, matching the documented contract that it's "only
// usable by a caller that Starts the runsc process in the same address
// space as this call" — still true here since runsc launch hasn't moved
// to the broker yet (a later phase of this split).
func (c *IPCClient) SetupRunCgroup(name string, limitMB int) (string, *os.File, error) {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	var res setupRunCgroupResult
	if err := c.call(ctx, methodSetupRunCgroup, setupRunCgroupArgs{Name: name, LimitMB: limitMB}, &res); err != nil {
		return "", nil, err
	}
	f, err := os.OpenFile(res.Dir, os.O_RDONLY, 0)
	if err != nil {
		return "", nil, fmt.Errorf("capbroker: open cgroup dir fd: %w", err)
	}
	return res.Dir, f, nil
}

func (c *IPCClient) RemoveRunCgroup(dir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	return c.call(ctx, methodRemoveRunCgroup, removeRunCgroupArgs{Dir: dir}, nil)
}

func (c *IPCClient) ReapOrphans(ctx context.Context) error {
	return c.call(ctx, methodReapOrphans, emptyArgs{}, nil)
}
