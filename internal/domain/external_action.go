package domain

import (
	"strings"
	"time"
)

// ExternalAction is one row in external_actions — the append-only audit log of
// record. Each row captures one external write Triage Factory performed under an
// ORG-scoped credential (the org GitHub App or the PAT standing in for it, the
// org Jira service account): who, what, when, under which credential, and
// from→to for a transition.
//
// This is event-grain and immutable, distinct from the mutable, object-grain
// Artifact (which records an object's current state and upserts in place). The
// gate for what lands here is the CREDENTIAL identity used at write time — an
// org-scoped credential is in, an individual user's own credential is out (the
// Jira claim/swipe/done/requeue flows, already attributed natively in Jira).
//
// One amendment to "immutable", and exactly one: the record of the act —
// Target, Action, DetailJSON, Credential, OccurredAt, and the url column as
// captured — is frozen, while the POINTER to where the object now lives is
// maintained. A repository rename fills the row's current_url column (in the
// same transaction as the rest of its rewrite), and reads serve
// COALESCE(current_url, url) through the URL field below, so the feed's link
// keeps resolving after the freed name is re-claimed by a different
// repository. current_url is the only column an UPDATE may touch; rewriting
// Target on the same reasoning would make the log assert TF acted on a name
// that did not exist at the time.
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
	// URL links to the object. On a read this is the maintained pointer —
	// COALESCE(current_url, url), so it resolves under the object's current
	// name — while the url column itself keeps the link as captured at the
	// time of the act. Empty → SQL NULL.
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
	// GitHub write until the atomic submit at approval, so there is no
	// review_started — start-review writes nothing.
	//
	// Dismissal is two different acts sharing a word, and only one of them is
	// here. Abandoning a TF-staged draft is a local state flip on an artifact
	// that never reached GitHub, and records no external action. Dismissing a
	// SUBMITTED review clears its approval or change-request standing on GitHub
	// under the org credential — an ordinary write, and one an agent can aim at
	// a review a person left.
	//
	// The comment edit/delete actions below are unrelated: they audit edits to
	// an already-submitted review's comments.
	ActionReviewSubmitted      = "review_submitted"
	ActionReviewDismissed      = "review_dismissed"
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
	// through the issue endpoint. Changing which labels the repository OFFERS is
	// a different act on a different object, and has its own verbs below.
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

	// A write through the real-`gh` credential channel that Triage Factory
	// refused before forwarding it — a submitted review, a repository creation,
	// or a GraphQL request whose act could not be established at all. Nothing
	// reached GitHub, which is what
	// separates this row from the fallbacks below: those say a write happened
	// and could not be named, this says a write was stopped.
	//
	// detail_json carries {reason, method, path}, plus "refused" naming the act
	// where the classifier resolved one, "mutation" for the GraphQL field, and
	// "unreadable" for the band where naming it is exactly what failed. The
	// distinction is the point: "we refused a review" and "we refused something
	// we could not read" are different operational facts, and only the second
	// one should ever be worth investigating.
	//
	// Appended unconditionally (no dedup key): every refused attempt is its own
	// event, exactly like ActionGitDenied, which this follows in every respect
	// — a per-run least-privilege gate turning down an act, recorded as a
	// security signal.
	ActionGHWriteDenied = "gh_write_denied"

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

	// Repository configuration: acts that change the repository itself rather
	// than anything being triaged inside it. They are not triage work and an org
	// may well decide an agent should never perform them — which is the reason
	// they are named rather than left anonymous. That decision gets made by
	// reading this log, and a log that files an archived repository under "some
	// GraphQL write happened" cannot inform it.
	//
	// repo_edited spans every settings change the porcelain makes, including the
	// topic replacement it sends to a separate endpoint: what varies between
	// them lives in a request body nothing on this path reads, so one verb is
	// all the URL supports and a finer one would be invented rather than
	// observed.
	ActionRepoCreated    = "repo_created"
	ActionRepoEdited     = "repo_edited"
	ActionRepoDeleted    = "repo_deleted"
	ActionRepoForked     = "repo_forked"
	ActionRepoArchived   = "repo_archived"
	ActionRepoUnarchived = "repo_unarchived"

	// The repository's label SET — which labels exist at all, not which ones are
	// on a given issue. Deliberately worded apart from label_added/label_removed
	// above: deleting a label definition strips it from every issue carrying it,
	// so the two must never read as variants of one another in the log.
	ActionLabelDefined           = "label_defined"
	ActionLabelDefinitionEdited  = "label_definition_edited"
	ActionLabelDefinitionDeleted = "label_definition_deleted"

	// Releases. A published release is the most outward-facing thing on this
	// list — it is what consumers of the repository actually fetch — so it is
	// named even though the run that makes one is doing something well outside
	// triage.
	//
	// Uploading and deleting a release's ASSETS have no verbs, because they are
	// unobservable here rather than unnamed: gh follows the absolute upload url
	// out of the release payload, which points at GitHub directly and never at
	// this channel's base. Naming an act nothing can see would promise coverage
	// that does not exist.
	ActionReleaseCreated = "release_created"
	ActionReleaseEdited  = "release_edited"
	ActionReleaseDeleted = "release_deleted"

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
	// CredentialGitHubPAT is the tier-3 PAT an org lends Triage Factory when it
	// has no GitHub App. It sits on the ORG side of the ingestion gate beside
	// the App, and the distinction the gate draws is about WHOSE ACT a row
	// records, not whose token it is: this credential acts for the org on every
	// run in it, exactly as the App does, and no user chose it per action. The
	// excluded case is the opposite one — a user acting as themselves through
	// their own authorization (the Jira claim/swipe/done/requeue flows), which
	// is already attributed natively in the system it lands in.
	//
	// Which of the two acted is not cosmetic. A PAT is unscopeable, so the blast
	// radius of a row written under it is every repository its holder can reach,
	// and it carries a person's name into whatever it touches. A log that files
	// those under github_app answers "who did this" with a service account that
	// was never involved.
	CredentialGitHubPAT = "github_pat"
	CredentialJiraOrg   = "jira_org"
	CredentialSlackBot  = "slack_bot"
	// CredentialNone is the honest value for an action that spent no credential
	// at all: the sandbox egress denial, where nothing left the box and no
	// external system was ever reached. The column is NOT NULL, so a refused
	// connection needs a name rather than an empty string.
	CredentialNone = "none"
)

// ExternalActionTypes is the closed action vocabulary — every Action* constant
// above, in declaration order. Read APIs validate a caller's ?action= filter
// against it so a typo is a rejected request rather than an authoritative-
// looking empty page. Hand-maintained like AllEventTypes: a new Action*
// constant belongs here too, or the feed can't be filtered by it.
func ExternalActionTypes() []string {
	return []string{
		ActionPRCreated, ActionPRMarkedReady, ActionPRConvertedToDraft, ActionPREdited,
		ActionPRClosed, ActionPRReopened, ActionPRMerged, ActionPRAutoMergeEnabled,
		ActionPRAutoMergeDisabled, ActionPRReverted, ActionPRBranchUpdated,
		ActionReviewSubmitted, ActionReviewDismissed, ActionReviewCommentEdited,
		ActionReviewCommentDeleted, ActionCommentPosted, ActionCommentEdited,
		ActionCommentDeleted, ActionReactionAdded, ActionReactionRemoved, ActionLabelAdded,
		ActionLabelRemoved, ActionConversationLocked, ActionConversationUnlocked,
		ActionWorkflowDispatched, ActionWorkflowRunCancelled, ActionBranchPushed,
		ActionLinkedBranchCreated, ActionBranchPushFailed, ActionGitDenied,
		ActionEgressDenied, ActionGHWriteDenied, ActionGHChannelWrite, ActionGraphQLWrite,
		ActionIssueCreated, ActionIssueTransitioned, ActionIssueAssigned, ActionIssueUpdated,
		ActionIssueCommentPosted, ActionIssueClosed, ActionIssueReopened, ActionIssueDeleted,
		ActionIssuePinned, ActionIssueUnpinned, ActionIssueTransferred,
		ActionReviewRequested, ActionReviewRequestRemoved, ActionRepoCreated,
		ActionRepoEdited, ActionRepoDeleted, ActionRepoForked, ActionRepoArchived,
		ActionRepoUnarchived, ActionLabelDefined, ActionLabelDefinitionEdited,
		ActionLabelDefinitionDeleted, ActionReleaseCreated, ActionReleaseEdited,
		ActionReleaseDeleted, ActionSlackMessagePosted, ActionSlackMessageEdited,
		ActionSlackReactionAdded,
	}
}

// BranchPushDedupKey builds the deterministic dedup key for a branch push:
// "branch:<conversationID>:<ref>:<sha>". The git pre-push hook and the git-proxy
// receive-pack backstop both observe the SAME push (same run, ref, sha), so they
// produce an identical key and the twin collapses under ON CONFLICT(org_id,
// dedup_key) DO NOTHING — while a genuinely new push (a force-push to the same
// ref, or new commits) carries a different sha and is recorded as its own row.
// This is one of the two actions with a deterministic natural key (the other is
// EgressDenialDedupKey); every other action leaves DedupKey empty (the store
// fills a uuid) so it can never be deduped away.
func BranchPushDedupKey(conversationID, ref, sha string) string {
	return strings.Join([]string{"branch", conversationID, ref, sha}, ":")
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
