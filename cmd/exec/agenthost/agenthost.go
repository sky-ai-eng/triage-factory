// Package agenthost is the seam every `triagefactory exec ...`
// subcommand uses to reach Triage Factory state. The interface keeps
// the per-subcommand control flow identical across two implementations:
//
//   - LocalClient: opens the same SQLite DB the binary already opens
//     and applies the synthetic-claims / admin-pool routing the
//     subcommands used to inline. This is the only path used when no
//     /run/tf.sock is bind-mounted into the process — i.e. every local
//     user today, and every test that doesn't spin up the IPC daemon.
//
//   - IPCClient: dials a per-run unix socket bind-mounted by the
//     spawner in the sandbox branch (see internal/agentproc.Run). The
//     socket file's fs permissions ARE the credential — one socket per
//     run, owned by the sandbox UID, mode 0600. The agent inside the
//     sandbox cannot reach a host DB or the keychain; routing every
//     state mutation back through this socket is how it acts on behalf
//     of its own run identity without ever holding tokens itself.
//
// AutoDetect is the single entry point. Subcommands call it once at
// the top of their dispatch and forward the returned Client to the
// action body. AutoDetect returns LocalClient when the socket is
// absent (local mode and non-sandbox multi mode), and IPCClient when
// the socket is present. It fails closed when the socket exists but
// the daemon doesn't respond — silently downgrading to LocalClient
// would route writes through the *binary's* identity-resolution path
// instead of the daemon's, which in multi-mode means the wrong org.
//
// The interface intentionally mirrors the existing store-method surface
// 1:1 rather than introducing higher-level operations. That keeps the
// subcommand bodies (gh pr submit-review, workspace add, etc.)
// byte-identical in shape to what they did before — the only change at
// each call site is `stores.X.Foo(...)` → `client.Foo(...)`, with the
// routing branch (synthetic-claims vs admin pool) collapsed into the
// LocalClient body.
package agenthost

import (
	"context"
	"errors"
	"io"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
)

// DefaultSocketPath is the in-sandbox bind-mount destination for the
// per-run unix socket. The host side creates the listener at
// /run/tf/<run_id>.sock (see internal/agentproc) and bind-mounts it
// here; from the sandbox's perspective there's exactly one socket
// and its path is fixed. AutoDetect probes this path.
const DefaultSocketPath = "/run/tf.sock"

// ProtocolVersion is the wire-format version. Bumped on any
// incompatible change to RPC request/response shape. The daemon
// rejects mismatching client versions so an old binary talking to
// a new daemon (or vice versa) surfaces a clear error rather than
// silently misbehaving.
const ProtocolVersion = 1

// RunInfo is what LookupRun returns. Carries the routing-relevant
// fields a subcommand needs to know about its own run — orgID for
// every per-row read, userID for the synthetic-claims tx path, RunID
// for the foreign-key columns on writes, TeamID for the artifact
// writers' NOT-NULL team_id stamp, and IsEventTriggered for any
// caller that still wants to branch on routing shape (most don't,
// since the routing decision is collapsed into the Client methods
// below).
//
// TeamID is the run's owning team (runs.team_id, NOT NULL). The capture
// writers (TFAC-459 Jira, TFAC-460 pre-push, GitHub-native rework) stamp
// artifacts.team_id off it (NOT NULL per TFAC-455 F1), so it must be
// populated on every construction path: the spawner reads it off the run
// row (no task hop), and the local resolver carries it from
// RunIdentity. Empty only on synthetic RunInfos that don't back a real
// run (test seams).
//
// Mirrors runident.RunIdentity but lives in this package so the IPC
// wire shape doesn't depend on runident's import graph (runident
// imports db, which we don't want every IPC consumer dragging in).
type RunInfo struct {
	OrgID            string `json:"org_id"`
	UserID           string `json:"user_id"`
	RunID            string `json:"run_id"`
	TeamID           string `json:"team_id"`
	IsEventTriggered bool   `json:"is_event_triggered"`
}

// ErrDaemonUnreachable is returned by AutoDetect when /run/tf.sock
// exists but the daemon is dead or unresponsive. Surfaces as a clear
// "sandbox daemon down" error in subcommand stderr rather than
// silently falling back to LocalClient (which would route through the
// wrong identity in multi-mode).
var ErrDaemonUnreachable = errors.New("agenthost: /run/tf.sock exists but daemon is not responding")

// ErrUnknownMethod is returned by the daemon when a client requests
// a method it doesn't know. Surfaces as a "TF binary / daemon version
// mismatch" hint in subcommand stderr.
var ErrUnknownMethod = errors.New("agenthost: unknown method")

// ErrProtocolVersion is returned by the daemon on a version-mismatch
// handshake. The client compares its ProtocolVersion against the
// daemon's reply; mismatches abort the request with this error.
var ErrProtocolVersion = errors.New("agenthost: protocol version mismatch")

// ErrPendingReviewCollision is returned by GithubCreatePendingReview when a
// pending review already exists on the PR for the acting identity AND that
// identity is a real-user PAT (not a GitHub App / service account) — i.e. the
// existing review might be a human's in-progress work the bot must not touch.
// The App/service-account case reuses the existing review instead (it is the
// bot's own from a prior run). exec (TFAC-463) turns this typed error into the
// actionable "configure a GitHub App / service-account credential so the bot
// manages its own review" message. It crosses the IPC boundary as a marker on
// the response envelope so the sandbox client reconstructs the same sentinel
// the in-process path returns (errors.Is matches on both paths).
var ErrPendingReviewCollision = errors.New("agenthost: a pending review already exists on this PR under a real-user PAT identity; refusing to modify it (use a GitHub App / service-account credential so the bot manages its own review)")

// Client is the surface every cmd/exec/* subcommand consumes. Every
// state-mutating operation that used to call stores.X.Y directly with
// inline synthetic-claims / admin-pool routing lives here as a single
// method; the implementation (LocalClient or IPCClient) handles the
// branch.
//
// Reads also flow through this surface — in sandbox mode the agent
// process has no DB at all, so any direct stores.X.GetSystem call would
// fail. Pulling reads into the Client keeps the contract symmetric
// across both implementations and avoids the trap of "works in local,
// dies in sandbox."
type Client interface {
	// LookupRun returns the run identity the client is acting on
	// behalf of. The daemon resolves identity from the socket's per-
	// run map; LocalClient resolves from TRIAGE_FACTORY_RUN_ID at
	// construction time. Idempotent and cheap — the LocalClient
	// returns its cached value, the IPCClient does one round-trip.
	LookupRun(ctx context.Context) (RunInfo, error)

	// --- pending reviews (gh pr review-* + add-review-comment + submit-review) ---

	GetPendingReview(ctx context.Context, reviewID string) (*domain.PendingReview, error)
	CreatePendingReview(ctx context.Context, r domain.PendingReview) error
	DeletePendingReview(ctx context.Context, reviewID string) error
	LockReviewSubmission(ctx context.Context, reviewID, body, event string) error
	AddPendingReviewComment(ctx context.Context, c domain.PendingReviewComment) error
	UpdatePendingReviewComment(ctx context.Context, commentID, body string) error
	DeletePendingReviewComment(ctx context.Context, commentID string) error
	ListPendingReviewComments(ctx context.Context, reviewID string) ([]domain.PendingReviewComment, error)

	// --- workspace (workspace add + list) ---

	GetAgentRun(ctx context.Context) (*domain.AgentRun, error)
	GetTask(ctx context.Context, taskID string) (*domain.Task, error)
	ListRepos(ctx context.Context) ([]domain.RepoProfile, error)
	GetRepo(ctx context.Context, repoID string) (*domain.RepoProfile, error)
	GetRunWorktreeByRepo(ctx context.Context, repoID string) (*domain.RunWorktree, error)
	ListRunWorktrees(ctx context.Context) ([]domain.RunWorktree, error)
	InsertRunWorktree(ctx context.Context, row domain.RunWorktree) (inserted bool, winningPath string, err error)
	DeleteRunWorktreeByRepo(ctx context.Context, repoID string) error

	// BuildAgentRunFooter returns the "*This <kind> was partially
	// generated by AI...*" markdown footer the gh pr standalone-mode
	// paths pre-apply to bodies before posting to GitHub. The delegated/
	// preview path skips this — the server applies the footer at human-
	// approval submit time — but the human-runs-the-CLI-directly path
	// exercises it. kind is "PR" or "review"; anything else passes
	// through unchanged into the footer text.
	//
	// On the interface so the IPC sandbox path can produce a footer
	// without the agent process touching the DB directly. The daemon
	// reads its own AgentRunStore for the lookup.
	BuildAgentRunFooter(ctx context.Context, kind string) (string, error)

	// --- artifacts (capture writers: pre-push hook, ...) ---
	//
	// UpsertArtifact records one durable run artifact (a pushed branch, a
	// Jira write, a GitHub action) at the host-side choke point. The
	// caller supplies the polymorphic fields (Provider/Kind/Target/State/
	// DedupKey/...); the client stamps run_id/org_id/team_id off the run
	// identity, so the agent never has to know them. Routed admin-pool for
	// event-triggered runs and synthetic-claims for manual runs, exactly
	// like the pending-PR writer. Returns the stored row; best-effort
	// callers (the pre-push hook) ignore it. See TFAC-460.
	UpsertArtifact(ctx context.Context, a domain.Artifact) (domain.Artifact, error)

	// --- jira (exec jira ticket ...) ---
	//
	// These route the agent's in-sandbox `exec jira` calls host-side:
	// the daemon builds the org's system Jira client (ForSystem) and
	// makes the REST call, so the sandbox never loads a Jira credential
	// — no keychain read, no dbus. Always bot-attributed; there is no
	// per-user routing in the jail (user-attributed Jira writes are the
	// server-side handlers, never the sandbox). LocalClient builds the
	// same ForSystem client in-process, which is the unchanged local-mode
	// path. Resolver/API errors propagate verbatim so a workflow-rejected
	// transition reaches the agent as an actionable message, not "failed."
	JiraGetIssue(ctx context.Context, key string) (*jiraclient.Issue, error)
	JiraTransitionTo(ctx context.Context, key, status string) error
	JiraGetTransitions(ctx context.Context, key string) ([]jiraclient.Transition, error)
	JiraAddComment(ctx context.Context, key, body string) error
	JiraAssignToSelf(ctx context.Context, key string) error
	JiraUnassign(ctx context.Context, key string) error
	JiraCreateIssue(ctx context.Context, project, issueType, summary, description, parentKey, priority string) (string, error)
	JiraUpdateIssue(ctx context.Context, key string, fields jiraclient.UpdateIssueFields) error
	JiraSetParent(ctx context.Context, key, parentKey string) error
	JiraGetChildIssues(ctx context.Context, key string) ([]jiraclient.Issue, error)
	JiraSearchIssues(ctx context.Context, jql string, fields []string, maxResults int) ([]jiraclient.Issue, error)
	JiraListPriorities(ctx context.Context) ([]jiraclient.Priority, error)
	JiraSetPriority(ctx context.Context, key, priority string) error
	JiraListIssueTypes(ctx context.Context, project string) ([]jiraclient.IssueType, error)

	// --- github (exec gh pr / actions ...) ---
	//
	// These route the agent's in-sandbox `exec gh` GitHub API calls host-
	// side: the daemon builds the org's authenticated client via the
	// resolver's ClientForRepo (App installation token for owner/repo when
	// the grant covers it, else the org PAT) and makes the REST call, so the
	// sandbox never loads a GitHub credential — no keychain read, no dbus,
	// and Property B holds (the jail never holds a token). LocalClient builds
	// the same client in-process via the same resolver, which is the
	// unchanged local-mode path (the keychain read happens on the user's own
	// machine, never in a jail). owner/repo select the credential tier per
	// call. HTTP failures propagate with their status intact (see the
	// response struct's HTTPStatus note) so the 406 diff fallback and the
	// 404 download-logs fallback keep working over the RPC.
	GithubGetPR(ctx context.Context, owner, repo string, number int, verbose bool) (*ghclient.PRView, error)
	GithubGetPRDiff(ctx context.Context, owner, repo string, number int, file string) (string, error)
	GithubGetPRFiles(ctx context.Context, owner, repo string, number int) ([]ghclient.PRFile, error)
	GithubGetCommentThread(ctx context.Context, owner, repo string, commentID, page int) (*ghclient.CommentThread, error)
	GithubGetReviewDetail(ctx context.Context, owner, repo string, number, reviewID int, verbose bool) (*ghclient.ReviewDetail, error)
	GithubDismissReview(ctx context.Context, owner, repo string, number, reviewID int, message string) error
	GithubSubmitReview(ctx context.Context, owner, repo string, number int, commitSHA, event, body string, comments []ghclient.SubmitReviewComment) (int, string, error)
	GithubCreatePR(ctx context.Context, owner, repo, head, base, title, body string, draft bool) (int, string, string, error)
	GithubAddComment(ctx context.Context, owner, repo string, number int, body string) (int, error)
	GithubReplyToComment(ctx context.Context, owner, repo string, number, commentID int, body string) (int, error)
	GithubReactToComment(ctx context.Context, owner, repo string, commentID int, emoji string) error
	GithubUpdateComment(ctx context.Context, owner, repo string, commentID int, body string) error
	GithubDeleteComment(ctx context.Context, owner, repo string, commentID int) error

	// --- github-native pending reviews (gh pr start-review / add-review-comment) ---
	//
	// The GitHub-native preview rework (TFAC-454 Sub-epic B) backs the review
	// editor with a real GitHub *pending* review — private to the acting
	// identity until submitted — instead of a local draft. These three bridge
	// the agent-invoked subset of the TFAC-461 client methods; the server's
	// live-edit/approve path calls github.Client directly and needs no bridge.
	// owner/repo ride on every call so the host resolves the right per-repo
	// credential tier; GithubCreatePR's node_id (above) and these node-ID-keyed
	// ops compose into one preview.
	//
	// GithubCreatePendingReview folds in the start-review collision check, which
	// must run host-side because it branches on the acting credential's identity
	// (App/service-account vs real-user PAT) — knowable only where ClientForRepo
	// resolves the token. It checks for an existing pending review first, then:
	// an App/service-account identity reuses it (the review is the bot's own
	// from a prior run), returning its id; a real-user-PAT identity returns
	// ErrPendingReviewCollision rather than risk hijacking a human's in-progress
	// review. exec stays simple — it gets a review id or the typed error.
	GithubCreatePendingReview(ctx context.Context, owner, repo string, number int, commitSHA string, comments []ghclient.SubmitReviewComment) (reviewID string, err error)
	GithubAddPendingReviewComment(ctx context.Context, owner, repo, reviewID, path, body string, line int, startLine *int) (commentID string, err error)
	GithubGetPendingReview(ctx context.Context, owner, repo string, number int) (reviewID string, comments []ghclient.PendingReviewComment, err error)

	// GithubAPIGet is the raw authenticated GET the actions verbs lean on
	// (workflow-run + job listings have no typed ghclient method). Returns
	// the response body bytes; the sandbox parses them locally.
	GithubAPIGet(ctx context.Context, owner, repo, path string) ([]byte, error)

	// GithubDownloadArtifact streams a large binary blob (a workflow-run log
	// archive or per-job log) to dst, capped at maxBytes. LocalClient streams
	// straight through (no buffering); the IPCClient buffers the host's reply
	// and copies it into dst, so the daemon does the privileged fetch and the
	// sandbox only ever writes into its own bind-mounted worktree. A 404
	// (run still in progress) comes back as a reconstructable github.HTTPError
	// so the per-job fallback fires.
	GithubDownloadArtifact(ctx context.Context, owner, repo, path string, dst io.Writer, maxBytes int64) (int64, error)

	// Close releases any resources held by the client. LocalClient is
	// a no-op (it doesn't own the DB conn — that's exec.Handle's
	// problem). IPCClient closes the unix socket.
	Close() error
}
