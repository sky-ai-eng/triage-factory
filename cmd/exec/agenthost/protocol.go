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

// maxIPCArtifactBytes caps a GithubDownloadArtifact body on the IPC path so
// the buffered blob fits in one response frame. The bytes marshal to a base64
// JSON string (~4/3 expansion), so the raw cap is held well under
// maxFrameSize*3/4 (12 MiB) to leave room for the envelope. The daemon clamps
// the caller's maxBytes (up to 500 MB from gh actions) to this before
// downloading, so an oversized archive fails fast with a clean "artifact too
// large" error instead of downloading in full and then blowing the frame.
// Realistic CI log archives are a few MB; larger ones fall back to per-job
// logs (smaller) or list-runs discovery. Local mode streams straight to disk
// and is unaffected — this clamp lives only on the daemon's IPC dispatch.
const maxIPCArtifactBytes = 11 * 1024 * 1024 // 11 MiB raw → ~14.7 MiB base64

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
//
// ErrCode is the second structured-error channel: a stable string tag for a
// daemon-side sentinel error that the client must reconstruct as a typed value
// (not just a message), where there's no HTTP status to key on. Today the only
// value is errCodeReviewAlreadyFinalized, mapped back to ErrReviewAlreadyFinalized
// so the finalize-review double-call guard stays errors.Is-able across the wire.
// Empty means "no typed sentinel" — the common case.
//
// ErrCode is only meaningful alongside a non-empty Error: the client inspects it
// inside the `Error != ""` branch (an error response always sets Error first,
// then any marker). A daemon that ever sets ErrCode without Error would have the
// client miss the reconstruction — so the two are set together in server.go.
type response struct {
	Result     json.RawMessage `json:"r,omitempty"`
	Error      string          `json:"e,omitempty"`
	HTTPStatus int             `json:"hs,omitempty"`
	HTTPBody   string          `json:"hb,omitempty"`
	ErrCode    string          `json:"ec,omitempty"`
}

// errCodeReviewAlreadyFinalized is the response.ErrCode marker mechanism for
// ErrReviewAlreadyFinalized — the finalize-review double-call guard — so the
// sandbox client rebuilds the typed sentinel and exec emits the same "your work
// is done, stop calling finalize-review" message on both paths.
const errCodeReviewAlreadyFinalized = "review_already_finalized"

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

type lookupConversationResult struct {
	Info ConversationInfo `json:"info"`
}

type finalizeReviewDraftArgs struct {
	ReviewID string `json:"review_id"`
	Event    string `json:"event"`
	Body     string `json:"body"`
}

// finalizeReviewDraftResult carries which of the two things finalize did:
// staged the draft for human approval, or submitted it to GitHub under the
// team's posture. Posted=false leaves URL empty — nothing exists on
// GitHub to link to yet.
type finalizeReviewDraftResult struct {
	Posted bool   `json:"posted"`
	URL    string `json:"url,omitempty"`
}

type getConversationResult struct {
	Conversation *domain.Conversation `json:"conversation,omitempty"`
}

type getTaskArgs struct {
	TaskID string `json:"task_id"`
}

type taskResult struct {
	Task *domain.Task `json:"task,omitempty"`
}

type reposResult struct {
	Repos []domain.Repository `json:"repos"`
}

type getRepoArgs struct {
	RepoID string `json:"repo_id"`
}

type repoResult struct {
	Repo *domain.Repository `json:"repo,omitempty"`
}

type teamTracksRepoArgs struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
}

type teamTracksRepoResult struct {
	Tracks bool `json:"tracks"`
}

type conversationWorktreeByRepoRefArgs struct {
	RepoID string `json:"repo_id"`
	Ref    string `json:"ref"`
}

type conversationWorktreeResult struct {
	Worktree *domain.ConversationWorktree `json:"worktree,omitempty"`
}

type conversationWorktreesResult struct {
	Worktrees []domain.ConversationWorktree `json:"worktrees"`
}

type insertConversationWorktreeArgs struct {
	Row domain.ConversationWorktree `json:"row"`
}

type insertConversationWorktreeResult struct {
	Inserted    bool   `json:"inserted"`
	WinningPath string `json:"winning_path"`
}

type deleteConversationWorktreeByRepoRefArgs struct {
	RepoID string `json:"repo_id"`
	Ref    string `json:"ref"`
}

// workspaceRootsResult carries the run root's two path-namespace views. The
// daemon substitutes the sandbox mount point (/work) as Agent; Host is what
// the host filesystem — and every host-side reader of conversation_worktrees paths —
// knows the same directory as. See Client.WorkspaceRoots.
type workspaceRootsResult struct {
	Host  string `json:"host"`
	Agent string `json:"agent"`
}

// createWorkspaceCheckoutArgs deliberately carries NO clone URLs — the daemon
// re-derives them from the stored repository row / its own PR fetch so a
// sandboxed caller can't steer the host's credential at an arbitrary repo.
type createWorkspaceCheckoutArgs struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	Ref   string `json:"ref,omitempty"`
	PR    int    `json:"pr,omitempty"`
}

// createWorkspaceCheckoutResult is the created checkout's path in HOST view;
// the workspace CLI translates it to the agent view for the `cd` it prints.
type createWorkspaceCheckoutResult struct {
	Path string `json:"path"`
}

type buildAgentFooterArgs struct {
	Kind string `json:"kind"`
}

type buildAgentFooterResult struct {
	Footer string `json:"footer"`
}

// --- artifacts (capture writers) ---

type upsertArtifactArgs struct {
	Artifact domain.Artifact `json:"artifact"`
}

type upsertArtifactResult struct {
	Artifact domain.Artifact `json:"artifact"`
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
	NodeID  string `json:"node_id"`
}

// --- review draft staging (TFAC-494) ---
//
// start-review / add-review-comment are pure local artifact writes routed
// host-side so the sandbox (no DB) reaches them over the socket. CommitSHA pins
// the reviewed commit for the atomic submit at approval; Comments is unused.

type githubCreatePendingReviewArgs struct {
	githubRepoRef
	Number    int                            `json:"number"`
	CommitSHA string                         `json:"commit_sha"`
	Comments  []ghclient.SubmitReviewComment `json:"comments"`
}

type githubReviewIDResult struct {
	ReviewID string `json:"review_id"`
}

// resetReviewDraftArgs carries the PR coordinates `start-review --fresh` resets
// the conversation's local draft for.
type resetReviewDraftArgs struct {
	githubRepoRef
	Number int `json:"number"`
}

// resetReviewDraftResult is the reset draft's handle plus its preserved head SHA
// (both "" when there was no draft to reset), so the CLI echoes the same
// commit_sha shape a normal start-review returns.
type resetReviewDraftResult struct {
	ReviewID  string `json:"review_id"`
	CommitSHA string `json:"commit_sha"`
}

// updateStagedReviewCommentArgs / deleteStagedReviewCommentArgs address one
// comment on the conversation's review draft by its TF-local id (not a repo-scoped op —
// the host resolves the owning draft from the conversation's artifacts).
type updateStagedReviewCommentArgs struct {
	CommentID string `json:"comment_id"`
	Body      string `json:"body"`
}

type deleteStagedReviewCommentArgs struct {
	CommentID string `json:"comment_id"`
}

type githubAddPendingReviewCommentArgs struct {
	githubRepoRef
	ReviewID  string `json:"review_id"`
	Path      string `json:"path"`
	Body      string `json:"body"`
	Line      int    `json:"line"`
	StartLine *int   `json:"start_line,omitempty"`
	// CommitSHA is the worktree HEAD the CLI validated and anchors this comment
	// against. Empty when the CLI had no local checkout — the host then resolves
	// the anchor from the live PR head and validates against the API diff.
	CommitSHA string `json:"commit_sha,omitempty"`
}

type githubCommentIDStringResult struct {
	CommentID string `json:"comment_id"`
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
}

// --- extensions (ee-registered agent-facing CLI verbs) ---
//
// callExtensionArgs/Result are opaque envelopes: Args/Result carry whatever
// JSON shape the registering ee package's handler and CLI runner agree on.
// The daemon never inspects the payload — it just routes (namespace, method,
// args) to callExtension and ships back whatever the handler returns.

type callExtensionArgs struct {
	Namespace string          `json:"namespace"`
	Method    string          `json:"method"`
	Args      json.RawMessage `json:"args,omitempty"`
}

type callExtensionResult struct {
	Result json.RawMessage `json:"result,omitempty"`
}

// emptyArgs is the args type for methods that take no parameters
// (LookupConversation, GetConversation, ListConversationWorktrees, ListRepos). Using an empty
// struct rather than json.RawMessage(nil)
// lets the daemon-side dispatch use the same json.Unmarshal call shape
// for every method without a nil-check.
type emptyArgs struct{}

type emptyResult struct{}

// memoryLoadArgs / memoryLoadResult are the `memory load` wire shapes, shared
// by the IPC method (methodMemoryLoad) and the sidecar relay op (opMemoryLoad)
// — the two hops a sandboxed executor run makes. Result wraps the pointer so a
// miss (nil entity) still round-trips as a well-formed envelope.
type memoryLoadArgs struct {
	Source   string `json:"source"`
	SourceID string `json:"source_id"`
	Limit    int    `json:"limit"`
}

type memoryLoadResult struct {
	Result *MemoryLoadResult `json:"result"`
}

// The method* constants are the wire names, used by both client and
// server, so a rename here is the only edit needed to propagate. That is
// safe because both ends of this socket are one binary generation for any
// given engagement: the sidecar hosting the Server is spawned by the
// orchestrator at bring-up, the jailed client is a bind-mount of the
// broker's own running executable (sandbox.TrustedTFBinaryPath), and a
// restart takes the supervision stream, the cell, and the engagement with
// it — the reaper requeues the row rather than a new orchestrator
// adopting a live cell.
const (
	methodLookupConversation                  = "LookupConversation"
	methodFinalizeReviewDraft                 = "FinalizeReviewDraft"
	methodResetReviewDraft                    = "ResetReviewDraft"
	methodUpdateStagedReviewComment           = "UpdateStagedReviewComment"
	methodDeleteStagedReviewComment           = "DeleteStagedReviewComment"
	methodGetConversation                     = "GetConversation"
	methodAvailableSources                    = "AvailableSources"
	methodGetTask                             = "GetTask"
	methodListRepos                           = "ListRepos"
	methodGetRepo                             = "GetRepo"
	methodTeamTracksRepo                      = "TeamTracksRepo"
	methodGetConversationWorktreeByRepoRef    = "GetConversationWorktreeByRepoRef"
	methodListConversationWorktrees           = "ListConversationWorktrees"
	methodInsertConversationWorktree          = "InsertConversationWorktree"
	methodDeleteConversationWorktreeByRepoRef = "DeleteConversationWorktreeByRepoRef"
	methodWorkspaceRoots                      = "WorkspaceRoots"
	methodCreateWorkspaceCheckout             = "CreateWorkspaceCheckout"
	methodBuildAgentFooter                    = "BuildAgentFooter"

	methodUpsertArtifact = "UpsertArtifact"

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

	methodGithubCreatePendingReview     = "GithubCreatePendingReview"
	methodGithubAddPendingReviewComment = "GithubAddPendingReviewComment"

	methodCallExtension = "CallExtension"

	methodRecordReadTouch = "RecordReadTouch"

	methodMemoryLoad = "MemoryLoad"
)
