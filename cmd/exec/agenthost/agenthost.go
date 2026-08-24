// Package agenthost is the seam every `triagefactory exec ...`
// subcommand uses to reach Triage Factory state. The interface keeps
// the per-subcommand control flow identical across two implementations:
//
//   - LocalClient: opens the same SQLite DB the binary already opens
//     and applies the synthetic-claims / admin-pool routing the
//     subcommands used to inline. This is the path the host CLI uses —
//     i.e. every local user today, and every test that doesn't spin up
//     the IPC daemon.
//
//   - IPCClient: dials a per-run unix socket bind-mounted by the
//     spawner in the sandbox branch (see internal/agentproc.Run). The
//     socket file's fs permissions ARE the credential — one socket per
//     run, owned by the sandbox UID, mode 0600. The agent inside the
//     sandbox cannot reach a host DB or the keychain; routing every
//     state mutation back through this socket is how it acts on behalf
//     of its own run identity without ever holding tokens itself.
//
// There are two constructors, one per boot identity, and the caller
// already knows which it is before it calls either (see main.go's
// identity resolution): NewLocalFromEnv for the host CLI, DialSandbox
// for the jailed CLI. Neither probes for the other's world. The pair
// replaced a single detect-at-last-moment entry point, which was only
// ever necessary because nothing upstream had declared what the process
// was — and which had to guess from socket presence, a signal that
// looks the same for "local CLI" and "jail whose daemon never came up".
//
// The interface intentionally mirrors the existing store-method surface
// 1:1 rather than introducing higher-level operations. That keeps the
// subcommand bodies (gh pr finalize-review, workspace add, etc.)
// byte-identical in shape to what they did before — the only change at
// each call site is `stores.X.Foo(...)` → `client.Foo(...)`, with the
// routing branch (synthetic-claims vs admin pool) collapsed into the
// LocalClient body.
package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
)

// DefaultSocketPath is the in-sandbox bind-mount destination for the
// per-run unix socket. The host side creates the listener at
// /run/tf/<conversation_id>.sock (see internal/agentproc) and bind-mounts it
// here; from the sandbox's perspective there's exactly one socket
// and its path is fixed. DialSandbox is bound to this path.
const DefaultSocketPath = "/run/tf.sock"

// ProtocolVersion is the wire-format version. Bumped on any
// incompatible change to RPC request/response shape. The daemon
// rejects mismatching client versions so an old binary talking to
// a new daemon (or vice versa) surfaces a clear error rather than
// silently misbehaving.
const ProtocolVersion = 1

// ConversationInfo is what LookupConversation returns. Carries the routing-relevant
// fields a subcommand needs to know about its own run — orgID for
// every per-row read, userID for the synthetic-claims tx path, ConversationID
// for the foreign-key columns on writes, TeamID for the artifact
// writers' NOT-NULL team_id stamp, and IsEventTriggered for any
// caller that still wants to branch on routing shape (most don't,
// since the routing decision is collapsed into the Client methods
// below).
//
// TeamID is the conversation's owning team (conversations.team_id — nullable at
// the schema level, but the CHECK constraint requires it non-NULL for every
// team-visibility conversation, which delegation always is). The capture
// writers (TFAC-459 Jira, TFAC-460 pre-push, GitHub-native rework) stamp
// artifacts.team_id off it (NOT NULL per TFAC-455 F1), so it must be
// populated on every construction path: the spawner reads it off the
// conversation row (no task hop), and the local resolver carries it from
// ConversationIdentity. Empty only on synthetic ConversationInfos that don't back a real
// conversation (test seams).
//
// Mirrors convident.ConversationIdentity but lives in this package so the IPC
// wire shape doesn't depend on convident's import graph (convident
// imports db, which we don't want every IPC consumer dragging in).
type ConversationInfo struct {
	OrgID            string `json:"org_id"`
	UserID           string `json:"user_id"`
	ConversationID   string `json:"conversation_id"`
	TeamID           string `json:"team_id"`
	IsEventTriggered bool   `json:"is_event_triggered"`
}

// ErrDaemonUnreachable is returned by DialSandbox when /run/tf.sock
// exists but the daemon is dead or unresponsive. Surfaces as a clear
// "sandbox daemon down" error rather than a local fallback (which
// would route through the wrong identity in multi-mode).
var ErrDaemonUnreachable = errors.New("agenthost: /run/tf.sock exists but daemon is not responding")

// ErrSandboxSocketMissing is returned by DialSandbox when /run/tf.sock is
// absent or is not a socket. It is raised only on the jailed boot identity,
// where nothing else can serve the exec verbs — so the absent socket is an
// outage, never the "this is the host CLI" signal socket absence used to mean
// back when the client was chosen by probing for it.
//
// The wording is aimed at the agent that will read it in a tool result:
// it names the file, names the owner of the problem, and rules out the two
// things an agent would otherwise waste turns investigating — its own
// command and the existence of its run. Failure here is an ordinary failed
// tool call: stderr plus a nonzero exit, no retry, no socket wait.
var ErrSandboxSocketMissing = errors.New("agenthost socket " + DefaultSocketPath +
	" is missing inside the sandbox; the exec verbs cannot run in this jail — " +
	"this is a launch wiring bug, not a problem with your command or the run")

// ErrUnknownMethod is returned by the daemon when a client requests
// a method it doesn't know. Surfaces as a "TF binary / daemon version
// mismatch" hint in subcommand stderr.
var ErrUnknownMethod = errors.New("agenthost: unknown method")

// ErrProtocolVersion is returned by the daemon on a version-mismatch
// handshake. The client compares its ProtocolVersion against the
// daemon's reply; mismatches abort the request with this error.
var ErrProtocolVersion = errors.New("agenthost: protocol version mismatch")

// ErrReviewAlreadyFinalized is returned by FinalizeReviewDraft when the run's
// review artifact already carries a ready sentinel (details_json.review_event is
// set) — i.e. the agent already called finalize-review once. exec turns it into
// the "do not call finalize-review again, your work on this review is complete"
// message that stops agents looping on the hand-off. It crosses the IPC boundary
// as a response-envelope marker so the sandbox client reconstructs the same
// sentinel (errors.Is matches on both the in-process and daemon paths).
//
// Deliberately posture-neutral: the first call may have staged the review for
// approval or posted it, and this error is raised without knowing which, so
// naming an outcome here would mislead whichever way it guessed.
var ErrReviewAlreadyFinalized = errors.New("agenthost: this conversation's review has already been finalized; do not call finalize-review again")

// ReviewFinalizeResult is what FinalizeReviewDraft actually did.
// Posted is false when the draft was staged for human approval — today's
// behavior, and what every staging posture produces — and true when the review
// was submitted to GitHub on finalize, with URL the posted review's deep link.
// URL is empty whenever Posted is false: nothing exists on GitHub to link to.
type ReviewFinalizeResult struct {
	Posted bool
	URL    string
}

// MemoryLoadResult is what MemoryLoad returns: the entity's coordinates plus
// the prior conversation memory attached to it, team-visibility-scoped to the
// conversation's team. EntityID is empty and Memories empty when the entity is unknown (a
// miss is a normal outcome, not an error). Count is the total number of
// memories under the visibility scope BEFORE the limit is applied, so the
// agent can tell "there are more than I asked for" from "that's everything".
type MemoryLoadResult struct {
	Source   string            `json:"source"`
	SourceID string            `json:"source_id"`
	EntityID string            `json:"entity_id,omitempty"` // empty when the entity is unknown
	Title    string            `json:"title,omitempty"`
	Count    int               `json:"count"` // total under visibility scope, pre-limit
	Memories []MemoryLoadEntry `json:"memories"`
}

// MemoryLoadEntry is one prior conversation's memory on the entity — the agent
// narrative composed with the human's post-run verdict, exactly as the
// spawn-time materializer composes it (agent content + a
// "## Human feedback (post-run)" separator).
type MemoryLoadEntry struct {
	ConversationID string    `json:"conversation_id"`
	BlueprintRunID string    `json:"blueprint_run_id,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	Content        string    `json:"content"`
}

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
	// LookupConversation returns the conversation identity the client is acting on
	// behalf of. The daemon resolves identity from the socket's per-
	// conversation map; LocalClient resolves from TRIAGE_FACTORY_CONVERSATION_ID at
	// construction time. Idempotent and cheap — the LocalClient
	// returns its cached value, the IPCClient does one round-trip.
	LookupConversation(ctx context.Context) (ConversationInfo, error)

	// AvailableSources answers which event-source kinds the run's org can
	// currently reach ("github", "jira", registered kinds like "slack") — the
	// availability set the top-level exec help filters its command index on,
	// resolved host-side from the same answer the run's <tools> prompt section
	// was composed from (the claim's stamped tools manifest where one exists,
	// a live resolve otherwise). Documentation only: the gate that actually
	// stops a verb is the credential funnel / extension dispatch it resolves
	// through. Callers treat any error as "unknown" and fall back to the
	// unfiltered surface — over-inclusion is the safe direction.
	AvailableSources(ctx context.Context) ([]string, error)

	// --- review draft finalization (gh pr finalize-review) ---
	//
	// FinalizeReviewDraft finalizes the conversation's fully TF-side `review` draft: it
	// locates the conversation's review artifact, stages body + event, snapshots the
	// agent's draft (body + event + the locally staged inline comments) into
	// details.proposed, and sets the ready sentinel (details_json.review_event).
	//
	// What happens next is the team's review posture, resolved here
	// on the host — the only side holding the team settings and the credential
	// resolver. Under a staging posture the draft is handed to the human
	// approval queue and nothing reaches GitHub (the atomic create+submit runs
	// at approval); under a posting posture the review is submitted right here.
	// The returned ReviewFinalizeResult says which happened, so the CLI reports
	// the outcome rather than the prompt asserting one. Returns
	// ErrReviewAlreadyFinalized when the ready sentinel is already set (the
	// anti-double-submit guard) — under every posture.
	FinalizeReviewDraft(ctx context.Context, reviewID, event, body string) (ReviewFinalizeResult, error)

	// ResetReviewDraft is the host side of `gh pr start-review --fresh`: a pure
	// local reset of this conversation's review draft for owner/repo#number, with zero
	// GitHub calls. It clears the staged comments and the body/event ready
	// sentinel (and the write-once Proposed snapshot) back to an empty pending
	// draft, keeping the artifact row, its handle, and its pinned head SHA so the
	// agent can restart the review cleanly. Returns the draft's handle and that
	// preserved head SHA (so the CLI can echo the same commit_sha a normal
	// start-review prints), or ("", "") when the conversation has no draft for that PR (the
	// caller falls through to a normal start-review). Post-494 there is no GitHub
	// pending review to delete and no identity / anti-hijack logic — the draft
	// lives entirely in the artifact.
	ResetReviewDraft(ctx context.Context, owner, repo string, number int) (reviewID, commitSHA string, err error)

	// --- workspace (workspace add + list) ---

	GetConversation(ctx context.Context) (*domain.Conversation, error)
	GetTask(ctx context.Context, taskID string) (*domain.Task, error)
	ListRepos(ctx context.Context) ([]domain.Repository, error)
	// GetRepo resolves the "owner/repo" the agent typed to its registry row,
	// or nil when nothing answers to that name. The agent surface speaks
	// names; the resolution happens daemon-side, at the runtime.
	GetRepo(ctx context.Context, slug string) (*domain.Repository, error)
	// TeamTracksRepo reports whether the run's team tracks owner/repo — the
	// gate `workspace add` applies (alongside the org-configured check) so it
	// only materializes repos the proxy will then authorize pushes to.
	TeamTracksRepo(ctx context.Context, owner, repo string) (bool, error)
	GetConversationWorktreeByRepoRef(ctx context.Context, repoID, ref string) (*domain.ConversationWorktree, error)
	ListConversationWorktrees(ctx context.Context) ([]domain.ConversationWorktree, error)
	InsertConversationWorktree(ctx context.Context, row domain.ConversationWorktree) (inserted bool, winningPath string, err error)
	DeleteConversationWorktreeByRepoRef(ctx context.Context, repoID, ref string) error

	// WorkspaceRoots returns the run root in both path namespaces: hostRoot is
	// the directory as the HOST filesystem knows it (what conversation_worktrees rows
	// must record, since the push gate and the workspace snapshot read those
	// paths host-side), agentRoot is the same directory as the CALLING process
	// sees it. They differ only across the sandbox boundary — the IPC transport
	// substitutes /work (where the host run root is bind-mounted) as the agent
	// view; in-process LocalClient callers live in the host namespace, so both
	// views are the same string there (TFAC-546).
	WorkspaceRoots(ctx context.Context) (hostRoot, agentRoot string, err error)

	// CreateWorkspaceCheckout materializes the checkout for (owner/repo, ref|pr)
	// into the run's HOST run root and returns the created path in host view —
	// the git work happens host-side in both transports, so a sandboxed
	// `workspace add` gets a checkout the push gate / snapshot / resume can
	// actually see (TFAC-546). ref names an existing branch ("" = the repo's
	// default branch, detached); prNumber > 0 checks out a PR head instead
	// (fork-aware; mutually exclusive with ref). The repo must be
	// org-configured and team-tracked — re-checked host-side, so the RPC
	// carries no clone URLs and cannot point the host's credential at an
	// arbitrary repo. Callers should reserve the conversation_worktrees row BEFORE
	// invoking this (materializeWorkspace's ordering).
	CreateWorkspaceCheckout(ctx context.Context, owner, repo, ref string, prNumber int) (string, error)

	// BuildAgentFooter returns the "*This <kind> was partially
	// generated by AI...*" markdown footer the gh pr standalone-mode
	// paths pre-apply to bodies before posting to GitHub. The delegated/
	// preview path skips this — the server applies the footer at human-
	// approval submit time — but the human-runs-the-CLI-directly path
	// exercises it. kind is "PR" or "review"; anything else passes
	// through unchanged into the footer text.
	//
	// On the interface so the IPC sandbox path can produce a footer
	// without the agent process touching the DB directly. The daemon
	// reads its own ConversationStore for the lookup.
	BuildAgentFooter(ctx context.Context, kind string) (string, error)

	// --- artifacts (capture writers: pre-push hook, ...) ---
	//
	// UpsertArtifact records one durable run artifact (a pushed branch, a
	// Jira write, a GitHub action) at the host-side choke point. The
	// caller supplies the polymorphic fields (Provider/Kind/Target/State/
	// DedupKey/...); the client stamps conversation_id/org_id/team_id off the conversation
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

	// --- review draft staging (gh pr start-review / add-review-comment) ---
	//
	// A review is staged entirely TF-side (TFAC-494) — start-review records a
	// conversation-scoped `review` artifact and add-review-comment appends to its
	// details.staged_comments, both pure local writes with zero GitHub calls. The
	// review never occupies GitHub's one-per-identity pending-review slot during
	// the draft window; the atomic create+submit happens at approval. These bridge
	// those writes host-side so the sandbox (no DB) routes them through the daemon,
	// exactly like UpsertArtifact. owner/repo ride along to build the artifact
	// target; reviewID is the local handle (the artifact id) start-review returns.
	//
	// GithubCreatePendingReview returns the local review handle; the comments param
	// is unused (start-review seeds none). GithubAddPendingReviewComment returns a
	// stable TF-local comment id. Concurrent conversations on one PR never collide — the
	// dedup key is conversation-scoped — so there is no identity branch and no collision
	// error.
	//
	// GithubAddPendingReviewComment's commitSHA is the conversation's worktree HEAD: the
	// commit the CLI validated the comment's (path, line, start_line) against and
	// the commit the submitted comment anchors to. The CLI passes it because the
	// agent reads the diff from its checkout, not the live PR head — sourcing the
	// anchor and the validation from that same HEAD keeps a line the agent saw
	// mapped to the line GitHub anchors to. Empty when the CLI had no checkout, in
	// which case the host anchors to the live head and validates against the API
	// diff (internally consistent on its own).
	GithubCreatePendingReview(ctx context.Context, owner, repo string, number int, commitSHA string, comments []ghclient.SubmitReviewComment) (reviewID string, err error)
	GithubAddPendingReviewComment(ctx context.Context, owner, repo, reviewID, path, body string, line int, startLine *int, commitSHA string) (commentID string, err error)

	// UpdateStagedReviewComment / DeleteStagedReviewComment mutate one inline
	// comment on this conversation's TF-side review draft, addressed by the stable TF-local
	// comment id add-review-comment minted (a non-numeric id, distinct from the
	// REST numeric ids GithubUpdate/DeleteComment take). They rewrite the matching
	// entry in the review artifact's details.staged_comments — a pure local write,
	// no GitHub call until the review submits at approval. These are the staged
	// counterpart of the comment-update / comment-delete REST path; the CLI picks
	// between them by try-parsing the id as an int. UpdateStagedReviewComment's
	// body already carries the (re-baked) severity badge — the CLI bakes it, as it
	// does for add-review-comment. An unknown id is an error (the conversation has no staged
	// comment with that id). These replace the GraphQL pending-review-comment
	// mutations the GitHub-native model used, removed by TFAC-494.
	UpdateStagedReviewComment(ctx context.Context, commentID, body string) error
	DeleteStagedReviewComment(ctx context.Context, commentID string) error

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

	// RecordReadTouch records a durable conversation→entity touch for an addressed read
	// whose host method can't cheaply build the entity target itself — today
	// `gh pr thread-view`, where the PR number the touch keys on is a CLI
	// positional the ghAPI seam (a deliberate mirror of *github.Client, which
	// fetches a thread by comment id) does not carry. The CLI calls this after
	// the read succeeds with the target it does hold. Every other addressed read
	// touches inside its own host method; this is the escape hatch for the one
	// verb that can't. Void + best-effort like the in-method touches — a read
	// never fails on its touch — and, on the sandbox transport, a dropped RPC
	// just costs one touch row.
	RecordReadTouch(ctx context.Context, provider, target, url string)

	// MemoryLoad returns the prior conversation memory attached to the entity identified
	// by (source, sourceID), team-visibility-scoped to the conversation's team. source is
	// an entity source value ("github" | "jira" | "slack"); sourceID is that
	// source's natural key (github "owner/repo#N", jira "KEY-123", slack
	// "<channel>/<thread_ts>"). A lookup of an unknown entity is a miss, not an
	// error: it returns an empty result with no EntityID and records no touch —
	// memory load never mints an entity (unlike the addressed-read touch path).
	// A hit records a conversation→entity 'touched' row best-effort (loading IS an
	// address) and returns up to limit of the most recent memories, with Count
	// the pre-limit total under the visibility scope.
	MemoryLoad(ctx context.Context, source, sourceID string, limit int) (*MemoryLoadResult, error)

	// --- extensions (ee-registered agent-facing CLI verbs) ---

	// CallExtension invokes a registered extension method host-side. The
	// entitlement gate lives in the LocalClient implementation — the single
	// dispatch point both transports route through — so a verb author cannot
	// forget it: unknown namespace → error; feature not held by the run's org
	// → error; otherwise the handler runs with the run's identity.
	CallExtension(ctx context.Context, namespace, method string, args json.RawMessage) (json.RawMessage, error)

	// Close releases any resources held by the client. LocalClient is
	// a no-op (it doesn't own the DB conn — that's exec.Handle's
	// problem). IPCClient closes the unix socket.
	Close() error
}
