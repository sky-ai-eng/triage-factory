package agenthost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
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

	// ghResolver is the GitHub credential resolver the host-routed gh
	// methods build their client from (App installation token → org PAT).
	// Built once per Server in NewServer so the token cache is shared
	// across every gh call in the run rather than re-minting per RPC.
	// Tests override it to inject a fake covering both tiers.
	ghResolver ghclient.Resolver

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
		stores:     stores,
		info:       info,
		ghResolver: ghclient.NewResolver(stores.Secrets, stores.GitHubApps, stores.Orgs, stores.Agents, nil),
		shutdown:   make(chan struct{}),
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

// connIOTimeout bounds a single frame's I/O on a connection — reading
// the request frame after accept, and writing the response (or error)
// frame. A client that never sends a frame, or never drains the reply,
// is confused or malicious; either way, no reason to hold the goroutine
// open. The dispatch between the read and the write is NOT bounded by
// this — it gets its own, longer budget (callTimeout) — so a slow DB or
// upstream API call doesn't trip the frame deadline.
const connIOTimeout = 10 * time.Second

// handleConn reads one request, dispatches it, writes one response,
// closes. Any malformed input is responded to with an error frame
// rather than silently dropped — the client has to know the call
// failed.
func (s *Server) handleConn(conn net.Conn) {
	if err := conn.SetDeadline(time.Now().Add(connIOTimeout)); err != nil {
		agenthostLog.Warn("set deadline failed", "error", err)
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

	// Clear the read deadline now that the request is in hand — the
	// dispatch below may make DB or upstream API calls (e.g. a host-side
	// Jira request) that take longer than connIOTimeout. We re-arm a
	// write deadline before writing the response.
	_ = conn.SetReadDeadline(time.Time{})

	// The download method streams a log archive and gets a longer budget so a
	// slow transfer isn't cancelled at callTimeout — mirrors the client cap.
	dispatchTimeout := callTimeout
	if req.Method == methodGithubDownloadArtifact {
		dispatchTimeout = downloadCallTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), dispatchTimeout)
	defer cancel()

	result, err := s.dispatch(ctx, req.Method, req.Args)
	resp := response{}
	if err != nil {
		resp.Error = err.Error()
		// Echo a GitHub HTTP failure's status + body so the sandbox client
		// can rebuild the typed error and the 406/404 fallbacks still fire.
		var he *ghclient.HTTPError
		if errors.As(err, &he) {
			resp.HTTPStatus = he.StatusCode
			resp.HTTPBody = he.Body
		}
		// Tag the pending-review collision so the client rebuilds the typed
		// sentinel (it has no HTTP status to key on — it's a host-side decision).
		if errors.Is(err, ErrPendingReviewCollision) {
			resp.ErrCode = errCodePendingReviewCollision
		}
		// Same for the finalize-review double-call guard.
		if errors.Is(err, ErrReviewAlreadyFinalized) {
			resp.ErrCode = errCodeReviewAlreadyFinalized
		}
	} else if result != nil {
		body, mErr := json.Marshal(result)
		if mErr != nil {
			resp.Error = fmt.Sprintf("agenthost: marshal result for %s: %v", req.Method, mErr)
		} else {
			resp.Result = body
		}
	}

	if err := conn.SetWriteDeadline(time.Now().Add(connIOTimeout)); err != nil {
		agenthostLog.Warn("set write deadline failed", "error", err)
		return
	}
	if err := writeFrame(conn, resp); err != nil {
		agenthostLog.Warn("write response failed", "method", req.Method, "error", err)
	}
}

func (s *Server) sendError(conn net.Conn, msg string) {
	if err := conn.SetWriteDeadline(time.Now().Add(connIOTimeout)); err != nil {
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
	// Share the Server's resolver (and its token cache) across every gh
	// call in this run instead of re-minting per request.
	client.ghResolver = s.ghResolver
	dec := func(dst any) error {
		if len(rawArgs) == 0 {
			return nil
		}
		return json.Unmarshal(rawArgs, dst)
	}

	switch method {
	case methodLookupRun:
		return lookupRunResult{Info: s.info}, nil

	case methodFinalizeReviewDraft:
		var a finalizeReviewDraftArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return emptyResult{}, client.FinalizeReviewDraft(ctx, a.ReviewID, a.Event, a.Body)

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

	case methodTeamTracksRepo:
		var a teamTracksRepoArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		tracks, err := client.TeamTracksRepo(ctx, a.Owner, a.Repo)
		if err != nil {
			return nil, err
		}
		return teamTracksRepoResult{Tracks: tracks}, nil

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

	case methodUpsertArtifact:
		var a upsertArtifactArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		stored, err := client.UpsertArtifact(ctx, a.Artifact)
		if err != nil {
			return nil, err
		}
		return upsertArtifactResult{Artifact: stored}, nil

	// --- jira: build ForSystem host-side, make the REST call, return the
	// result / error. The per-request LocalClient reads the credential
	// from this daemon's (host-side, Vault-backed) stores, so nothing
	// reaches the sandbox but the typed result or the error string. ---

	case methodJiraGetIssue:
		var a jiraKeyArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		issue, err := client.JiraGetIssue(ctx, a.Key)
		if err != nil {
			return nil, err
		}
		return jiraIssueResult{Issue: issue}, nil

	case methodJiraTransitionTo:
		var a jiraTransitionArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return emptyResult{}, client.JiraTransitionTo(ctx, a.Key, a.Status)

	case methodJiraGetTransitions:
		var a jiraKeyArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		transitions, err := client.JiraGetTransitions(ctx, a.Key)
		if err != nil {
			return nil, err
		}
		return jiraTransitionsResult{Transitions: transitions}, nil

	case methodJiraAddComment:
		var a jiraCommentArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return emptyResult{}, client.JiraAddComment(ctx, a.Key, a.Body)

	case methodJiraAssignToSelf:
		var a jiraKeyArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return emptyResult{}, client.JiraAssignToSelf(ctx, a.Key)

	case methodJiraUnassign:
		var a jiraKeyArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return emptyResult{}, client.JiraUnassign(ctx, a.Key)

	case methodJiraCreateIssue:
		var a jiraCreateIssueArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		key, err := client.JiraCreateIssue(ctx, a.Project, a.IssueType, a.Summary, a.Description, a.ParentKey, a.Priority)
		if err != nil {
			return nil, err
		}
		return jiraCreateIssueResult{Key: key}, nil

	case methodJiraUpdateIssue:
		var a jiraUpdateIssueArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return emptyResult{}, client.JiraUpdateIssue(ctx, a.Key, a.Fields)

	case methodJiraSetParent:
		var a jiraSetParentArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return emptyResult{}, client.JiraSetParent(ctx, a.Key, a.ParentKey)

	case methodJiraGetChildIssues:
		var a jiraKeyArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		issues, err := client.JiraGetChildIssues(ctx, a.Key)
		if err != nil {
			return nil, err
		}
		return jiraIssuesResult{Issues: issues}, nil

	case methodJiraSearchIssues:
		var a jiraSearchArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		issues, err := client.JiraSearchIssues(ctx, a.JQL, a.Fields, a.MaxResults)
		if err != nil {
			return nil, err
		}
		return jiraIssuesResult{Issues: issues}, nil

	case methodJiraListPriorities:
		priorities, err := client.JiraListPriorities(ctx)
		if err != nil {
			return nil, err
		}
		return jiraPrioritiesResult{Priorities: priorities}, nil

	case methodJiraSetPriority:
		var a jiraSetPriorityArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return emptyResult{}, client.JiraSetPriority(ctx, a.Key, a.Priority)

	case methodJiraListIssueTypes:
		var a jiraProjectArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		types, err := client.JiraListIssueTypes(ctx, a.Project)
		if err != nil {
			return nil, err
		}
		return jiraIssueTypesResult{IssueTypes: types}, nil

	// --- github: build the org-tiered client host-side (App→PAT), make the
	// REST call, return the result / error. The per-request LocalClient
	// resolves the credential from this daemon's (host-side, Vault-backed)
	// stores, so nothing reaches the sandbox but the typed result or the
	// error (with its HTTP status preserved for the fallback paths). ---

	case methodGithubGetPR:
		var a githubGetPRArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		pr, err := client.GithubGetPR(ctx, a.Owner, a.Repo, a.Number, a.Verbose)
		if err != nil {
			return nil, err
		}
		return githubPRViewResult{PR: pr}, nil

	case methodGithubGetPRDiff:
		var a githubPRDiffArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		diff, err := client.GithubGetPRDiff(ctx, a.Owner, a.Repo, a.Number, a.File)
		if err != nil {
			return nil, err
		}
		return githubPRDiffResult{Diff: diff}, nil

	case methodGithubGetPRFiles:
		var a githubPRFilesArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		files, err := client.GithubGetPRFiles(ctx, a.Owner, a.Repo, a.Number)
		if err != nil {
			return nil, err
		}
		return githubPRFilesResult{Files: files}, nil

	case methodGithubGetCommentThread:
		var a githubCommentThreadArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		thread, err := client.GithubGetCommentThread(ctx, a.Owner, a.Repo, a.CommentID, a.Page)
		if err != nil {
			return nil, err
		}
		return githubCommentThreadResult{Thread: thread}, nil

	case methodGithubGetReviewDetail:
		var a githubReviewDetailArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		detail, err := client.GithubGetReviewDetail(ctx, a.Owner, a.Repo, a.Number, a.ReviewID, a.Verbose)
		if err != nil {
			return nil, err
		}
		return githubReviewDetailResult{Detail: detail}, nil

	case methodGithubDismissReview:
		var a githubDismissReviewArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return emptyResult{}, client.GithubDismissReview(ctx, a.Owner, a.Repo, a.Number, a.ReviewID, a.Message)

	case methodGithubSubmitReview:
		var a githubSubmitReviewArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		id, event, err := client.GithubSubmitReview(ctx, a.Owner, a.Repo, a.Number, a.CommitSHA, a.Event, a.Body, a.Comments)
		if err != nil {
			return nil, err
		}
		return githubSubmitReviewResult{ReviewID: id, Event: event}, nil

	case methodGithubCreatePR:
		var a githubCreatePRArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		number, htmlURL, nodeID, err := client.GithubCreatePR(ctx, a.Owner, a.Repo, a.Head, a.Base, a.Title, a.Body, a.Draft)
		if err != nil {
			return nil, err
		}
		return githubCreatePRResult{Number: number, HTMLURL: htmlURL, NodeID: nodeID}, nil

	case methodGithubCreatePendingReview:
		var a githubCreatePendingReviewArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		reviewID, err := client.GithubCreatePendingReview(ctx, a.Owner, a.Repo, a.Number, a.CommitSHA, a.Comments, a.Fresh)
		if err != nil {
			return nil, err
		}
		return githubReviewIDResult{ReviewID: reviewID}, nil

	case methodGithubAddPendingReviewComment:
		var a githubAddPendingReviewCommentArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		commentID, err := client.GithubAddPendingReviewComment(ctx, a.Owner, a.Repo, a.ReviewID, a.Path, a.Body, a.Line, a.StartLine)
		if err != nil {
			return nil, err
		}
		return githubCommentIDStringResult{CommentID: commentID}, nil

	case methodGithubGetPendingReview:
		var a githubGetPendingReviewArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		reviewID, comments, err := client.GithubGetPendingReview(ctx, a.Owner, a.Repo, a.Number)
		if err != nil {
			return nil, err
		}
		return githubGetPendingReviewResult{ReviewID: reviewID, Comments: comments}, nil

	case methodGithubUpdatePendingReviewComment:
		var a githubUpdatePendingReviewCommentArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return emptyResult{}, client.GithubUpdatePendingReviewComment(ctx, a.Owner, a.Repo, a.CommentID, a.Body)

	case methodGithubDeletePendingReviewComment:
		var a githubDeletePendingReviewCommentArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return emptyResult{}, client.GithubDeletePendingReviewComment(ctx, a.Owner, a.Repo, a.CommentID)

	case methodGithubAddComment:
		var a githubAddCommentArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		id, err := client.GithubAddComment(ctx, a.Owner, a.Repo, a.Number, a.Body)
		if err != nil {
			return nil, err
		}
		return githubCommentIDResult{CommentID: id}, nil

	case methodGithubReplyToComment:
		var a githubReplyToCommentArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		id, err := client.GithubReplyToComment(ctx, a.Owner, a.Repo, a.Number, a.CommentID, a.Body)
		if err != nil {
			return nil, err
		}
		return githubCommentIDResult{CommentID: id}, nil

	case methodGithubReactToComment:
		var a githubReactToCommentArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return emptyResult{}, client.GithubReactToComment(ctx, a.Owner, a.Repo, a.CommentID, a.Emoji)

	case methodGithubUpdateComment:
		var a githubUpdateCommentArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return emptyResult{}, client.GithubUpdateComment(ctx, a.Owner, a.Repo, a.CommentID, a.Body)

	case methodGithubDeleteComment:
		var a githubDeleteCommentArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return emptyResult{}, client.GithubDeleteComment(ctx, a.Owner, a.Repo, a.CommentID)

	case methodGithubAPIGet:
		var a githubAPIGetArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		data, err := client.GithubAPIGet(ctx, a.Owner, a.Repo, a.Path)
		if err != nil {
			return nil, err
		}
		return githubAPIGetResult{Data: data}, nil

	case methodGithubDownloadArtifact:
		var a githubDownloadArtifactArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		// Buffer host-side: the blob can't stream over the one-shot frame,
		// so collect the (capped) body and hand the bytes back for the
		// sandbox to write into its worktree. Clamp the caller's cap to a
		// frame-safe size so an oversized archive errors cleanly up front
		// rather than downloading in full and overflowing the response frame.
		maxBytes := a.MaxBytes
		if maxBytes > maxIPCArtifactBytes {
			maxBytes = maxIPCArtifactBytes
		}
		var buf bytes.Buffer
		if _, err := client.GithubDownloadArtifact(ctx, a.Owner, a.Repo, a.Path, &buf, maxBytes); err != nil {
			return nil, err
		}
		return githubDownloadArtifactResult{Data: buf.Bytes()}, nil

	default:
		return nil, fmt.Errorf("%w: %s", ErrUnknownMethod, method)
	}
}
