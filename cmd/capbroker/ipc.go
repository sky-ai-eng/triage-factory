//go:build linux

package capbroker

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
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
func (c *IPCClient) callWithCap(ctx context.Context, method string, args, result any, budget time.Duration) (err error) {
	// The single client-side span for the whole broker surface. It sits here
	// rather than on each verb because this is the only place all of them
	// pass through — network setup, rootfs, the run launch, every teardown —
	// so a method added later is instrumented by existing.
	//
	// This is also the ONLY place the broker's work is visible at all. The
	// broker holds CAP_SYS_ADMIN/CAP_NET_ADMIN and must never gain an
	// outbound network dependency, so it exports nothing itself (standing
	// decision 1) and no frame this file writes carries trace context: the
	// request struct is method + args, unchanged. What the reader gets is the
	// round trip as the caller experienced it, which is what a slow bring-up
	// needs. If a breakdown of the broker's internals is ever wanted, the
	// protocol can carry optional timing fields on the RESPONSE without a
	// version bump — data flowing outward from the privileged process, never
	// instructions flowing in.
	ctx, span := tracer.Start(ctx, "capbroker.call",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(telemetry.Op(method)))
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()

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
	if err := readFrame(conn, &resp, maxFrameSize); err != nil {
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

func (c *IPCClient) SetupNetwork(ctx context.Context, conversationID string, subnetIdx uint8) (sandbox.NetworkState, error) {
	var res setupNetworkResult
	if err := c.call(ctx, methodSetupNetwork, setupNetworkArgs{ConversationID: conversationID, SubnetIdx: subnetIdx}, &res); err != nil {
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

// captureSocketDir is the parent directory for per-capture stdout sockets —
// the same shared socket directory the launch path's per-run stdio sockets
// live under. A var (like stdioSocketPath's directory) so tests can
// redirect it to a writable temp dir instead of the root-only production
// /run/tf.
var captureSocketDir = socketDir

// captureSocketPath mints a fresh, unique per-capture stdout socket path.
// Unlike the per-run stdio socket (keyed by the run's unique container id),
// a capture has no natural unique key at this layer, so this generates one.
func captureSocketPath() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("capbroker: generate capture socket name: %w", err)
	}
	return filepath.Join(captureSocketDir, fmt.Sprintf("capture-%x.sock", raw)), nil
}

// CaptureRunDelta streams the capture child's stdout directly from the
// broker to this process over a per-capture unix socket — the fd-passthrough
// pattern LaunchRun uses for the run's live stdio (brokerrun_linux.go),
// applied here so the park-time git-delta capture's (potentially large)
// result never enters the broker's address space either.
//
// It opens the listener the broker will dial, issues the RPC carrying the
// socket path, and concurrently accepts + reads the stream while awaiting
// the RPC response. Bytes are returned only once BOTH signals hold: a
// successful RPC response, AND the stream's EOF — a response without EOF
// (the stream wedged) or EOF without a response (the RPC is still the
// authoritative outcome, e.g. it may yet report a child failure) are each
// insufficient alone. captureTimeout bounds the whole round trip on both
// sides: the RPC call, the accept, and the capped read.
func (c *IPCClient) CaptureRunDelta(ctx context.Context, worktree, sessionID string) ([]byte, error) {
	sockPath, err := captureSocketPath()
	if err != nil {
		return nil, err
	}
	l, err := listen(sockPath)
	if err != nil {
		return nil, fmt.Errorf("capbroker: listen capture stdout socket: %w", err)
	}
	defer func() {
		_ = l.Close()
		_ = os.Remove(sockPath)
	}()

	ctx, cancel := context.WithTimeout(ctx, captureTimeout)
	defer cancel()

	// acceptedConn always receives exactly one value from the goroutine
	// below — the accepted conn, or nil if Accept itself failed — kept
	// separate from streamDone so an RPC failure can synchronously reach
	// (and close) an already-accepted conn instead of leaving it to buffer
	// toward CaptureMaxBytes, unread, for up to the full cap/deadline.
	acceptedConn := make(chan net.Conn, 1)
	type streamResult struct {
		buf []byte
		err error
	}
	streamDone := make(chan streamResult, 1)
	go func() {
		deadline, _ := ctx.Deadline()
		if ul, ok := l.(*net.UnixListener); ok {
			_ = ul.SetDeadline(deadline)
		}
		conn, err := l.Accept()
		if err != nil {
			acceptedConn <- nil
			streamDone <- streamResult{nil, fmt.Errorf("accept capture stream: %w", err)}
			return
		}
		acceptedConn <- conn
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(deadline)
		buf, err := sandbox.ReadCapturedDelta(conn)
		streamDone <- streamResult{buf, err}
	}()

	var res captureRunDeltaResult
	args := captureRunDeltaArgs{Worktree: worktree, StdoutSocketPath: sockPath, SessionID: sessionID}
	if err := c.callWithCap(ctx, methodCaptureRunDelta, args, &res, captureTimeout); err != nil {
		// The RPC is authoritative on failure (dial failure, child failure) —
		// discard whatever, if anything, crossed the stream, and stop it from
		// continuing to read toward CaptureMaxBytes on a result nobody will
		// use. Closing the listener first fails a still-pending Accept
		// immediately; the receive below then resolves at once either way
		// (Accept had already succeeded, or just failed because we closed
		// the listener) and closes an already-accepted conn, unblocking a
		// Read that would otherwise ride out the full cap or deadline. No
		// goroutine leak: acceptedConn and streamDone are both buffered, so
		// the goroutine's sends never block even though nothing here reads
		// streamDone's value.
		_ = l.Close()
		if conn := <-acceptedConn; conn != nil {
			_ = conn.Close()
		}
		return nil, err
	}

	select {
	case stream := <-streamDone:
		if stream.err != nil {
			return nil, fmt.Errorf("capbroker: capture stream: %w", stream.err)
		}
		return stream.buf, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("capbroker: capture stream: %w", ctx.Err())
	}
}
