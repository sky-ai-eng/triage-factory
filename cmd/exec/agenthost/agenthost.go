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

	"github.com/sky-ai-eng/triage-factory/internal/domain"
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
// for the foreign-key columns on writes, and IsEventTriggered for any
// caller that still wants to branch on routing shape (most don't,
// since the routing decision is collapsed into the Client methods
// below).
//
// Mirrors runident.RunIdentity but lives in this package so the IPC
// wire shape doesn't depend on runident's import graph (runident
// imports db, which we don't want every IPC consumer dragging in).
type RunInfo struct {
	OrgID            string `json:"org_id"`
	UserID           string `json:"user_id"`
	RunID            string `json:"run_id"`
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

	// --- pending PRs (gh pr create) ---

	GetPendingPRByRunID(ctx context.Context) (*domain.PendingPR, error)
	CreateAndLockPendingPR(ctx context.Context, row domain.PendingPR) error
	LockPendingPR(ctx context.Context, id, title, body string) error

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

	// Close releases any resources held by the client. LocalClient is
	// a no-op (it doesn't own the DB conn — that's exec.Handle's
	// problem). IPCClient closes the unix socket.
	Close() error
}
