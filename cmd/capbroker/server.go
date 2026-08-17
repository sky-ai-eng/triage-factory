//go:build linux

package capbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// callTimeout bounds one dispatch — network/rootfs/cgroup setup can touch
// the network stack or the filesystem but never blocks on anything
// user-controlled, so a generous fixed budget is enough; anything longer is
// a wedged host operation. WaitRun is the deliberate exception (it blocks
// until the run exits) and opts out of this budget on both sides. A var
// only so tests can shrink it to prove that opt-out without a 60s wait.
var callTimeout = 60 * time.Second

// captureTimeout bounds one CaptureRunDelta dispatch, AND (mirrored) the
// client-side budget IPCClient.CaptureRunDelta gives the whole streamed
// round trip — the RPC itself, the socket accept, and the capped read all
// share this one budget. Unlike the other host operations it shells out to
// git (a bundle of local-only commits plus a full binary diff) against a
// worktree whose size the run dictates, so it gets a budget sized for a
// large tree rather than a wedged syscall. A var (like callTimeout above)
// so tests can shrink it.
var captureTimeout = 5 * time.Minute

// connIOTimeout bounds a single frame's read/write. A client that never
// sends a frame, or never drains the reply, is confused or malicious;
// there's no reason to hold the goroutine open past this.
const connIOTimeout = 10 * time.Second

// Server is the broker-side RPC dispatcher. One per broker process — the
// broker is a single, long-lived, per-executor daemon (spec §9: "one
// broker process per executor, not one per run"), unlike agenthost's
// one-socket-per-run Server.
type Server struct {
	ops sandbox.PrivilegedOps

	// baseCtx is the parent of every in-flight dispatch's context;
	// cancelBase cancels it. Shutdown calls cancelBase so a long-running,
	// ctx-aware op (SetupNetwork/TeardownNetwork/EnsureRootfs/ReapOrphans
	// all take ctx) unwinds promptly on shutdown instead of running to its
	// full callTimeout budget.
	baseCtx    context.Context
	cancelBase context.CancelFunc

	shutdown chan struct{}
	mu       sync.Mutex
	closed   bool
	inflight sync.WaitGroup

	// runs is the registry of in-flight supervised runtimes, keyed by run
	// id, so a KillRun/WaitRun on a later connection can reach the runsc
	// child a LaunchRun started on an earlier one. Unlike the stateless
	// P1 methods, the runtime launch makes the broker stateful for the
	// lifetime of each run.
	runsMu sync.Mutex
	runs   map[string]*conversationEntry

	// launchSem caps concurrent in-flight LaunchRuns. Buffered to
	// maxInflightLaunches (the subnet-pool size); a full channel makes the
	// next launchRun QUEUE on acquireLaunchSlot instead of piling on
	// privileged setup and exhausting the /16→/24 allocator. Released when a
	// run is reaped. DoS resistance, not a capability boundary — a validated
	// caller still can't escalate; this just stops a runaway one from
	// exhausting the host. One orchestrator maps to one broker, so this is
	// the per-orchestrator cap.
	launchSem chan struct{}
}

// maxInflightLaunches is the per-broker cap on concurrent LaunchRuns. Tied
// to the subnet pool size (sandbox.MaxSandboxes) — the scarcest per-run
// resource — so the broker never accepts more launches than the allocator
// can back. A var so tests can shrink it to exercise the queueing behavior
// without spinning up 256 stand-in runtimes.
var maxInflightLaunches = sandbox.MaxSandboxes

// NewServer constructs a Server dispatching onto ops. Production callers
// pass sandbox.NewHostOps() — the in-process implementation of the
// privileged operations the broker holds capabilities for.
func NewServer(ops sandbox.PrivilegedOps) *Server {
	baseCtx, cancel := context.WithCancel(context.Background())
	return &Server{
		ops:        ops,
		baseCtx:    baseCtx,
		cancelBase: cancel,
		shutdown:   make(chan struct{}),
		runs:       make(map[string]*conversationEntry),
		launchSem:  make(chan struct{}, maxInflightLaunches),
	}
}

// acquireLaunchSlot takes one in-flight launch slot, blocking (queueing) if
// the broker is already at capacity. It respects the dispatch ctx (the
// call budget) and broker shutdown, so a caller that jams the queue is
// eventually thrown a timeout rather than blocking a goroutine forever.
func (s *Server) acquireLaunchSlot(ctx context.Context) error {
	select {
	case s.launchSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("capbroker: launch queue wait: %w", ctx.Err())
	case <-s.baseCtx.Done():
		return fmt.Errorf("capbroker: broker shutting down")
	}
}

// releaseLaunchSlot returns one in-flight launch slot to the pool.
func (s *Server) releaseLaunchSlot() {
	select {
	case <-s.launchSem:
	default:
		// Never happens (every release pairs an acquire); the default guards
		// against a double-release wedging a goroutine on an empty channel.
	}
}

// Serve accepts connections on l and dispatches each one's first frame as
// an RPC. Returns when l is closed (the normal shutdown path via
// Shutdown), or on an unrecoverable accept error.
func (s *Server) Serve(l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			select {
			case <-s.shutdown:
				return nil
			default:
			}
			return fmt.Errorf("capbroker server: accept: %w", err)
		}
		s.inflight.Add(1)
		go func() {
			defer s.inflight.Done()
			defer func() { _ = conn.Close() }()
			s.handleConn(conn)
		}()
	}
}

// Shutdown cancels every in-flight dispatch's context (unwinding any
// ctx-aware privileged op that's still running) and waits up to ctx's
// deadline for in-flight handlers to finish. Caller closes the listener
// separately (unblocks Serve's Accept).
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.shutdown)
	s.mu.Unlock()

	s.cancelBase()

	done := make(chan struct{})
	go func() {
		s.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) handleConn(conn net.Conn) {
	if err := conn.SetDeadline(time.Now().Add(connIOTimeout)); err != nil {
		return
	}

	var req request
	if err := readFrame(conn, &req, maxFrameSize); err != nil {
		if errors.Is(err, io.EOF) {
			return
		}
		s.sendError(conn, fmt.Sprintf("read request: %v", err))
		return
	}

	if req.Version != ProtocolVersion {
		s.sendError(conn, fmt.Sprintf("capbroker: client v%d, broker v%d", req.Version, ProtocolVersion))
		return
	}

	// Clear the read deadline now that the request is in hand — dispatch
	// gets its own, longer budget below so a slow host operation doesn't
	// trip the per-frame I/O deadline.
	_ = conn.SetReadDeadline(time.Time{})

	// WaitRun blocks until the supervised run exits — potentially the whole
	// agent run (minutes, idle hibernation), far past callTimeout — so it
	// runs on baseCtx directly, bounded by the run itself and unwound only
	// on broker Shutdown (which cancels baseCtx). CaptureRunDelta shells
	// out to git (bundle + full binary diff) against a worktree of
	// arbitrary size, so it gets captureTimeout rather than the host-op
	// budget. Every other method is a bounded host operation and keeps
	// the callTimeout budget.
	ctx := s.baseCtx
	if req.Method != methodWaitRun {
		budget := callTimeout
		if req.Method == methodCaptureRunDelta {
			budget = captureTimeout
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(s.baseCtx, budget)
		defer cancel()
	}

	result, err := s.dispatch(ctx, req.Method, req.Args)
	resp := response{}
	if err != nil {
		resp.Error = err.Error()
	} else if result != nil {
		body, mErr := json.Marshal(result)
		if mErr != nil {
			resp.Error = fmt.Sprintf("capbroker: marshal result for %s: %v", req.Method, mErr)
		} else {
			resp.Result = body
		}
	}

	if err := conn.SetWriteDeadline(time.Now().Add(connIOTimeout)); err != nil {
		return
	}
	_ = writeFrame(conn, resp, maxFrameSize)
}

func (s *Server) sendError(conn net.Conn, msg string) {
	if err := conn.SetWriteDeadline(time.Now().Add(connIOTimeout)); err != nil {
		return
	}
	_ = writeFrame(conn, response{Error: msg}, maxFrameSize)
}

// dispatch routes one method to s.ops (the privileged operations) or to one
// of the broker's own handlers (launchRun/waitRun/killRun, which own the
// supervised runtime state, and captureRunDelta, which streams via a
// passed-through socket) — neither delegates to s.ops.
func (s *Server) dispatch(ctx context.Context, method string, rawArgs json.RawMessage) (any, error) {
	dec := func(dst any) error {
		if len(rawArgs) == 0 {
			return nil
		}
		return json.Unmarshal(rawArgs, dst)
	}

	switch method {
	case methodPing:
		return emptyResult{}, nil

	case methodSetupNetwork:
		var a setupNetworkArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		state, err := s.ops.SetupNetwork(ctx, a.ConversationID, a.SubnetIdx)
		if err != nil {
			return nil, err
		}
		return setupNetworkResult{State: state}, nil

	case methodTeardownNetwork:
		var a teardownNetworkArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return emptyResult{}, s.ops.TeardownNetwork(ctx, a.State)

	case methodEnsureRootfs:
		var a ensureRootfsArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		path, err := s.ops.EnsureRootfs(ctx, a.Selector)
		if err != nil {
			return nil, err
		}
		return ensureRootfsResult{Path: path}, nil

	case methodReapOrphans:
		return emptyResult{}, s.ops.ReapOrphans(ctx)

	case methodLaunchRun:
		var a launchRunArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return s.launchRun(ctx, a)

	case methodWaitRun:
		var a waitRunArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return s.waitRun(ctx, a)

	case methodKillRun:
		var a killRunArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return s.killRun(a)

	case methodChownRunTree:
		var a chownRunTreeArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return emptyResult{}, s.ops.ChownRunTree(ctx, a.Root, a.Subpath)

	case methodRemoveRunTree:
		var a removeRunTreeArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return emptyResult{}, s.ops.RemoveRunTree(ctx, a.Path)

	case methodCaptureRunDelta:
		var a captureRunDeltaArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return s.captureRunDelta(ctx, a)

	case methodLaunchSidecar:
		var a launchSidecarArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return s.launchSidecar(ctx, a)

	case methodWaitSidecar:
		// Reuses waitRun's registry logic outright — a sidecar entry lives
		// in the same s.runs map as a run entry, keyed by its own distinct
		// ContainerID, and waiting/draining it needs nothing sidecar-specific.
		var a waitSidecarArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return s.waitRun(ctx, waitRunArgs(a))

	case methodKillSidecar:
		var a killSidecarArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return s.killRun(killRunArgs(a))

	default:
		return nil, fmt.Errorf("capbroker: unknown method %q", method)
	}
}
