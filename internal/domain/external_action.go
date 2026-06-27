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
	// Target is the resource key: owner/repo#123, owner/repo, SKY-123.
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
	// RunID is the producing run — the agent's for a bot/system action, the
	// drafter's for an approval. Empty (→ SQL NULL) for an action with no run (a
	// dashboard board-drag) or after the run is purged (FK ON DELETE SET NULL).
	RunID string `json:"run_id,omitempty"`
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

	// GitHub review lifecycle.
	ActionReviewStarted        = "review_started"
	ActionReviewSubmitted      = "review_submitted"
	ActionReviewDismissed      = "review_dismissed"
	ActionReviewCommentEdited  = "review_comment_edited"
	ActionReviewCommentDeleted = "review_comment_deleted"

	// GitHub standalone comments.
	ActionCommentPosted  = "comment_posted"
	ActionCommentEdited  = "comment_edited"
	ActionCommentDeleted = "comment_deleted"

	// Git branch push (the one double-capture case — see BranchPushDedupKey).
	ActionBranchPushed = "branch_pushed"

	// Git operation denied by the per-run least-privilege gate — the git proxy
	// (off-repo / off-ref / non-git path) or the exec-gh channel (off-repo).
	// A security signal, recorded even for a denied read. detail_json carries
	// {op, ref, reason}.
	ActionGitDenied = "git_denied"

	// Jira issue lifecycle + comments.
	ActionIssueCreated       = "issue_created"
	ActionIssueTransitioned  = "issue_transitioned"
	ActionIssueAssigned      = "issue_assigned"
	ActionIssueUpdated       = "issue_updated"
	ActionIssueCommentPosted = "issue_comment_posted"
)

// Org-credential identities an external action can be performed under. The
// ingestion gate keys on this: an org credential is recorded, an individual
// user's own credential is excluded.
const (
	CredentialGitHubApp = "github_app"
	CredentialJiraOrg   = "jira_org"
)

// BranchPushDedupKey builds the deterministic dedup key for a branch push:
// "branch:<runID>:<ref>:<sha>". The git pre-push hook and the git-proxy
// receive-pack backstop both observe the SAME push (same run, ref, sha), so they
// produce an identical key and the twin collapses under ON CONFLICT(org_id,
// dedup_key) DO NOTHING — while a genuinely new push (a force-push to the same
// ref, or new commits) carries a different sha and is recorded as its own row.
// This is the ONLY action with a deterministic natural key; every other action
// leaves DedupKey empty (the store fills a uuid) so it can never be deduped away.
func BranchPushDedupKey(runID, ref, sha string) string {
	return strings.Join([]string{"branch", runID, ref, sha}, ":")
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
