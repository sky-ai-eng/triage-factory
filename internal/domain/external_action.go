package domain

import (
	"strings"
	"time"
)

// ExternalAction is one row in external_actions — the append-only audit log of
// record. Each row captures one external write Triage Factory performed under an
// ORG-scoped credential (the org GitHub App / the org Jira service account):
// who, what, when, under which credential, and from→to for a transition.
//
// This is event-grain and immutable, distinct from the mutable, object-grain
// Artifact (which records an object's current state and upserts in place). The
// gate for what lands here is the CREDENTIAL identity used at write time — an
// org-scoped credential is in, an individual user's own credential is out (the
// Jira claim/swipe/done/requeue flows, already attributed natively in Jira).
//
// Wherever the action carries a DB state change (the server approval flips), the
// row is written in the SAME transaction as that change, so the log can't diverge
// from the action. The bot funnels record alongside the artifact upsert under the
// same pool routing. Empty optional strings serialize to SQL NULL (like
// AccessChange). OrgID + ID + OccurredAt are populated on read; Record takes
// orgID separately and lets the column DEFAULTs stamp id + occurred_at. See
// TFAC-483.
type ExternalAction struct {
	ID    string `json:"id"`
	OrgID string `json:"org_id"`
	// TeamID is the action's team (denormalized from the run/artifact). Empty for
	// an action with no team context (a dashboard board-drag on an org-wide PR) →
	// SQL NULL. Backs the team-scoped read.
	TeamID string `json:"team_id,omitempty"`
	// Provider is the external system: github | jira (the Artifact provider
	// consts). A branch push is a github action even though its artifact provider
	// is "git".
	Provider string `json:"provider"`
	// Action is the discriminator — one of the Action* consts below. Free text
	// (extensible — no CHECK constraint on the column).
	Action string `json:"action"`
	// Target is the resource key: owner/repo#123, owner/repo, or a Jira issue key.
	Target string `json:"target"`
	// ExternalID is the provider-native id of the backing object (PR number /
	// review node id / comment id / issue key / branch ref). Empty → SQL NULL.
	ExternalID string `json:"external_id,omitempty"`
	// URL links to the object; empty → SQL NULL.
	URL string `json:"url,omitempty"`
	// FromState / ToState carry a transition's endpoints (a Jira status move, a PR
	// draft→open). Empty for a non-transition action → SQL NULL.
	FromState string `json:"from_state,omitempty"`
	ToState   string `json:"to_state,omitempty"`
	// ConversationID is the producing run — the agent's for a bot/system action, the
	// drafter's for an approval. Empty (→ SQL NULL) for an action with no run (a
	// dashboard board-drag) or after the run is purged (FK ON DELETE SET NULL).
	ConversationID string `json:"conversation_id,omitempty"`
	// ActorUserID is the human authorizer/initiator (the approver, the dragger,
	// the kicking-off user of a manual run). Empty (→ SQL NULL) for an autonomous
	// system action (an event-triggered bot run, the Jira board mirror).
	ActorUserID string `json:"actor_user_id,omitempty"`
	// Credential names the org credential used — one of the Credential* consts.
	Credential string `json:"credential"`
	// DedupKey is the natural per-action key. A branch push carries a deterministic
	// BranchPushDedupKey so the git hook+proxy twin collapses under ON CONFLICT DO
	// NOTHING; every other action leaves this empty and the store fills a uuid, so
	// the row is an unconditional append (a repeated action is never dropped).
	DedupKey string `json:"dedup_key"`
	// DetailJSON carries the action-specific payload (e.g. {"sha":...,"new":true}
	// for a branch push, {"status":...} for a transition). Empty → SQL NULL.
	DetailJSON string    `json:"detail_json,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
}

// External-action discriminators (text, extensible — no CHECK constraint). The
// vocabulary spans the org-credential write surface: the bot's GitHub/Jira
// mutations, the human-authorized GitHub approval lifecycle, and the Jira board
// mirror. See TFAC-483 §2.
const (
	// GitHub PR lifecycle.
	ActionPRCreated          = "pr_created"
	ActionPRMarkedReady      = "pr_marked_ready"
	ActionPRConvertedToDraft = "pr_converted_to_draft"
	ActionPREdited           = "pr_edited"
	ActionPRClosed           = "pr_closed"
	ActionPRReopened         = "pr_reopened"
	ActionPRMerged           = "pr_merged"

	// Auto-merge is arming a merge, not performing one, which is why it is its
	// own pair rather than a flavour of pr_merged: the run that enables it has
	// merged nothing, and the merge it authorizes may land much later, triggered
	// by a green check rather than by any agent.
	ActionPRAutoMergeEnabled  = "pr_auto_merge_enabled"
	ActionPRAutoMergeDisabled = "pr_auto_merge_disabled"

	// Opening a pull request that undoes a merged one. Mechanically a pr_created
	// and in intent something else entirely — undoing work that already landed is
	// among the acts a reader most wants named — so it keeps its own verb, and
	// its row addresses the revert that was opened.
	ActionPRReverted = "pr_reverted"

	// Merging the base branch into a pull request's own branch. It writes to the
	// head ref under the org credential while changing nothing about the pull
	// request itself, which is why it is audited separately from pr_edited.
	ActionPRBranchUpdated = "pr_branch_updated"

	// GitHub review lifecycle. The review *draft* is staged TF-side and makes no
	// GitHub write until the atomic submit at approval (TFAC-494), so the only
	// draft-lifecycle external-action is the submit — there is no review_started
	// (start-review writes nothing) and no review_dismissed (a dismiss is a local
	// state flip, not an org-credential write). The comment edit/delete actions
	// below are unrelated: they audit edits to an already-*submitted* review's
	// comments (real GitHub writes via `gh pr comment-update` / `comment-delete`).
	ActionReviewSubmitted      = "review_submitted"
	ActionReviewCommentEdited  = "review_comment_edited"
	ActionReviewCommentDeleted = "review_comment_deleted"

	// GitHub standalone comments.
	ActionCommentPosted  = "comment_posted"
	ActionCommentEdited  = "comment_edited"
	ActionCommentDeleted = "comment_deleted"

	// GitHub reactions. The target is the object the reaction lands on (a
	// comment, an issue, a PR), never the reaction row itself — an emoji is
	// only interesting as something done TO something.
	ActionReactionAdded   = "reaction_added"
	ActionReactionRemoved = "reaction_removed"

	// Labels applied to or taken off a pull request or an issue — ordinary triage
	// work, and one verb pair for both families because GitHub serves both
	// through the issue endpoint. Defining the repository's label SET is a
	// different thing (repo configuration, not triage) and deliberately has no
	// verb here.
	ActionLabelAdded   = "label_added"
	ActionLabelRemoved = "label_removed"

	// Conversation locking, GitHub's own term for it. One pair for both families
	// for the same reason as labels: a pull request's lock is an issue lock.
	ActionConversationLocked   = "conversation_locked"
	ActionConversationUnlocked = "conversation_unlocked"

	// GitHub Actions. Both spend CI capacity under the org credential, which is
	// why they are audited alongside the content writes.
	ActionWorkflowDispatched   = "workflow_dispatched"
	ActionWorkflowRunCancelled = "workflow_run_cancelled"

	// Git branch push (the one double-capture case — see BranchPushDedupKey).
	ActionBranchPushed = "branch_pushed"

	// A branch GitHub created and linked to an issue. Not a branch_pushed:
	// nothing was pushed and no commit exists yet — what the row records is the
	// link between an issue and the branch meant to implement it.
	ActionLinkedBranchCreated = "linked_branch_created"

	// Git branch push the upstream REFUSED (a non-2xx on the receive-pack
	// POST — auth, protection, or outage; nothing landed). Recorded by the
	// git proxy's outcome capture so the audit log never omits an attempted
	// external write, while the branch artifact is deliberately NOT written
	// (artifacts track only work that exists). detail_json carries
	// {sha, new, http_status}. Appended unconditionally (no dedup key): each
	// failed attempt is its own audit event, and sharing branch_pushed's
	// deterministic key would let a failed attempt swallow the later
	// successful push of the same (run, ref, sha).
	ActionBranchPushFailed = "branch_push_failed"

	// Git operation denied by the per-run least-privilege gate — the git proxy
	// (off-repo / off-ref / non-git path) or the exec-gh channel (off-repo).
	// A security signal, recorded even for a denied read. detail_json carries
	// {op, ref, reason}.
	ActionGitDenied = "git_denied"

	// Outbound network connection refused by the sandbox's gating egress proxy
	// (an off-allowlist host, a non-443 port, a hostname resolving only to
	// internal addresses). Provider is ArtifactProviderNetwork, target is the
	// CONNECT authority (host:port), detail_json carries {target, reason}.
	// Repeated denials collapse per (conversation, host) — see
	// EgressDenialDedupKey — because the signal is that this conversation was
	// refused a destination at all, not how many times it retried.
	ActionEgressDenied = "egress_denied"

	// The fallback for a write the agent made through the real-`gh` credential
	// channel — a mutating REST request whose method+path the shared classifier
	// (internal/ghwrite) does not recognize, or a recognized one the upstream
	// refused. A classified success carries its own semantic action instead (a
	// reply is comment_posted, a merge is pr_merged), so this row means exactly
	// "a write happened here and it has no better name yet".
	//
	// detail_json carries {method, path, http_status}, plus "attempted" naming
	// the semantic action a refused write was reaching for — a 404'd merge is
	// an attempt, never a merge. Appended unconditionally (no dedup key): each
	// attempt is its own event, exactly like ActionBranchPushFailed.
	ActionGHChannelWrite = "gh_channel_write"

	// The same fallback one transport over: a GraphQL write through that
	// channel whose mutation the classifier could not name, or could not read at
	// all. Most of what `gh`'s porcelain writes is GraphQL, so this is the
	// transport where an unrecognized act is likeliest to appear.
	//
	// It is a separate action from the REST fallback because the two know
	// different things. A REST fallback has a path, which is most of an act's
	// identity; this one has whatever the request envelope disclosed, so
	// detail_json carries {operation, mutations, http_status}, "unreadable"
	// naming why no single act could be resolved (an over-cap body, a request
	// that stopped arriving, a malformed document, an ambiguous operation, an
	// unresolved fragment spread), and "attempted" for a recognized mutation the
	// server refused.
	ActionGraphQLWrite = "graphql_write"

	// Issue lifecycle + comments. The verb names the act; PROVIDER names the
	// system it happened in, which is why a GitHub issue and a Jira issue share
	// this vocabulary rather than each getting a parallel copy of it. Filtering
	// the log for "issues created" across both is the useful default, and a
	// caller that wants one system filters on provider as well.
	//
	// The transition/assignment pair is Jira-shaped (named statuses, a single
	// assignee) and only the Jira writers use it. The open/closed pair below
	// mirrors the pull-request family instead, because a GitHub issue has states
	// rather than a workflow.
	ActionIssueCreated       = "issue_created"
	ActionIssueTransitioned  = "issue_transitioned"
	ActionIssueAssigned      = "issue_assigned"
	ActionIssueUpdated       = "issue_updated"
	ActionIssueCommentPosted = "issue_comment_posted"
	ActionIssueClosed        = "issue_closed"
	ActionIssueReopened      = "issue_reopened"
	ActionIssueDeleted       = "issue_deleted"

	// The three that stay GitHub-shaped no matter which system the row's provider
	// names: Jira has no pinned issues, and its equivalent of a transfer is a
	// project move that reads as a field edit. They live here rather than in a
	// parallel GitHub-only block because the family is organized by act, and a
	// reader filtering for what happened to issues wants them in the same set.
	ActionIssuePinned      = "issue_pinned"
	ActionIssueUnpinned    = "issue_unpinned"
	ActionIssueTransferred = "issue_transferred"

	// GitHub review requests. Asking someone to review is an org-credential
	// write that reaches a human, so it belongs in the log by name; it is not a
	// review itself, which is why it sits apart from the review lifecycle above.
	ActionReviewRequested      = "review_requested"
	ActionReviewRequestRemoved = "review_request_removed"

	// Slack messages (TFAC-596). Reads (thread/channel history, file
	// download) are writes-only-by-charter exclusions — see external_actions'
	// table doc — so there is no read-side action here.
	ActionSlackMessagePosted = "slack_message_posted"
	ActionSlackMessageEdited = "slack_message_edited"
	ActionSlackReactionAdded = "slack_reaction_added"
)

// Org-credential identities an external action can be performed under. The
// ingestion gate keys on this: an org credential is recorded, an individual
// user's own credential is excluded.
const (
	CredentialGitHubApp = "github_app"
	CredentialJiraOrg   = "jira_org"
	CredentialSlackBot  = "slack_bot"
	// CredentialNone is the honest value for an action that spent no credential
	// at all: the sandbox egress denial, where nothing left the box and no
	// external system was ever reached. The column is NOT NULL, so a refused
	// connection needs a name rather than an empty string.
	CredentialNone = "none"
)

// BranchPushDedupKey builds the deterministic dedup key for a branch push:
// "branch:<runID>:<ref>:<sha>". The git pre-push hook and the git-proxy
// receive-pack backstop both observe the SAME push (same run, ref, sha), so they
// produce an identical key and the twin collapses under ON CONFLICT(org_id,
// dedup_key) DO NOTHING — while a genuinely new push (a force-push to the same
// ref, or new commits) carries a different sha and is recorded as its own row.
// This is one of the two actions with a deterministic natural key (the other is
// EgressDenialDedupKey); every other action leaves DedupKey empty (the store
// fills a uuid) so it can never be deduped away.
func BranchPushDedupKey(runID, ref, sha string) string {
	return strings.Join([]string{"branch", runID, ref, sha}, ":")
}

// EgressDenialDedupKey builds the deterministic dedup key for a refused sandbox
// egress connection: "egress:<conversationID>:<target>", where target is the
// CONNECT authority (host:port). An agent that has lost its way retries the same
// blocked host in a tight loop, and hundreds of identical rows would bury the
// signal rather than sharpen it — the thing worth recording is that THIS
// conversation was refused THIS destination, so the repeats collapse under
// ON CONFLICT(org_id, dedup_key) DO NOTHING.
//
// The conversation id is load-bearing, not decoration: the unique index spans
// (org_id, dedup_key), so a key built from the host alone would let one
// conversation's probe silently swallow every later conversation's probe of the
// same host — org-wide, for the lifetime of the table.
func EgressDenialDedupKey(conversationID, target string) string {
	return strings.Join([]string{"egress", conversationID, target}, ":")
}

// ExternalActionListOpts bounds + filters a list read for the action-log feeds.
// The zero value lists every action (newest first) with no filter or paging.
// Mirrors ArtifactListOpts.
type ExternalActionListOpts struct {
	// Limit caps rows returned (0 = no limit; the feed passes a page size).
	Limit int
	// Offset skips the first N rows for limit/offset paging. Only meaningful
	// alongside a positive Limit.
	Offset int
	// Provider / Action / ActorUserID are optional exact-match filters on the
	// matching column. Empty means no filter on that column.
	Provider    string
	Action      string
	ActorUserID string
	// Since / Until bound occurred_at to the half-open window [Since, Until).
	// Each side applies only when non-zero, so the zero value is unbounded.
	Since time.Time
	Until time.Time
}
