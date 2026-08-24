package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
)

// dialTimeout caps how long the client waits for the daemon to accept
// a connection. Set short — the socket is local; if the daemon is
// up, accept happens in microseconds. A long timeout would mask a
// dead-daemon scenario as "slow" instead of failing fast with a clear
// "is the host process alive?" error.
const dialTimeout = 5 * time.Second

// callTimeout caps a single in-flight RPC. The daemon serves every
// method from in-memory or short DB ops; anything longer than this
// is either a wedged DB or a leaked goroutine. Surface as a clear
// timeout error rather than letting the agent stall indefinitely.
const callTimeout = 30 * time.Second

// downloadCallTimeout is the cap for GithubDownloadArtifact, which streams a
// log archive host-side and can legitimately run for minutes on a slow link —
// the underlying github.Client.DownloadArtifact allows 15m. The generic
// callTimeout would cancel those mid-transfer, so the download method gets its
// own longer budget on both the client deadline and the daemon dispatch ctx.
const downloadCallTimeout = 15 * time.Minute

// checkoutCallTimeout is the cap for CreateWorkspaceCheckout, which runs real
// git host-side — a first-touch bare clone of a large repo legitimately takes
// a couple of minutes. Deliberately held UNDER the workspace CLI's 5-minute
// stale-reservation window (cmd/exec/workspace staleReservationAge): a create
// that times out here is dead before its conversation_worktrees reservation becomes
// reclaimable, so a reclaimed slot never races a still-running create.
const checkoutCallTimeout = 4 * time.Minute

// IPCClient is the unix-socket implementation of Client. Each call
// opens its own connection and closes it on return — the daemon's
// handleConn is one-shot (read one frame, write one frame, hang up),
// so reusing a connection across calls would EOF on the second read.
// Unix-socket accept on a local listener is microseconds; the per-
// call dial overhead is irrelevant against any DB op the daemon
// services.
//
// The mutex serializes calls so two goroutines in the same process
// can't interleave on the same client (cmd/exec subcommands are
// single-goroutine, but a future caller might not be).
type IPCClient struct {
	socketPath string

	mu     sync.Mutex
	closed bool
}

// Dial returns an IPCClient bound to socketPath. No connection is
// opened until the first call — each call gets a fresh dial.
func Dial(socketPath string) *IPCClient {
	return &IPCClient{socketPath: socketPath}
}

// Close marks the client closed so subsequent calls fail fast.
// No conn to release — every call is dial-on-demand.
func (c *IPCClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

// call dials the daemon, sends one request, reads one response, and
// closes the connection. Matches the server's one-shot handleConn
// contract — any state on the wire is bounded by a single RPC.
func (c *IPCClient) call(ctx context.Context, method string, args any, result any) error {
	return c.callWithin(ctx, callTimeout, method, args, result)
}

// callWithin is call with an explicit absolute-timeout cap. Most methods use
// callTimeout via call; the download method passes downloadCallTimeout so a
// slow archive transfer isn't cancelled at 30s.
func (c *IPCClient) callWithin(ctx context.Context, timeout time.Duration, method string, args any, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("agenthost: client closed")
	}

	dialer := net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return fmt.Errorf("agenthost: dial %s: %w", c.socketPath, err)
	}
	defer func() { _ = conn.Close() }()

	// Apply ctx and the absolute timeout as a single deadline. Whichever
	// fires first wins. SetDeadline on a unix socket interrupts in-flight
	// I/O cleanly (returns *net.OpError with Timeout()==true).
	deadline, ok := ctx.Deadline()
	deadlineCap := time.Now().Add(timeout)
	if !ok || deadlineCap.Before(deadline) {
		deadline = deadlineCap
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return fmt.Errorf("agenthost: set deadline: %w", err)
	}

	argsJSON, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("agenthost: marshal %s args: %w", method, err)
	}
	req := request{
		Version: ProtocolVersion,
		Method:  method,
		Args:    argsJSON,
	}
	if err := writeFrame(conn, req); err != nil {
		return err
	}

	var resp response
	if err := readFrame(conn, &resp); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("agenthost: daemon closed connection during %s: %w", method, err)
		}
		return err
	}

	if resp.Error != "" {
		// A tagged sentinel (no HTTP status to key on) rebuilds as the typed
		// value so errors.Is matches identically to the in-process path —
		// today only the finalize-review double-call guard.
		if resp.ErrCode == errCodeReviewAlreadyFinalized {
			return ErrReviewAlreadyFinalized
		}
		// A non-zero HTTPStatus marks a host-routed GitHub API failure;
		// rebuild the typed error so status-discriminating callers
		// (IsHTTP406, the download-logs 404 fallback) behave identically to
		// the in-process path. Plain (non-HTTP) errors — every Jira/DB
		// method, resolver outages — keep the bare string.
		if resp.HTTPStatus != 0 {
			return ghclient.NewHTTPError(resp.HTTPStatus, resp.HTTPBody, resp.Error)
		}
		return errors.New(resp.Error)
	}
	if result == nil {
		return nil
	}
	if len(resp.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(resp.Result, result); err != nil {
		return fmt.Errorf("agenthost: decode %s result: %w", method, err)
	}
	return nil
}

// --- Client interface implementation ---

func (c *IPCClient) LookupConversation(ctx context.Context) (ConversationInfo, error) {
	var res lookupConversationResult
	if err := c.call(ctx, methodLookupConversation, emptyArgs{}, &res); err != nil {
		return ConversationInfo{}, err
	}
	return res.Info, nil
}

func (c *IPCClient) FinalizeReviewDraft(ctx context.Context, reviewID, event, body string) (ReviewFinalizeResult, error) {
	var res finalizeReviewDraftResult
	if err := c.call(ctx, methodFinalizeReviewDraft, finalizeReviewDraftArgs{
		ReviewID: reviewID, Event: event, Body: body,
	}, &res); err != nil {
		return ReviewFinalizeResult{}, err
	}
	return ReviewFinalizeResult(res), nil
}

func (c *IPCClient) ResetReviewDraft(ctx context.Context, owner, repo string, number int) (string, string, error) {
	var res resetReviewDraftResult
	if err := c.call(ctx, methodResetReviewDraft, resetReviewDraftArgs{githubRepoRef: githubRepoRef{Owner: owner, Repo: repo}, Number: number}, &res); err != nil {
		return "", "", err
	}
	return res.ReviewID, res.CommitSHA, nil
}

func (c *IPCClient) UpdateStagedReviewComment(ctx context.Context, commentID, body string) error {
	return c.call(ctx, methodUpdateStagedReviewComment, updateStagedReviewCommentArgs{CommentID: commentID, Body: body}, nil)
}

func (c *IPCClient) DeleteStagedReviewComment(ctx context.Context, commentID string) error {
	return c.call(ctx, methodDeleteStagedReviewComment, deleteStagedReviewCommentArgs{CommentID: commentID}, nil)
}

func (c *IPCClient) AvailableSources(ctx context.Context) ([]string, error) {
	var res availableSourcesResult
	if err := c.call(ctx, methodAvailableSources, emptyArgs{}, &res); err != nil {
		return nil, err
	}
	return res.Kinds, nil
}

func (c *IPCClient) GetConversation(ctx context.Context) (*domain.Conversation, error) {
	var res getConversationResult
	if err := c.call(ctx, methodGetConversation, emptyArgs{}, &res); err != nil {
		return nil, err
	}
	return res.Conversation, nil
}

func (c *IPCClient) GetTask(ctx context.Context, taskID string) (*domain.Task, error) {
	var res taskResult
	if err := c.call(ctx, methodGetTask, getTaskArgs{TaskID: taskID}, &res); err != nil {
		return nil, err
	}
	return res.Task, nil
}

func (c *IPCClient) ListRepos(ctx context.Context) ([]domain.Repository, error) {
	var res reposResult
	if err := c.call(ctx, methodListRepos, emptyArgs{}, &res); err != nil {
		return nil, err
	}
	return res.Repos, nil
}

func (c *IPCClient) GetRepo(ctx context.Context, slug string) (*domain.Repository, error) {
	var res repoResult
	if err := c.call(ctx, methodGetRepo, getRepoArgs{RepoID: slug}, &res); err != nil {
		return nil, err
	}
	return res.Repo, nil
}

func (c *IPCClient) TeamTracksRepo(ctx context.Context, owner, repo string) (bool, error) {
	var res teamTracksRepoResult
	if err := c.call(ctx, methodTeamTracksRepo, teamTracksRepoArgs{Owner: owner, Repo: repo}, &res); err != nil {
		return false, err
	}
	return res.Tracks, nil
}

func (c *IPCClient) GetConversationWorktreeByRepoRef(ctx context.Context, repoID, ref string) (*domain.ConversationWorktree, error) {
	var res conversationWorktreeResult
	if err := c.call(ctx, methodGetConversationWorktreeByRepoRef, conversationWorktreeByRepoRefArgs{RepoID: repoID, Ref: ref}, &res); err != nil {
		return nil, err
	}
	return res.Worktree, nil
}

func (c *IPCClient) ListConversationWorktrees(ctx context.Context) ([]domain.ConversationWorktree, error) {
	var res conversationWorktreesResult
	if err := c.call(ctx, methodListConversationWorktrees, emptyArgs{}, &res); err != nil {
		return nil, err
	}
	return res.Worktrees, nil
}

func (c *IPCClient) InsertConversationWorktree(ctx context.Context, row domain.ConversationWorktree) (bool, string, error) {
	var res insertConversationWorktreeResult
	if err := c.call(ctx, methodInsertConversationWorktree, insertConversationWorktreeArgs{Row: row}, &res); err != nil {
		return false, "", err
	}
	return res.Inserted, res.WinningPath, nil
}

func (c *IPCClient) DeleteConversationWorktreeByRepoRef(ctx context.Context, repoID, ref string) error {
	return c.call(ctx, methodDeleteConversationWorktreeByRepoRef, deleteConversationWorktreeByRepoRefArgs{RepoID: repoID, Ref: ref}, nil)
}

// WorkspaceRoots asks the daemon for the run root's two views. This transport
// IS the sandbox boundary, so the agent view comes back as the /work mount
// (the daemon substitutes it) while the host view is what conversation_worktrees rows
// must record. See Client.WorkspaceRoots.
func (c *IPCClient) WorkspaceRoots(ctx context.Context) (string, string, error) {
	var res workspaceRootsResult
	if err := c.call(ctx, methodWorkspaceRoots, emptyArgs{}, &res); err != nil {
		return "", "", err
	}
	return res.Host, res.Agent, nil
}

// CreateWorkspaceCheckout routes the git materialization host-side: the daemon
// builds the checkout into the real host run root (visible in the sandbox
// under /work) and returns its host-view path. Uses the checkout call budget —
// a first-touch clone legitimately outlives the generic 30s cap.
func (c *IPCClient) CreateWorkspaceCheckout(ctx context.Context, owner, repo, ref string, prNumber int) (string, error) {
	var res createWorkspaceCheckoutResult
	if err := c.callWithin(ctx, checkoutCallTimeout, methodCreateWorkspaceCheckout,
		createWorkspaceCheckoutArgs{Owner: owner, Repo: repo, Ref: ref, PR: prNumber}, &res); err != nil {
		return "", err
	}
	return res.Path, nil
}

func (c *IPCClient) BuildAgentFooter(ctx context.Context, kind string) (string, error) {
	var res buildAgentFooterResult
	if err := c.call(ctx, methodBuildAgentFooter, buildAgentFooterArgs{Kind: kind}, &res); err != nil {
		return "", err
	}
	return res.Footer, nil
}

// --- artifacts ---

// UpsertArtifact ships the polymorphic artifact to the daemon, which stamps
// the run identity and upserts it host-side (admin- or app-pool per the run's
// trigger shape). Returns the stored row. See TFAC-460.
func (c *IPCClient) UpsertArtifact(ctx context.Context, a domain.Artifact) (domain.Artifact, error) {
	var res upsertArtifactResult
	if err := c.call(ctx, methodUpsertArtifact, upsertArtifactArgs{Artifact: a}, &res); err != nil {
		return domain.Artifact{}, err
	}
	return res.Artifact, nil
}

// --- jira ---
//
// Each method is a thin RPC: serialize args, ship one frame to the
// daemon, decode one frame back. The daemon constructs the ForSystem
// Jira client host-side and makes the REST call — the sandbox holds no
// credential. Errors surface as the response Error string (set verbatim
// from the daemon's err.Error()), so a workflow-rejected transition or a
// Jira 4xx reaches the caller intact.

func (c *IPCClient) JiraGetIssue(ctx context.Context, key string) (*jiraclient.Issue, error) {
	var res jiraIssueResult
	if err := c.call(ctx, methodJiraGetIssue, jiraKeyArgs{Key: key}, &res); err != nil {
		return nil, err
	}
	return res.Issue, nil
}

func (c *IPCClient) JiraTransitionTo(ctx context.Context, key, status string) error {
	return c.call(ctx, methodJiraTransitionTo, jiraTransitionArgs{Key: key, Status: status}, nil)
}

func (c *IPCClient) JiraGetTransitions(ctx context.Context, key string) ([]jiraclient.Transition, error) {
	var res jiraTransitionsResult
	if err := c.call(ctx, methodJiraGetTransitions, jiraKeyArgs{Key: key}, &res); err != nil {
		return nil, err
	}
	return res.Transitions, nil
}

func (c *IPCClient) JiraAddComment(ctx context.Context, key, body string) error {
	return c.call(ctx, methodJiraAddComment, jiraCommentArgs{Key: key, Body: body}, nil)
}

func (c *IPCClient) JiraAssignToSelf(ctx context.Context, key string) error {
	return c.call(ctx, methodJiraAssignToSelf, jiraKeyArgs{Key: key}, nil)
}

func (c *IPCClient) JiraUnassign(ctx context.Context, key string) error {
	return c.call(ctx, methodJiraUnassign, jiraKeyArgs{Key: key}, nil)
}

func (c *IPCClient) JiraCreateIssue(ctx context.Context, project, issueType, summary, description, parentKey, priority string) (string, error) {
	var res jiraCreateIssueResult
	if err := c.call(ctx, methodJiraCreateIssue, jiraCreateIssueArgs{
		Project:     project,
		IssueType:   issueType,
		Summary:     summary,
		Description: description,
		ParentKey:   parentKey,
		Priority:    priority,
	}, &res); err != nil {
		return "", err
	}
	return res.Key, nil
}

func (c *IPCClient) JiraUpdateIssue(ctx context.Context, key string, fields jiraclient.UpdateIssueFields) error {
	return c.call(ctx, methodJiraUpdateIssue, jiraUpdateIssueArgs{Key: key, Fields: fields}, nil)
}

func (c *IPCClient) JiraSetParent(ctx context.Context, key, parentKey string) error {
	return c.call(ctx, methodJiraSetParent, jiraSetParentArgs{Key: key, ParentKey: parentKey}, nil)
}

func (c *IPCClient) JiraGetChildIssues(ctx context.Context, key string) ([]jiraclient.Issue, error) {
	var res jiraIssuesResult
	if err := c.call(ctx, methodJiraGetChildIssues, jiraKeyArgs{Key: key}, &res); err != nil {
		return nil, err
	}
	return res.Issues, nil
}

func (c *IPCClient) JiraSearchIssues(ctx context.Context, jql string, fields []string, maxResults int) ([]jiraclient.Issue, error) {
	var res jiraIssuesResult
	if err := c.call(ctx, methodJiraSearchIssues, jiraSearchArgs{JQL: jql, Fields: fields, MaxResults: maxResults}, &res); err != nil {
		return nil, err
	}
	return res.Issues, nil
}

func (c *IPCClient) JiraListPriorities(ctx context.Context) ([]jiraclient.Priority, error) {
	var res jiraPrioritiesResult
	if err := c.call(ctx, methodJiraListPriorities, emptyArgs{}, &res); err != nil {
		return nil, err
	}
	return res.Priorities, nil
}

func (c *IPCClient) JiraSetPriority(ctx context.Context, key, priority string) error {
	return c.call(ctx, methodJiraSetPriority, jiraSetPriorityArgs{Key: key, Priority: priority}, nil)
}

func (c *IPCClient) JiraListIssueTypes(ctx context.Context, project string) ([]jiraclient.IssueType, error) {
	var res jiraIssueTypesResult
	if err := c.call(ctx, methodJiraListIssueTypes, jiraProjectArgs{Project: project}, &res); err != nil {
		return nil, err
	}
	return res.IssueTypes, nil
}

// --- github ---
//
// Each method is a thin RPC: serialize args, ship one frame to the daemon,
// decode one frame back. The daemon builds the org-tiered GitHub client
// host-side (App installation token → org PAT) and makes the REST call — the
// sandbox holds no credential. HTTP failures surface as the response Error
// string and, when the daemon flagged a github.HTTPError, as a
// reconstructed typed error (see call()), so the 406/404 fallbacks survive.

func (c *IPCClient) GithubGetPR(ctx context.Context, owner, repo string, number int, verbose bool) (*ghclient.PRView, error) {
	var res githubPRViewResult
	if err := c.call(ctx, methodGithubGetPR, githubGetPRArgs{githubRepoRef: githubRepoRef{Owner: owner, Repo: repo}, Number: number, Verbose: verbose}, &res); err != nil {
		return nil, err
	}
	return res.PR, nil
}

func (c *IPCClient) GithubGetPRDiff(ctx context.Context, owner, repo string, number int, file string) (string, error) {
	var res githubPRDiffResult
	if err := c.call(ctx, methodGithubGetPRDiff, githubPRDiffArgs{githubRepoRef: githubRepoRef{Owner: owner, Repo: repo}, Number: number, File: file}, &res); err != nil {
		return "", err
	}
	return res.Diff, nil
}

func (c *IPCClient) GithubGetPRFiles(ctx context.Context, owner, repo string, number int) ([]ghclient.PRFile, error) {
	var res githubPRFilesResult
	if err := c.call(ctx, methodGithubGetPRFiles, githubPRFilesArgs{githubRepoRef: githubRepoRef{Owner: owner, Repo: repo}, Number: number}, &res); err != nil {
		return nil, err
	}
	return res.Files, nil
}

func (c *IPCClient) GithubGetCommentThread(ctx context.Context, owner, repo string, commentID, page int) (*ghclient.CommentThread, error) {
	var res githubCommentThreadResult
	if err := c.call(ctx, methodGithubGetCommentThread, githubCommentThreadArgs{githubRepoRef: githubRepoRef{Owner: owner, Repo: repo}, CommentID: commentID, Page: page}, &res); err != nil {
		return nil, err
	}
	return res.Thread, nil
}

// RecordReadTouch is best-effort: a transport failure is logged and swallowed,
// never surfaced to the read that triggered it (mirrors the void host-side
// contract). The daemon answers with an empty result.
func (c *IPCClient) RecordReadTouch(ctx context.Context, provider, target, url string) {
	if err := c.call(ctx, methodRecordReadTouch, recordReadTouchArgs{Provider: provider, Target: target, URL: url}, nil); err != nil {
		agenthostLog.Warn("read-touch RPC failed (touch dropped)", "target", target, "error", err)
	}
}

// MemoryLoad ships the load to the daemon, which resolves the entity + reads
// its team-scoped memory + records the touch host-side. A pre-ProtocolVersion
// daemon that predates this method answers with the dispatch default case's
// clean "unknown method" error rather than misbehaving — the reason this
// backward-compatible addition does NOT bump ProtocolVersion.
func (c *IPCClient) MemoryLoad(ctx context.Context, source, sourceID string, limit int) (*MemoryLoadResult, error) {
	var res memoryLoadResult
	if err := c.call(ctx, methodMemoryLoad, memoryLoadArgs{Source: source, SourceID: sourceID, Limit: limit}, &res); err != nil {
		return nil, err
	}
	return res.Result, nil
}

func (c *IPCClient) GithubGetReviewDetail(ctx context.Context, owner, repo string, number, reviewID int, verbose bool) (*ghclient.ReviewDetail, error) {
	var res githubReviewDetailResult
	if err := c.call(ctx, methodGithubGetReviewDetail, githubReviewDetailArgs{githubRepoRef: githubRepoRef{Owner: owner, Repo: repo}, Number: number, ReviewID: reviewID, Verbose: verbose}, &res); err != nil {
		return nil, err
	}
	return res.Detail, nil
}

func (c *IPCClient) GithubDismissReview(ctx context.Context, owner, repo string, number, reviewID int, message string) error {
	return c.call(ctx, methodGithubDismissReview, githubDismissReviewArgs{githubRepoRef: githubRepoRef{Owner: owner, Repo: repo}, Number: number, ReviewID: reviewID, Message: message}, nil)
}

func (c *IPCClient) GithubSubmitReview(ctx context.Context, owner, repo string, number int, commitSHA, event, body string, comments []ghclient.SubmitReviewComment) (int, string, error) {
	var res githubSubmitReviewResult
	if err := c.call(ctx, methodGithubSubmitReview, githubSubmitReviewArgs{githubRepoRef: githubRepoRef{Owner: owner, Repo: repo}, Number: number, CommitSHA: commitSHA, Event: event, Body: body, Comments: comments}, &res); err != nil {
		return 0, "", err
	}
	return res.ReviewID, res.Event, nil
}

func (c *IPCClient) GithubCreatePR(ctx context.Context, owner, repo, head, base, title, body string, draft bool) (int, string, string, error) {
	var res githubCreatePRResult
	if err := c.call(ctx, methodGithubCreatePR, githubCreatePRArgs{githubRepoRef: githubRepoRef{Owner: owner, Repo: repo}, Head: head, Base: base, Title: title, Body: body, Draft: draft}, &res); err != nil {
		return 0, "", "", err
	}
	return res.Number, res.HTMLURL, res.NodeID, nil
}

func (c *IPCClient) GithubCreatePendingReview(ctx context.Context, owner, repo string, number int, commitSHA string, comments []ghclient.SubmitReviewComment) (string, error) {
	var res githubReviewIDResult
	if err := c.call(ctx, methodGithubCreatePendingReview, githubCreatePendingReviewArgs{githubRepoRef: githubRepoRef{Owner: owner, Repo: repo}, Number: number, CommitSHA: commitSHA, Comments: comments}, &res); err != nil {
		return "", err
	}
	return res.ReviewID, nil
}

func (c *IPCClient) GithubAddPendingReviewComment(ctx context.Context, owner, repo, reviewID, path, body string, line int, startLine *int, commitSHA string) (string, error) {
	var res githubCommentIDStringResult
	if err := c.call(ctx, methodGithubAddPendingReviewComment, githubAddPendingReviewCommentArgs{githubRepoRef: githubRepoRef{Owner: owner, Repo: repo}, ReviewID: reviewID, Path: path, Body: body, Line: line, StartLine: startLine, CommitSHA: commitSHA}, &res); err != nil {
		return "", err
	}
	return res.CommentID, nil
}

func (c *IPCClient) GithubAddComment(ctx context.Context, owner, repo string, number int, body string) (int, error) {
	var res githubCommentIDResult
	if err := c.call(ctx, methodGithubAddComment, githubAddCommentArgs{githubRepoRef: githubRepoRef{Owner: owner, Repo: repo}, Number: number, Body: body}, &res); err != nil {
		return 0, err
	}
	return res.CommentID, nil
}

func (c *IPCClient) GithubReplyToComment(ctx context.Context, owner, repo string, number, commentID int, body string) (int, error) {
	var res githubCommentIDResult
	if err := c.call(ctx, methodGithubReplyToComment, githubReplyToCommentArgs{githubRepoRef: githubRepoRef{Owner: owner, Repo: repo}, Number: number, CommentID: commentID, Body: body}, &res); err != nil {
		return 0, err
	}
	return res.CommentID, nil
}

func (c *IPCClient) GithubReactToComment(ctx context.Context, owner, repo string, commentID int, emoji string) error {
	return c.call(ctx, methodGithubReactToComment, githubReactToCommentArgs{githubRepoRef: githubRepoRef{Owner: owner, Repo: repo}, CommentID: commentID, Emoji: emoji}, nil)
}

func (c *IPCClient) GithubUpdateComment(ctx context.Context, owner, repo string, commentID int, body string) error {
	return c.call(ctx, methodGithubUpdateComment, githubUpdateCommentArgs{githubRepoRef: githubRepoRef{Owner: owner, Repo: repo}, CommentID: commentID, Body: body}, nil)
}

func (c *IPCClient) GithubDeleteComment(ctx context.Context, owner, repo string, commentID int) error {
	return c.call(ctx, methodGithubDeleteComment, githubDeleteCommentArgs{githubRepoRef: githubRepoRef{Owner: owner, Repo: repo}, CommentID: commentID}, nil)
}

func (c *IPCClient) GithubAPIGet(ctx context.Context, owner, repo, path string) ([]byte, error) {
	var res githubAPIGetResult
	if err := c.call(ctx, methodGithubAPIGet, githubAPIGetArgs{githubRepoRef: githubRepoRef{Owner: owner, Repo: repo}, Path: path}, &res); err != nil {
		return nil, err
	}
	return res.Data, nil
}

// --- extensions ---

// CallExtension ships (namespace, method, args) to the daemon, which
// resolves the registered handler, checks entitlement, and invokes it (see
// LocalClient.CallExtension / callExtension). An older daemon that predates
// this RPC answers with the dispatch default case's "unknown method" error,
// which surfaces here verbatim — no hang, no panic.
func (c *IPCClient) CallExtension(ctx context.Context, namespace, method string, args json.RawMessage) (json.RawMessage, error) {
	var res callExtensionResult
	if err := c.call(ctx, methodCallExtension, callExtensionArgs{Namespace: namespace, Method: method, Args: args}, &res); err != nil {
		return nil, err
	}
	return res.Result, nil
}

// GithubDownloadArtifact buffers the daemon's reply and copies it into dst.
// The blob can't stream over the one-shot RPC frame, so the host returns the
// full (capped) body and the sandbox writes it into its own worktree. The
// frame cap bounds what fits this way; realistic CI log archives are a few MB.
func (c *IPCClient) GithubDownloadArtifact(ctx context.Context, owner, repo, path string, dst io.Writer, maxBytes int64) (int64, error) {
	var res githubDownloadArtifactResult
	if err := c.callWithin(ctx, downloadCallTimeout, methodGithubDownloadArtifact, githubDownloadArtifactArgs{githubRepoRef: githubRepoRef{Owner: owner, Repo: repo}, Path: path, MaxBytes: maxBytes}, &res); err != nil {
		return 0, err
	}
	if _, err := dst.Write(res.Data); err != nil {
		return 0, fmt.Errorf("agenthost: write downloaded artifact: %w", err)
	}
	// Report bytes actually written to dst, which is the function's contract.
	return int64(len(res.Data)), nil
}
