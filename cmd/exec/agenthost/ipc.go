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
	deadlineCap := time.Now().Add(callTimeout)
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

func (c *IPCClient) LookupRun(ctx context.Context) (RunInfo, error) {
	var res lookupRunResult
	if err := c.call(ctx, methodLookupRun, emptyArgs{}, &res); err != nil {
		return RunInfo{}, err
	}
	return res.Info, nil
}

func (c *IPCClient) GetPendingReview(ctx context.Context, reviewID string) (*domain.PendingReview, error) {
	var res pendingReviewResult
	if err := c.call(ctx, methodGetPendingReview, byIDArgs{ID: reviewID}, &res); err != nil {
		return nil, err
	}
	return res.Review, nil
}

func (c *IPCClient) CreatePendingReview(ctx context.Context, r domain.PendingReview) error {
	return c.call(ctx, methodCreatePendingReview, createPendingReviewArgs{Review: r}, nil)
}

func (c *IPCClient) DeletePendingReview(ctx context.Context, reviewID string) error {
	return c.call(ctx, methodDeletePendingReview, byIDArgs{ID: reviewID}, nil)
}

func (c *IPCClient) LockReviewSubmission(ctx context.Context, reviewID, body, event string) error {
	return c.call(ctx, methodLockReviewSubmission, lockReviewSubmissionArgs{
		ReviewID: reviewID, Body: body, Event: event,
	}, nil)
}

func (c *IPCClient) AddPendingReviewComment(ctx context.Context, comment domain.PendingReviewComment) error {
	return c.call(ctx, methodAddPendingReviewComment, addCommentArgs{Comment: comment}, nil)
}

func (c *IPCClient) UpdatePendingReviewComment(ctx context.Context, commentID, body string) error {
	return c.call(ctx, methodUpdatePendingReviewComment, updateCommentArgs{ID: commentID, Body: body}, nil)
}

func (c *IPCClient) DeletePendingReviewComment(ctx context.Context, commentID string) error {
	return c.call(ctx, methodDeletePendingReviewComment, byIDArgs{ID: commentID}, nil)
}

func (c *IPCClient) ListPendingReviewComments(ctx context.Context, reviewID string) ([]domain.PendingReviewComment, error) {
	var res listCommentsResult
	if err := c.call(ctx, methodListPendingReviewComments, listCommentsArgs{ReviewID: reviewID}, &res); err != nil {
		return nil, err
	}
	return res.Comments, nil
}

func (c *IPCClient) GetPendingPRByRunID(ctx context.Context) (*domain.PendingPR, error) {
	var res pendingPRResult
	if err := c.call(ctx, methodGetPendingPRByRunID, emptyArgs{}, &res); err != nil {
		return nil, err
	}
	return res.PR, nil
}

func (c *IPCClient) CreateAndLockPendingPR(ctx context.Context, row domain.PendingPR) error {
	return c.call(ctx, methodCreateAndLockPendingPR, createAndLockPendingPRArgs{Row: row}, nil)
}

func (c *IPCClient) LockPendingPR(ctx context.Context, id, title, body string) error {
	return c.call(ctx, methodLockPendingPR, lockPendingPRArgs{ID: id, Title: title, Body: body}, nil)
}

func (c *IPCClient) GetAgentRun(ctx context.Context) (*domain.AgentRun, error) {
	var res agentRunResult
	if err := c.call(ctx, methodGetAgentRun, emptyArgs{}, &res); err != nil {
		return nil, err
	}
	return res.Run, nil
}

func (c *IPCClient) GetTask(ctx context.Context, taskID string) (*domain.Task, error) {
	var res taskResult
	if err := c.call(ctx, methodGetTask, getTaskArgs{TaskID: taskID}, &res); err != nil {
		return nil, err
	}
	return res.Task, nil
}

func (c *IPCClient) ListRepos(ctx context.Context) ([]domain.RepoProfile, error) {
	var res reposResult
	if err := c.call(ctx, methodListRepos, emptyArgs{}, &res); err != nil {
		return nil, err
	}
	return res.Repos, nil
}

func (c *IPCClient) GetRepo(ctx context.Context, repoID string) (*domain.RepoProfile, error) {
	var res repoResult
	if err := c.call(ctx, methodGetRepo, getRepoArgs{RepoID: repoID}, &res); err != nil {
		return nil, err
	}
	return res.Repo, nil
}

func (c *IPCClient) GetRunWorktreeByRepo(ctx context.Context, repoID string) (*domain.RunWorktree, error) {
	var res runWorktreeResult
	if err := c.call(ctx, methodGetRunWorktreeByRepo, runWorktreeByRepoArgs{RepoID: repoID}, &res); err != nil {
		return nil, err
	}
	return res.Worktree, nil
}

func (c *IPCClient) ListRunWorktrees(ctx context.Context) ([]domain.RunWorktree, error) {
	var res runWorktreesResult
	if err := c.call(ctx, methodListRunWorktrees, emptyArgs{}, &res); err != nil {
		return nil, err
	}
	return res.Worktrees, nil
}

func (c *IPCClient) InsertRunWorktree(ctx context.Context, row domain.RunWorktree) (bool, string, error) {
	var res insertRunWorktreeResult
	if err := c.call(ctx, methodInsertRunWorktree, insertRunWorktreeArgs{Row: row}, &res); err != nil {
		return false, "", err
	}
	return res.Inserted, res.WinningPath, nil
}

func (c *IPCClient) DeleteRunWorktreeByRepo(ctx context.Context, repoID string) error {
	return c.call(ctx, methodDeleteRunWorktreeByRepo, deleteRunWorktreeByRepoArgs{RepoID: repoID}, nil)
}

func (c *IPCClient) BuildAgentRunFooter(ctx context.Context, kind string) (string, error) {
	var res buildAgentRunFooterResult
	if err := c.call(ctx, methodBuildAgentRunFooter, buildAgentRunFooterArgs{Kind: kind}, &res); err != nil {
		return "", err
	}
	return res.Footer, nil
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
