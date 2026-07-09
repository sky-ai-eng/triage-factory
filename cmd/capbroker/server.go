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
// user-controlled (no agent I/O crosses this socket in P1), so a generous
// fixed budget is enough; anything longer is a wedged host operation.
const callTimeout = 60 * time.Second

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
	runs   map[string]*runEntry
}

// NewServer constructs a Server dispatching onto ops. Production callers
// pass sandbox.NewHostOps() — the same in-process implementation
// defaultOps uses when TF_PRIVSEP is off.
func NewServer(ops sandbox.PrivilegedOps) *Server {
	baseCtx, cancel := context.WithCancel(context.Background())
	return &Server{
		ops:        ops,
		baseCtx:    baseCtx,
		cancelBase: cancel,
		shutdown:   make(chan struct{}),
		runs:       make(map[string]*runEntry),
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
	if err := readFrame(conn, &req); err != nil {
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
	// on broker Shutdown (which cancels baseCtx). Every other method is a
	// bounded host operation and keeps the callTimeout budget.
	ctx := s.baseCtx
	if req.Method != methodWaitRun {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(s.baseCtx, callTimeout)
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
	_ = writeFrame(conn, resp)
}

func (s *Server) sendError(conn net.Conn, msg string) {
	if err := conn.SetWriteDeadline(time.Now().Add(connIOTimeout)); err != nil {
		return
	}
	_ = writeFrame(conn, response{Error: msg})
}

// dispatch routes one method to s.ops. SetupRunCgroup is the one method
// whose in-process return (a *os.File dir-fd) can't cross the wire — the
// server closes its own copy after extracting the directory path; see
// setupRunCgroupResult's doc in protocol.go and IPCClient.SetupRunCgroup
// in ipc.go, which reopens that same path locally.
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
		state, err := s.ops.SetupNetwork(ctx, a.RunID, a.SubnetIdx)
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

	case methodSetupRunCgroup:
		var a setupRunCgroupArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		dir, f, err := s.ops.SetupRunCgroup(a.Name, a.LimitMB)
		if f != nil {
			_ = f.Close()
		}
		if err != nil {
			return nil, err
		}
		return setupRunCgroupResult{Dir: dir}, nil

	case methodRemoveRunCgroup:
		var a removeRunCgroupArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return emptyResult{}, s.ops.RemoveRunCgroup(a.Dir)

	case methodReapOrphans:
		return emptyResult{}, s.ops.ReapOrphans(ctx)

	case methodLaunchRun:
		var a launchRunArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return s.launchRun(a)

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

	default:
		return nil, fmt.Errorf("capbroker: unknown method %q", method)
	}
}
