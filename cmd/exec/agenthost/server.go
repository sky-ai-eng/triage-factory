package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// Server is the daemon-side counterpart to IPCClient. One Server per
// per-run unix socket: the spawner constructs a Server with the run
// identity baked in, then Serve runs the accept loop until the
// listener closes. There is intentionally no multi-run multiplex —
// "one socket per run" is the security boundary.
//
// The agent inside the sandbox can only see this socket. The socket
// is owned by the sandbox UID (chmod 0600 — see internal/agentproc).
// Any RPC that arrives here is, by construction, acting AS this run;
// the server does not accept identity from the wire and uses its
// constructor-supplied RunInfo for every method's routing.
type Server struct {
	stores db.Stores
	info   RunInfo

	// shutdown signals the accept loop to stop accepting new conns
	// and lets in-flight handlers drain.
	shutdown chan struct{}

	mu       sync.Mutex
	closed   bool
	inflight sync.WaitGroup
}

// NewServer constructs a Server bound to (stores, info). info comes
// from the spawner's per-run map — it carries the run's owning org
// and the kicking-off user identity (empty for event-triggered runs).
func NewServer(stores db.Stores, info RunInfo) *Server {
	return &Server{
		stores:   stores,
		info:     info,
		shutdown: make(chan struct{}),
	}
}

// Serve accepts connections on l and dispatches each one's first
// frame as an RPC. Returns when l is closed (the normal shutdown
// path), or when the accept loop hits an unrecoverable error.
//
// Per-connection handling is one request, one response, then close.
// No keep-alive — the cmd/exec subprocess is short-lived and pays
// roughly nothing for a fresh connect per call. Streaming/multiplexed
// connections would be useful for the future "tail my run output"
// surface but aren't in scope here.
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
			return fmt.Errorf("agenthost server: accept: %w", err)
		}
		s.inflight.Add(1)
		go func() {
			defer s.inflight.Done()
			defer func() { _ = conn.Close() }()
			s.handleConn(conn)
		}()
	}
}

// Shutdown closes the listener (caller does that — Serve returns when
// it does) and waits up to drainTimeout for in-flight handlers to
// complete. In-flight handlers can still write to the DB after
// Shutdown returns — those writes commit on the host process and
// don't need network access. The drain just bounds how long Shutdown
// blocks the caller waiting for clean stops.
//
// Caller pattern in agentproc.Run: defer listener.Close() (unblocks
// Serve) → defer Server.Shutdown(ctx) (waits for handlers).
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.shutdown)
	s.mu.Unlock()

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

// connReadTimeout caps how long the daemon waits for a frame to
// arrive after accept. A connecting client that never sends a frame
// is either confused or malicious; either way, no reason to hold the
// goroutine open.
const connReadTimeout = 10 * time.Second

// handleConn reads one request, dispatches it, writes one response,
// closes. Any malformed input is responded to with an error frame
// rather than silently dropped — the client has to know the call
// failed.
func (s *Server) handleConn(conn net.Conn) {
	if err := conn.SetDeadline(time.Now().Add(connReadTimeout)); err != nil {
		log.Printf("[agenthost] set deadline: %v", err)
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
		s.sendError(conn, fmt.Sprintf("%s: client v%d, daemon v%d", ErrProtocolVersion, req.Version, ProtocolVersion))
		return
	}

	// Clear the read deadline now that the request is in hand —
	// the dispatch below may make DB calls that take longer than
	// connReadTimeout. We re-arm a write deadline before writing
	// the response.
	_ = conn.SetReadDeadline(time.Time{})

	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()

	result, err := s.dispatch(ctx, req.Method, req.Args)
	resp := response{}
	if err != nil {
		resp.Error = err.Error()
	} else if result != nil {
		body, mErr := json.Marshal(result)
		if mErr != nil {
			resp.Error = fmt.Sprintf("agenthost: marshal result for %s: %v", req.Method, mErr)
		} else {
			resp.Result = body
		}
	}

	if err := conn.SetWriteDeadline(time.Now().Add(connReadTimeout)); err != nil {
		log.Printf("[agenthost] set write deadline: %v", err)
		return
	}
	if err := writeFrame(conn, resp); err != nil {
		log.Printf("[agenthost] write response for %s: %v", req.Method, err)
	}
}

func (s *Server) sendError(conn net.Conn, msg string) {
	if err := conn.SetWriteDeadline(time.Now().Add(connReadTimeout)); err != nil {
		return
	}
	_ = writeFrame(conn, response{Error: msg})
}

// dispatch routes one method to the per-run LocalClient. Each method
// unmarshals its args into the matching argv shape, calls into
// LocalClient, and returns the matching result shape. The big switch
// is intentional — it's the wire-to-Go boundary and a future
// generated-from-spec version of this file would just expand to the
// same shape.
func (s *Server) dispatch(ctx context.Context, method string, rawArgs json.RawMessage) (any, error) {
	client := NewLocal(s.stores, s.info)
	dec := func(dst any) error {
		if len(rawArgs) == 0 {
			return nil
		}
		return json.Unmarshal(rawArgs, dst)
	}

	switch method {
	case methodLookupRun:
		return lookupRunResult{Info: s.info}, nil

	case methodGetPendingReview:
		var a byIDArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		r, err := client.GetPendingReview(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		return pendingReviewResult{Review: r}, nil

	case methodCreatePendingReview:
		var a createPendingReviewArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return emptyResult{}, client.CreatePendingReview(ctx, a.Review)

	case methodDeletePendingReview:
		var a byIDArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return emptyResult{}, client.DeletePendingReview(ctx, a.ID)

	case methodLockReviewSubmission:
		var a lockReviewSubmissionArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return emptyResult{}, client.LockReviewSubmission(ctx, a.ReviewID, a.Body, a.Event)

	case methodAddPendingReviewComment:
		var a addCommentArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return emptyResult{}, client.AddPendingReviewComment(ctx, a.Comment)

	case methodUpdatePendingReviewComment:
		var a updateCommentArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return emptyResult{}, client.UpdatePendingReviewComment(ctx, a.ID, a.Body)

	case methodDeletePendingReviewComment:
		var a byIDArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return emptyResult{}, client.DeletePendingReviewComment(ctx, a.ID)

	case methodListPendingReviewComments:
		var a listCommentsArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		c, err := client.ListPendingReviewComments(ctx, a.ReviewID)
		if err != nil {
			return nil, err
		}
		return listCommentsResult{Comments: c}, nil

	case methodGetPendingPRByRunID:
		pr, err := client.GetPendingPRByRunID(ctx)
		if err != nil {
			return nil, err
		}
		return pendingPRResult{PR: pr}, nil

	case methodCreateAndLockPendingPR:
		var a createAndLockPendingPRArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return emptyResult{}, client.CreateAndLockPendingPR(ctx, a.Row)

	case methodLockPendingPR:
		var a lockPendingPRArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return emptyResult{}, client.LockPendingPR(ctx, a.ID, a.Title, a.Body)

	case methodGetAgentRun:
		run, err := client.GetAgentRun(ctx)
		if err != nil {
			return nil, err
		}
		return agentRunResult{Run: run}, nil

	case methodGetTask:
		var a getTaskArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		t, err := client.GetTask(ctx, a.TaskID)
		if err != nil {
			return nil, err
		}
		return taskResult{Task: t}, nil

	case methodListRepos:
		repos, err := client.ListRepos(ctx)
		if err != nil {
			return nil, err
		}
		return reposResult{Repos: repos}, nil

	case methodGetRepo:
		var a getRepoArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		r, err := client.GetRepo(ctx, a.RepoID)
		if err != nil {
			return nil, err
		}
		return repoResult{Repo: r}, nil

	case methodGetRunWorktreeByRepo:
		var a runWorktreeByRepoArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		w, err := client.GetRunWorktreeByRepo(ctx, a.RepoID)
		if err != nil {
			return nil, err
		}
		return runWorktreeResult{Worktree: w}, nil

	case methodListRunWorktrees:
		w, err := client.ListRunWorktrees(ctx)
		if err != nil {
			return nil, err
		}
		return runWorktreesResult{Worktrees: w}, nil

	case methodInsertRunWorktree:
		var a insertRunWorktreeArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		inserted, winningPath, err := client.InsertRunWorktree(ctx, a.Row)
		if err != nil {
			return nil, err
		}
		return insertRunWorktreeResult{Inserted: inserted, WinningPath: winningPath}, nil

	case methodDeleteRunWorktreeByRepo:
		var a deleteRunWorktreeByRepoArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return emptyResult{}, client.DeleteRunWorktreeByRepo(ctx, a.RepoID)

	case methodBuildAgentRunFooter:
		var a buildAgentRunFooterArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		footer, err := client.BuildAgentRunFooter(ctx, a.Kind)
		if err != nil {
			return nil, err
		}
		return buildAgentRunFooterResult{Footer: footer}, nil

	default:
		return nil, fmt.Errorf("%w: %s", ErrUnknownMethod, method)
	}
}
