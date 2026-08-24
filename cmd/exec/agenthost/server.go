package agenthost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// strictlyWithin reports whether path is a strict descendant of root — not
// root itself, not a sibling, not an escape via "..", and not anything when
// root is empty. Gate for the create-cleanup RemoveAll above: destructive
// recovery must be provably scoped to the run root.
func strictlyWithin(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// sandboxAgentRoot is the sandbox's view of the run root — the bind-mount
// destination agentproc's sandbox spec puts the run's Cwd at. Mirrors
// agentproc's sandboxAgentRoot (which itself mirrors sandbox/spec.go). The
// daemon only ever serves sandboxed callers, so this is unconditionally the
// agent view its WorkspaceRoots dispatch reports.
const sandboxAgentRoot = "/work"

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
// constructor-supplied ConversationInfo for every method's routing.
type Server struct {
	// rt is the runtime every per-request LocalClient's DB effects route
	// through: directRuntime over db.Stores on all/local (NewServer), or
	// relayRuntime over the supervision channel on the executor sidecar
	// (NewServerWithRuntime). This is what lets the SAME dispatch run in the
	// orchestrator and in the capless per-run jail.
	rt Runtime

	// stores backs the all/local gh/jira credential resolver only; it is the
	// zero db.Stores on the executor sidecar (which holds no store — the gh/jira
	// verbs route through proxyCreds there, so the resolver path is never taken).
	stores db.Stores
	info   ConversationInfo

	// gateWired reports whether the exec-gh repo authz gate can run — true on the
	// sidecar and a fully-wired all/local Server, false for a partial fixture.
	gateWired bool

	// ghResolver is the GitHub credential resolver the host-routed gh
	// methods build their client from (App installation token → org PAT).
	// Built once per Server in NewServer so the token cache is shared
	// across every gh call in the run rather than re-minting per RPC. nil on
	// the executor sidecar (no resolver is constructed there — proxyCreds win,
	// and the secret store the resolver would read is disabled).
	// Tests override it to inject a fake covering both tiers.
	ghResolver ghclient.Resolver

	// proxyCreds points the gh/jira verbs at this run's credential-sidecar REST
	// proxies — always set in multi mode, where the daemon lives in the
	// sidecar; nil in local mode. When set, every gh/jira verb builds a client
	// against the proxy URL holding only a per-run placeholder; the sidecar
	// injects the real credential on the upstream hop, so this daemon holds
	// none. nil resolves through ghResolver / the Jira resolver exactly as
	// before. The coordinates are stable for the run's lifetime (the sidecar's
	// own TokenSource handles any brain refresh-sweep remint behind the proxy).
	proxyCreds *ProxyCredentials

	// upstreamGate bounds per-upstream (Jira / GitHub) in-flight REST calls for
	// this run — a per-run governor on how many concurrent requests one agent
	// makes against the shared org bot identity. nil is a no-op (no throttling);
	// both constructors wire one sized from the environment.
	upstreamGate *upstreamThrottle

	// shutdown signals the accept loop to stop accepting new conns
	// and lets in-flight handlers drain.
	shutdown chan struct{}

	mu       sync.Mutex
	closed   bool
	inflight sync.WaitGroup
}

// NewServer constructs a Server bound to (stores, info). info comes
// from the spawner's per-conversation map — it carries the conversation's owning org
// and the kicking-off user identity (empty for event-triggered conversations).
// proxyCreds is non-nil only for a TF_ROLE=executor run; nil disables the
// proxy branch and every gh/jira verb resolves through ghResolver / the Jira
// resolver exactly as before.
func NewServer(stores db.Stores, info ConversationInfo, proxyCreds *ProxyCredentials) *Server {
	resolver := ghclient.NewResolver(stores.Secrets, stores.GitHubApps, stores.Orgs, stores.Agents, nil)
	rt := newDirectRuntime(stores, info)
	// Share the one resolver with the runtime so the review-posture identity
	// probe reuses the same installation-token cache every gh verb in this
	// daemon already warms, instead of building a second resolver per call.
	rt.ghResolver = resolver
	return &Server{
		rt:           rt,
		stores:       stores,
		info:         info,
		gateWired:    stores.TeamGitHubRepos != nil && stores.ConversationWorktrees != nil,
		ghResolver:   resolver,
		proxyCreds:   proxyCreds,
		upstreamGate: newUpstreamThrottle(maxUpstreamConcurrencyFromEnv()),
		shutdown:     make(chan struct{}),
	}
}

// SetGitHubResolver replaces the daemon's credential resolver, keeping the
// runtime's review-posture identity probe on the same object — the posture
// decision must classify the very credential the gh verbs then use. Test seam
// (a fake tier in place of the real resolver NewServer builds from stores).
func (s *Server) SetGitHubResolver(r ghclient.Resolver) {
	s.ghResolver = r
	if dr, ok := s.rt.(*directRuntime); ok {
		dr.ghResolver = r
	}
}

// NewServerWithRuntime constructs the executor sidecar's Server: every
// per-request LocalClient runs its DB effects over rt (the relay runtime — no
// stores, no DB connection), and the gh/jira verbs route through proxyCreds so
// the real credential stays behind the sidecar's REST proxies. NO credential
// resolver is constructed — the sidecar holds no secret store, and the resolver
// would be both dead (proxyCreds win) and impossible (nil stores). Identity
// comes from the runtime; the repo gate is always wired (the relay serves it).
func NewServerWithRuntime(rt Runtime, proxyCreds *ProxyCredentials) *Server {
	return &Server{
		rt:           rt,
		info:         rt.Info(),
		gateWired:    true,
		proxyCreds:   proxyCreds,
		upstreamGate: newUpstreamThrottle(maxUpstreamConcurrencyFromEnv()),
		shutdown:     make(chan struct{}),
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
		// Recovers via serveConn — every frame this dispatches was composed by
		// the jailed agent, and this goroutine's death is the process's.
		go func() {
			defer s.inflight.Done()
			defer func() { _ = conn.Close() }()
			s.serveConn(conn)
		}()
	}
}

// serveConn is handleConn under a panic guard. It exists because of where this
// accept loop runs: in multi mode the daemon lives in the per-run credential
// sidecar, and the frames it dispatches are composed by the jailed agent — the
// most directly attacker-influenced input in a process that also holds that
// run's credentials and every proxy serving it. An unrecovered panic in this
// goroutine would terminate that whole process; the dispatch fan-out below
// unmarshals agent-supplied arguments across roughly forty verbs, so "no verb
// ever panics" is not a property worth betting the process on. See the
// goroutine rule in cmd/runsidecar's package doc.
//
// Deliberately NO response frame from here. The panic may have unwound
// mid-write and the frame protocol has no partial-frame recovery, so the only
// safe move is to let the deferred Close hand the client an EOF — already a
// defined outcome on this channel, which the IPCClient surfaces as "daemon
// closed connection during <method>" and the agent reads as a failed tool call.
func (s *Server) serveConn(conn net.Conn) {
	// Set by handleConn the moment the request frame decodes, so the guard can
	// name the verb that died; a panic unwinds past every return value, which is
	// why this is an out-parameter rather than a result.
	var method string
	defer func() {
		if r := recover(); r != nil {
			agenthostLog.Error("panic serving exec rpc; connection dropped",
				"method", method, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
		}
	}()
	s.handleConn(conn, &method)
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
	// No recover: the body is a WaitGroup wait and a channel close, neither of
	// which can panic (Wait's own misuse panic requires a negative counter, and
	// Add/Done here are strictly paired around one connection).
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
//
// methodSeen, when non-nil, receives the request's method name as soon as the
// frame decodes, for serveConn's panic guard to name. nil is allowed and means
// "nobody is watching" — a caller that doesn't want the name must not have to
// invent a variable, and a nil here must never become the very failure the
// parameter exists to report.
func (s *Server) handleConn(conn net.Conn, methodSeen *string) {
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

	if methodSeen != nil {
		*methodSeen = req.Method
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
	// The workspace create runs real git (a first-touch bare clone can take a
	// couple of minutes on a big repo) and gets its own budget, also mirrored
	// client-side.
	dispatchTimeout := callTimeout
	switch req.Method {
	case methodGithubDownloadArtifact:
		dispatchTimeout = downloadCallTimeout
	case methodCreateWorkspaceCheckout:
		dispatchTimeout = checkoutCallTimeout
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
		// Tag the finalize-review double-call guard so the client rebuilds the typed
		// sentinel (it has no HTTP status to key on — it's a host-side decision).
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
	// Bound this run's concurrent calls to the shared upstream identity before
	// doing any work: an over-cap call waits here until a slot frees or the
	// dispatch deadline fires (graceful degradation, not an error into the
	// agent). DB/core methods are upstreamNone and acquire instantly.
	if up := upstreamForMethod(method); up != upstreamNone {
		release, err := s.upstreamGate.acquire(ctx, up)
		if err != nil {
			return nil, err
		}
		defer release()
	}

	// The per-request LocalClient shares the Server's runtime (its DB effects go
	// direct-to-stores on all/local, or relay to the orchestrator on the
	// sidecar), its resolver + token cache (all/local only; nil on the sidecar),
	// and its proxyCreds (executor only — the gh/jira verbs then build clients
	// against the sidecar's REST proxies holding only placeholders). stores backs
	// the resolver path alone and is the zero value on the sidecar.
	client := &LocalClient{
		stores:     s.stores,
		info:       s.info,
		rt:         s.rt,
		ghResolver: s.ghResolver,
		proxyCreds: s.proxyCreds,
		gateWired:  s.gateWired,
	}
	// Seed the audit credential before any verb runs. On the sidecar this is the
	// tier it reported off the sealed bundle, so a write that records without
	// ever resolving a repo client is still attributed; on all/local it is empty
	// and the resolver settles it at the first resolution.
	if s.proxyCreds != nil {
		client.SetGitHubCredential(s.proxyCreds.GitHubCredential)
	}
	dec := func(dst any) error {
		if len(rawArgs) == 0 {
			return nil
		}
		return json.Unmarshal(rawArgs, dst)
	}

	switch method {
	case methodLookupConversation:
		return lookupConversationResult{Info: s.info}, nil

	case methodAvailableSources:
		kinds, err := client.AvailableSources(ctx)
		if err != nil {
			return nil, err
		}
		return availableSourcesResult{Kinds: kinds}, nil

	case methodFinalizeReviewDraft:
		var a finalizeReviewDraftArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		fin, err := client.FinalizeReviewDraft(ctx, a.ReviewID, a.Event, a.Body)
		if err != nil {
			return nil, err
		}
		return finalizeReviewDraftResult(fin), nil

	case methodResetReviewDraft:
		var a resetReviewDraftArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		reviewID, commitSHA, err := client.ResetReviewDraft(ctx, a.Owner, a.Repo, a.Number)
		if err != nil {
			return nil, err
		}
		return resetReviewDraftResult{ReviewID: reviewID, CommitSHA: commitSHA}, nil

	case methodUpdateStagedReviewComment:
		var a updateStagedReviewCommentArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return emptyResult{}, client.UpdateStagedReviewComment(ctx, a.CommentID, a.Body)

	case methodDeleteStagedReviewComment:
		var a deleteStagedReviewCommentArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return emptyResult{}, client.DeleteStagedReviewComment(ctx, a.CommentID)

	case methodGetConversation:
		conv, err := client.GetConversation(ctx)
		if err != nil {
			return nil, err
		}
		return getConversationResult{Conversation: conv}, nil

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

	case methodGetConversationWorktreeByRepoRef:
		var a conversationWorktreeByRepoRefArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		w, err := client.GetConversationWorktreeByRepoRef(ctx, a.RepoID, a.Ref)
		if err != nil {
			return nil, err
		}
		return conversationWorktreeResult{Worktree: w}, nil

	case methodListConversationWorktrees:
		w, err := client.ListConversationWorktrees(ctx)
		if err != nil {
			return nil, err
		}
		return conversationWorktreesResult{Worktrees: w}, nil

	case methodInsertConversationWorktree:
		var a insertConversationWorktreeArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		inserted, winningPath, err := client.InsertConversationWorktree(ctx, a.Row)
		if err != nil {
			return nil, err
		}
		return insertConversationWorktreeResult{Inserted: inserted, WinningPath: winningPath}, nil

	case methodDeleteConversationWorktreeByRepoRef:
		var a deleteConversationWorktreeByRepoRefArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return emptyResult{}, client.DeleteConversationWorktreeByRepoRef(ctx, a.RepoID, a.Ref)

	case methodWorkspaceRoots:
		// The transport IS the namespace boundary: an RPC arriving here came
		// from inside the sandbox, whose view of the host run root is always
		// the /work bind mount — so the daemon substitutes that as the agent
		// view rather than the LocalClient's same-namespace answer.
		host, _, err := client.WorkspaceRoots(ctx)
		if err != nil {
			return nil, err
		}
		return workspaceRootsResult{Host: host, Agent: sandboxAgentRoot}, nil

	case methodCreateWorkspaceCheckout:
		var a createWorkspaceCheckoutArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		// The executor's credential sidecar (proxyCreds set) owns neither the
		// shared bare cache nor the run-root — both belong to the orchestrator /
		// jail — so its uid can't materialize a checkout. Relay the whole create
		// to the orchestrator, which owns them and clones through the run's git
		// proxy exactly like the eager-PR path. all/local (proxyCreds nil) owns
		// the FS and materializes in-process. The orchestrator serves the relayed
		// op via RelayServer, never through this method, so proxyCreds here
		// unambiguously means "this is the sidecar."
		if client.proxyCreds != nil {
			var res createWorkspaceCheckoutResult
			if err := client.rt.Relay(ctx, agentproc.RelayNamespaceCore, opCreateWorkspaceCheckout, a, &res); err != nil {
				return nil, err
			}
			return res, nil
		}
		path, err := client.materializeWorkspaceCheckout(ctx, a.Owner, a.Repo, a.Ref, a.PR)
		if err != nil {
			return nil, err
		}
		return createWorkspaceCheckoutResult{Path: path}, nil

	case methodBuildAgentFooter:
		var a buildAgentFooterArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		footer, err := client.BuildAgentFooter(ctx, a.Kind)
		if err != nil {
			return nil, err
		}
		return buildAgentFooterResult{Footer: footer}, nil

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
		reviewID, err := client.GithubCreatePendingReview(ctx, a.Owner, a.Repo, a.Number, a.CommitSHA, a.Comments)
		if err != nil {
			return nil, err
		}
		return githubReviewIDResult{ReviewID: reviewID}, nil

	case methodGithubAddPendingReviewComment:
		var a githubAddPendingReviewCommentArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		commentID, err := client.GithubAddPendingReviewComment(ctx, a.Owner, a.Repo, a.ReviewID, a.Path, a.Body, a.Line, a.StartLine, a.CommitSHA)
		if err != nil {
			return nil, err
		}
		return githubCommentIDStringResult{CommentID: commentID}, nil

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

	// --- extensions: route to the same entitlement-gated CallExtension the
	// LocalClient exposes in-process, so a verb author never has to duplicate
	// the gate for the sandbox transport. ---

	case methodRecordReadTouch:
		var a recordReadTouchArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		// Best-effort touch for an addressed read the CLI couldn't key host-side
		// (gh pr thread-view). Routes through the same runtime the in-method
		// reads use, so it relays to the orchestrator on the sidecar.
		client.RecordReadTouch(ctx, a.Provider, a.Target, a.URL)
		return emptyResult{}, nil

	case methodMemoryLoad:
		var a memoryLoadArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		res, err := client.MemoryLoad(ctx, a.Source, a.SourceID, a.Limit)
		if err != nil {
			return nil, err
		}
		return memoryLoadResult{Result: res}, nil

	case methodCallExtension:
		var a callExtensionArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		result, err := client.CallExtension(ctx, a.Namespace, a.Method, a.Args)
		if err != nil {
			return nil, err
		}
		return callExtensionResult{Result: result}, nil

	default:
		return nil, fmt.Errorf("%w: %s", ErrUnknownMethod, method)
	}
}
