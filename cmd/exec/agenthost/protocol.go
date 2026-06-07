package agenthost

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
)

// Wire format: length-prefixed JSON frames.
//
//	┌──────────────┬─────────────┐
//	│ uint32 BE    │ length bytes │
//	│ frame length │ JSON payload │
//	└──────────────┴─────────────┘
//
// One frame in, one frame out per call. The protocol is intentionally
// the smallest thing that works: no streaming, no multiplexing, no
// keep-alive — a fresh connection per RPC. The agenthost socket lives
// for at most the lifetime of one delegated run; serializing on a
// single conn per call avoids any concurrency bookkeeping in the
// daemon and matches what the cmd/exec process actually wants (one
// shell invocation = one RPC).
//
// Choice of length-prefixed JSON over net/rpc + jsonrpc codec: same
// dependency surface (stdlib only), but no method-registration ritual
// and the frame is trivially inspectable in tcpdump / strace. The
// frame length cap below protects the daemon against a malformed
// client sending a 4GB header.
const maxFrameSize = 16 * 1024 * 1024 // 16 MiB; pending review comments etc. easily fit

// request is the envelope for every RPC. Method identifies the
// operation; Args is the method-specific payload (JSON-encoded). The
// daemon dispatches on Method and unmarshals Args into the matching
// argv shape.
//
// Version is the protocol version the client is built for. The daemon
// rejects requests with a mismatching version so an old binary can't
// silently misinterpret a new method's args. Live deployments will
// usually be lock-step but defensive matters because the sandbox
// bind-mounts the host binary — a rolling upgrade where the host
// daemon advanced before the worker's binary refreshes is a real
// scenario.
type request struct {
	Version uint32          `json:"v"`
	Method  string          `json:"m"`
	Args    json.RawMessage `json:"a,omitempty"`
}

// response wraps either a method-specific Result (success) or an
// Error string (failure). The daemon never returns a partially-
// populated response — exactly one of Result / Error is set.
//
// Error is a plain string rather than a typed error code because the
// only consumer is cmd/exec subcommands that surface the message
// verbatim to the agent via stderr; adding code-based routing would
// be premature.
//
// HTTPStatus / HTTPBody are the one structured-error exception: when a
// host-routed GitHub API call fails with a *github.HTTPError, the daemon
// echoes the status code and response body alongside Error so the sandbox
// client can rebuild the typed error (github.NewHTTPError). That's what
// keeps the diff-too-large 406 fallback and the download-logs 404 fallback
// working over the RPC — they discriminate on status, and a bare string
// would erase it. Zero HTTPStatus means "not an HTTP error" (the common
// case, including every Jira/DB method) and the client returns a plain
// errors.New(Error).
type response struct {
	Result     json.RawMessage `json:"r,omitempty"`
	Error      string          `json:"e,omitempty"`
	HTTPStatus int             `json:"hs,omitempty"`
	HTTPBody   string          `json:"hb,omitempty"`
}

// writeFrame serializes msg as a length-prefixed JSON frame on w.
// JSON marshal failures are returned without writing anything — the
// caller can still send a follow-up error frame.
func writeFrame(w io.Writer, msg any) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("agenthost: marshal frame: %w", err)
	}
	if len(body) > maxFrameSize {
		return fmt.Errorf("agenthost: frame %d bytes exceeds cap %d", len(body), maxFrameSize)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)))
	if _, err := w.Write(header[:]); err != nil {
		return fmt.Errorf("agenthost: write frame header: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("agenthost: write frame body: %w", err)
	}
	return nil
}

// readFrame reads one length-prefixed JSON frame from r and decodes
// it into dst. EOF on the header read is returned verbatim so callers
// (the daemon's accept loop in particular) can tell a clean connection
// close apart from a malformed frame.
func readFrame(r io.Reader, dst any) error {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		// io.ReadFull returns io.EOF when zero bytes are read and the
		// stream is closed cleanly; io.ErrUnexpectedEOF when some bytes
		// were read first. Surface EOF as-is so the accept-loop can
		// detect graceful close; everything else wraps for context.
		if errors.Is(err, io.EOF) {
			return io.EOF
		}
		return fmt.Errorf("agenthost: read frame header: %w", err)
	}
	length := binary.BigEndian.Uint32(header[:])
	if length > maxFrameSize {
		return fmt.Errorf("agenthost: frame %d bytes exceeds cap %d", length, maxFrameSize)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return fmt.Errorf("agenthost: read frame body: %w", err)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("agenthost: decode frame: %w", err)
	}
	return nil
}

// --- per-method argv shapes ---
//
// Kept in this file (rather than fanned out per concern) so the wire
// format lives in one place. Adding a method = appending one struct
// here + one case in dispatch.go + one method on IPCClient.

type lookupRunResult struct {
	Info RunInfo `json:"info"`
}

type byIDArgs struct {
	ID string `json:"id"`
}

type pendingReviewResult struct {
	Review *domain.PendingReview `json:"review,omitempty"`
}

type createPendingReviewArgs struct {
	Review domain.PendingReview `json:"review"`
}

type lockReviewSubmissionArgs struct {
	ReviewID string `json:"review_id"`
	Body     string `json:"body"`
	Event    string `json:"event"`
}

type addCommentArgs struct {
	Comment domain.PendingReviewComment `json:"comment"`
}

type updateCommentArgs struct {
	ID   string `json:"id"`
	Body string `json:"body"`
}

type listCommentsArgs struct {
	ReviewID string `json:"review_id"`
}

type listCommentsResult struct {
	Comments []domain.PendingReviewComment `json:"comments"`
}

type pendingPRResult struct {
	PR *domain.PendingPR `json:"pr,omitempty"`
}

type createAndLockPendingPRArgs struct {
	Row domain.PendingPR `json:"row"`
}

type lockPendingPRArgs struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

type agentRunResult struct {
	Run *domain.AgentRun `json:"run,omitempty"`
}

type getTaskArgs struct {
	TaskID string `json:"task_id"`
}

type taskResult struct {
	Task *domain.Task `json:"task,omitempty"`
}

type reposResult struct {
	Repos []domain.RepoProfile `json:"repos"`
}

type getRepoArgs struct {
	RepoID string `json:"repo_id"`
}

type repoResult struct {
	Repo *domain.RepoProfile `json:"repo,omitempty"`
}

type runWorktreeByRepoArgs struct {
	RepoID string `json:"repo_id"`
}

type runWorktreeResult struct {
	Worktree *domain.RunWorktree `json:"worktree,omitempty"`
}

type runWorktreesResult struct {
	Worktrees []domain.RunWorktree `json:"worktrees"`
}

type insertRunWorktreeArgs struct {
	Row domain.RunWorktree `json:"row"`
}

type insertRunWorktreeResult struct {
	Inserted    bool   `json:"inserted"`
	WinningPath string `json:"winning_path"`
}

type deleteRunWorktreeByRepoArgs struct {
	RepoID string `json:"repo_id"`
}

type buildAgentRunFooterArgs struct {
	Kind string `json:"kind"`
}

type buildAgentRunFooterResult struct {
	Footer string `json:"footer"`
}

// --- jira (exec jira ticket ...) ---
//
// Args/result envelopes for the host-routed Jira surface. The daemon
// (or the in-process LocalClient) builds the org's bot-attributed Jira
// client via ForSystem and makes the REST call; these structs are just
// the wire shapes. The jiraclient result types (Issue, Transition,
// Priority, IssueType) already carry JSON tags, so they cross the wire
// unchanged. Method errors (a workflow-rejected transition, a 4xx from
// the Jira API) surface as the response Error string verbatim, so the
// agent reads the same actionable message it would in local mode.

type jiraKeyArgs struct {
	Key string `json:"key"`
}

type jiraProjectArgs struct {
	Project string `json:"project"`
}

type jiraTransitionArgs struct {
	Key    string `json:"key"`
	Status string `json:"status"`
}

type jiraCommentArgs struct {
	Key  string `json:"key"`
	Body string `json:"body"`
}

type jiraCreateIssueArgs struct {
	Project     string `json:"project"`
	IssueType   string `json:"issue_type"`
	Summary     string `json:"summary"`
	Description string `json:"description"`
	ParentKey   string `json:"parent_key"`
	Priority    string `json:"priority"`
}

type jiraUpdateIssueArgs struct {
	Key    string                       `json:"key"`
	Fields jiraclient.UpdateIssueFields `json:"fields"`
}

type jiraSetParentArgs struct {
	Key       string `json:"key"`
	ParentKey string `json:"parent_key"`
}

type jiraSetPriorityArgs struct {
	Key      string `json:"key"`
	Priority string `json:"priority"`
}

type jiraSearchArgs struct {
	JQL        string   `json:"jql"`
	Fields     []string `json:"fields,omitempty"`
	MaxResults int      `json:"max_results"`
}

type jiraIssueResult struct {
	Issue *jiraclient.Issue `json:"issue,omitempty"`
}

type jiraIssuesResult struct {
	Issues []jiraclient.Issue `json:"issues"`
}

type jiraTransitionsResult struct {
	Transitions []jiraclient.Transition `json:"transitions"`
}

type jiraCreateIssueResult struct {
	Key string `json:"key"`
}

type jiraPrioritiesResult struct {
	Priorities []jiraclient.Priority `json:"priorities"`
}

type jiraIssueTypesResult struct {
	IssueTypes []jiraclient.IssueType `json:"issue_types"`
}

// --- github (exec gh pr / actions ...) ---
//
// Args/result envelopes for the host-routed GitHub surface. The daemon
// (or the in-process LocalClient) builds the org's authenticated client via
// the resolver's ClientForRepo (App installation token → org PAT) and makes
// the REST call; these structs are just the wire shapes. The ghclient result
// types (PRView, PRFile, CommentThread, ReviewDetail) already carry JSON
// tags, so they cross the wire unchanged. owner/repo ride on every request so
// the host resolves the right per-repo credential tier; the sandbox holds no
// token. HTTP failures surface as the response Error string AND, via the
// HTTPStatus/HTTPBody fields above, as a reconstructable github.HTTPError.

type githubRepoRef struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
}

type githubGetPRArgs struct {
	githubRepoRef
	Number  int  `json:"number"`
	Verbose bool `json:"verbose"`
}

type githubPRViewResult struct {
	PR *ghclient.PRView `json:"pr,omitempty"`
}

type githubPRDiffArgs struct {
	githubRepoRef
	Number int    `json:"number"`
	File   string `json:"file"`
}

type githubPRDiffResult struct {
	Diff string `json:"diff"`
}

type githubPRFilesArgs struct {
	githubRepoRef
	Number int `json:"number"`
}

type githubPRFilesResult struct {
	Files []ghclient.PRFile `json:"files"`
}

type githubCommentThreadArgs struct {
	githubRepoRef
	CommentID int `json:"comment_id"`
	Page      int `json:"page"`
}

type githubCommentThreadResult struct {
	Thread *ghclient.CommentThread `json:"thread,omitempty"`
}

type githubReviewDetailArgs struct {
	githubRepoRef
	Number   int  `json:"number"`
	ReviewID int  `json:"review_id"`
	Verbose  bool `json:"verbose"`
}

type githubReviewDetailResult struct {
	Detail *ghclient.ReviewDetail `json:"detail,omitempty"`
}

type githubDismissReviewArgs struct {
	githubRepoRef
	Number   int    `json:"number"`
	ReviewID int    `json:"review_id"`
	Message  string `json:"message"`
}

type githubSubmitReviewArgs struct {
	githubRepoRef
	Number    int                            `json:"number"`
	CommitSHA string                         `json:"commit_sha"`
	Event     string                         `json:"event"`
	Body      string                         `json:"body"`
	Comments  []ghclient.SubmitReviewComment `json:"comments"`
}

type githubSubmitReviewResult struct {
	ReviewID int    `json:"review_id"`
	Event    string `json:"event"`
}

type githubCreatePRArgs struct {
	githubRepoRef
	Head  string `json:"head"`
	Base  string `json:"base"`
	Title string `json:"title"`
	Body  string `json:"body"`
	Draft bool   `json:"draft"`
}

type githubCreatePRResult struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
}

type githubAddCommentArgs struct {
	githubRepoRef
	Number int    `json:"number"`
	Body   string `json:"body"`
}

type githubCommentIDResult struct {
	CommentID int `json:"comment_id"`
}

type githubReplyToCommentArgs struct {
	githubRepoRef
	Number    int    `json:"number"`
	CommentID int    `json:"comment_id"`
	Body      string `json:"body"`
}

type githubReactToCommentArgs struct {
	githubRepoRef
	CommentID int    `json:"comment_id"`
	Emoji     string `json:"emoji"`
}

type githubUpdateCommentArgs struct {
	githubRepoRef
	CommentID int    `json:"comment_id"`
	Body      string `json:"body"`
}

type githubDeleteCommentArgs struct {
	githubRepoRef
	CommentID int `json:"comment_id"`
}

type githubAPIGetArgs struct {
	githubRepoRef
	Path string `json:"path"`
}

type githubAPIGetResult struct {
	Data []byte `json:"data"`
}

type githubDownloadArtifactArgs struct {
	githubRepoRef
	Path     string `json:"path"`
	MaxBytes int64  `json:"max_bytes"`
}

type githubDownloadArtifactResult struct {
	Data []byte `json:"data"`
	N    int64  `json:"n"`
}

// emptyArgs is the args type for methods that take no parameters
// (LookupRun, GetPendingPRByRunID, GetAgentRun, ListRunWorktrees,
// ListRepos). Using an empty struct rather than json.RawMessage(nil)
// lets the daemon-side dispatch use the same json.Unmarshal call shape
// for every method without a nil-check.
type emptyArgs struct{}

type emptyResult struct{}

// methodCallNames are the wire-name constants. Used by both client
// and server so a rename here is the only edit needed to propagate.
const (
	methodLookupRun                  = "LookupRun"
	methodGetPendingReview           = "GetPendingReview"
	methodCreatePendingReview        = "CreatePendingReview"
	methodDeletePendingReview        = "DeletePendingReview"
	methodLockReviewSubmission       = "LockReviewSubmission"
	methodAddPendingReviewComment    = "AddPendingReviewComment"
	methodUpdatePendingReviewComment = "UpdatePendingReviewComment"
	methodDeletePendingReviewComment = "DeletePendingReviewComment"
	methodListPendingReviewComments  = "ListPendingReviewComments"
	methodGetPendingPRByRunID        = "GetPendingPRByRunID"
	methodCreateAndLockPendingPR     = "CreateAndLockPendingPR"
	methodLockPendingPR              = "LockPendingPR"
	methodGetAgentRun                = "GetAgentRun"
	methodGetTask                    = "GetTask"
	methodListRepos                  = "ListRepos"
	methodGetRepo                    = "GetRepo"
	methodGetRunWorktreeByRepo       = "GetRunWorktreeByRepo"
	methodListRunWorktrees           = "ListRunWorktrees"
	methodInsertRunWorktree          = "InsertRunWorktree"
	methodDeleteRunWorktreeByRepo    = "DeleteRunWorktreeByRepo"
	methodBuildAgentRunFooter        = "BuildAgentRunFooter"

	methodJiraGetIssue       = "JiraGetIssue"
	methodJiraTransitionTo   = "JiraTransitionTo"
	methodJiraGetTransitions = "JiraGetTransitions"
	methodJiraAddComment     = "JiraAddComment"
	methodJiraAssignToSelf   = "JiraAssignToSelf"
	methodJiraUnassign       = "JiraUnassign"
	methodJiraCreateIssue    = "JiraCreateIssue"
	methodJiraUpdateIssue    = "JiraUpdateIssue"
	methodJiraSetParent      = "JiraSetParent"
	methodJiraGetChildIssues = "JiraGetChildIssues"
	methodJiraSearchIssues   = "JiraSearchIssues"
	methodJiraListPriorities = "JiraListPriorities"
	methodJiraSetPriority    = "JiraSetPriority"
	methodJiraListIssueTypes = "JiraListIssueTypes"

	methodGithubGetPR            = "GithubGetPR"
	methodGithubGetPRDiff        = "GithubGetPRDiff"
	methodGithubGetPRFiles       = "GithubGetPRFiles"
	methodGithubGetCommentThread = "GithubGetCommentThread"
	methodGithubGetReviewDetail  = "GithubGetReviewDetail"
	methodGithubDismissReview    = "GithubDismissReview"
	methodGithubSubmitReview     = "GithubSubmitReview"
	methodGithubCreatePR         = "GithubCreatePR"
	methodGithubAddComment       = "GithubAddComment"
	methodGithubReplyToComment   = "GithubReplyToComment"
	methodGithubReactToComment   = "GithubReactToComment"
	methodGithubUpdateComment    = "GithubUpdateComment"
	methodGithubDeleteComment    = "GithubDeleteComment"
	methodGithubAPIGet           = "GithubAPIGet"
	methodGithubDownloadArtifact = "GithubDownloadArtifact"
)
