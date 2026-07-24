package domain

import "time"

// Artifact is one row in the artifacts table — the single durable,
// run-attributed, polymorphic record of something a run produced in an
// external system (a pushed branch, a draft/open PR, a draft/submitted
// review, a Jira/Linear issue, a comment). One row per external object;
// the (Provider, Kind) pair discriminates the shape. See TFAC-455.
//
// Artifacts are deduped on (OrgID, DedupKey) so the same logical object
// upserts to one row no matter which capture writer (exec choke point,
// pre-push hook, git-proxy backstop, reconciliation) saw it first.
// Build the key with ArtifactDedupKey.
//
// TeamID is denormalized from the owning run so reads scope by team
// exactly like runs. RunID is nullable (empty string here) so a row
// survives a run purge for audit — the FK is ON DELETE SET NULL.
type Artifact struct {
	ID string `json:"id"`
	// ConversationID is the run that produced this artifact. Empty after the run
	// is purged (FK ON DELETE SET NULL) — the artifact outlives it for
	// the audit ledger.
	ConversationID string `json:"conversation_id,omitempty"`
	OrgID          string `json:"org_id"`
	TeamID         string `json:"team_id"`

	// Provider + Kind are the polymorphic discriminators. Use the
	// ArtifactProvider* / ArtifactKind* consts.
	Provider string `json:"provider"`
	Kind     string `json:"kind"`

	// Target is the resource key: 'owner/repo', 'owner/repo#123',
	// or a Jira-style issue key (e.g. 'PROJ-123'). ExternalID is the provider-native id of the backing
	// object (PR number / review id / issue key / branch ref); empty
	// until the object exists. URL links to it; empty until created.
	Target     string `json:"target"`
	ExternalID string `json:"external_id,omitempty"`
	URL        string `json:"url,omitempty"`

	// State is the per-Kind lifecycle position. Use the ArtifactState*
	// consts; not DB-constrained so the set stays extensible.
	State string `json:"state"`

	// DedupKey is the stable natural key Upsert conflicts on. Built by
	// ArtifactDedupKey so every writer that sees the same logical
	// object lands on the same row.
	DedupKey string `json:"dedup_key"`

	// DetailsJSON is optional kind-specific payload. Empty string
	// serializes to SQL NULL.
	DetailsJSON string `json:"details_json,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Artifact provider discriminators.
const (
	ArtifactProviderGitHub = "github"
	ArtifactProviderJira   = "jira"
	ArtifactProviderLinear = "linear"
	ArtifactProviderGit    = "git"
	ArtifactProviderSlack  = "slack"
)

// Artifact kind discriminators.
const (
	ArtifactKindBranch      = "branch"
	ArtifactKindPullRequest = "pull_request"
	ArtifactKindReview      = "review"
	ArtifactKindIssue       = "issue"
	ArtifactKindComment     = "comment"
	ArtifactKindMessage     = "message"
)

// Artifact state lifecycle values, grouped by kind. App-validated only —
// there is no DB CHECK, so the set is extensible per the locked design.
//
// Some values intentionally alias across kinds (ArtifactStatePRPending and
// ArtifactStateReviewPending are both "pending"), so state is only
// meaningful read together with Kind — always branch on Artifact.Kind
// first, never on the raw state string alone. The per-kind named consts
// exist to nudge callers to the right one for their kind; the equal string
// values mean a cross-kind mix-up won't surface as a type or string error.
const (
	// branch: 'pushed' once the ref lands on the remote; 'deleted' is the
	// terminal state the reconciler stamps when the ref is gone from GitHub
	// (TFAC-464). The row persists as the historical record — it just drops
	// out of the non-terminal set, i.e. becomes "untracked".
	ArtifactStateBranchPushed  = "pushed"
	ArtifactStateBranchDeleted = "deleted"

	// pull_request: 'pending' is intent-only (not yet on GitHub — the
	// branch-anchored PR before approval); the rest mirror GitHub's PR
	// lifecycle.
	ArtifactStatePRPending = "pending"
	ArtifactStatePRDraft   = "draft"
	ArtifactStatePROpen    = "open"
	ArtifactStatePRMerged  = "merged"
	ArtifactStatePRClosed  = "closed"

	// review: 'pending' is a GitHub pending review (private to the bot);
	// 'submitted' once it lands on the PR; 'dismissed' if discarded.
	ArtifactStateReviewPending   = "pending"
	ArtifactStateReviewSubmitted = "submitted"
	ArtifactStateReviewDismissed = "dismissed"

	// issue (Jira / future Linear). These track the *last* action on the
	// row, not the first: because the artifact is one deduped row, a
	// 'created' issue that a later upsert touches again flips to 'updated',
	// so "did this run create or only update the issue?" is NOT recoverable
	// from state alone — a writer that needs that distinction must record it
	// in details_json at create time.
	ArtifactStateIssueCreated = "created"
	ArtifactStateIssueUpdated = "updated"

	// comment: 'posted' once it lands on GitHub/Jira; 'deleted' when the agent
	// later removes it (the row persists for the audit ledger, marked deleted —
	// retiring a comment never drops its artifact).
	ArtifactStateCommentPosted  = "posted"
	ArtifactStateCommentDeleted = "deleted"

	// message (Slack): 'posted' once chat.postMessage lands, and stays
	// 'posted' through an in-place chat.update edit — Slack has no delete verb
	// in this ticket's scope, so there is no terminal state to add yet.
	ArtifactStateMessagePosted = "posted"
)

// ArtifactDedupKey builds the stable, provider-natural key Upsert
// conflicts on: provider:kind:resource[:anchor]. The same logical artifact
// maps to the same key regardless of which writer observed it, so a PR
// seen via exec and again via reconciliation is one row.
//
// The key is ':'-delimited, so no segment may itself contain a ':' —
// ("a:b", "") and ("a", "b") would otherwise collapse to the same
// "...:a:b". Today's providers and GitHub/Jira identifiers (owner/repo,
// refs/heads/x, a Jira issue key) carry no colons, so no current writer is at risk;
// a future provider passing something URL-like must encode it first.
//
// resource and anchor are the caller's choice of *stable* dedup
// coordinates — they need NOT equal the row's Artifact.Target /
// Artifact.ExternalID, which may evolve over the artifact's life. Pick
// values that don't change across the transitions the artifact undergoes:
//
//   - resource: the stable resource key — 'owner/repo' for a branch or a
//     branch-anchored PR, 'owner/repo#123' for a PR keyed on its number,
//     the Jira issue key for a Jira issue.
//   - anchor: an optional stable sub-discriminator appended when resource
//     alone isn't unique — a branch ref for a branch, or for a PR whose
//     number isn't known yet (see below). Empty when resource is already
//     unique (e.g. jira:issue:PROJ-123).
//
// Examples:
//
//	ArtifactDedupKey("github", "pull_request", "owner/repo#123", "")          => "github:pull_request:owner/repo#123"
//	ArtifactDedupKey("git",    "branch",       "owner/repo", "refs/heads/x")  => "git:branch:owner/repo:refs/heads/x"
//	ArtifactDedupKey("jira",   "issue",        "PROJ-123", "")                 => "jira:issue:PROJ-123"
//
// Pending→real PR — why resource/anchor are NOT the struct fields: a
// 'pending' PR has no number yet, so the writer keys it on the branch ref
// it will open from: ArtifactDedupKey("github", "pull_request",
// "owner/repo", "refs/heads/x"). When the real PR is created the writer
// keys on the same repo+ref, so the row upserts in place — Artifact.Target
// migrates owner/repo → owner/repo#123 and Artifact.ExternalID fills in,
// but the dedup key stays put. Keying the real PR on its number instead
// would mint a second row.
func ArtifactDedupKey(provider, kind, resource, anchor string) string {
	key := provider + ":" + kind + ":" + resource
	if anchor != "" {
		key += ":" + anchor
	}
	return key
}

// reconcilableNonTerminal lists, per Kind, the states the reconciler
// (TFAC-464) treats as non-terminal AND backed by a fetchable GitHub object —
// the exact set Artifacts.ListNonTerminalBySystem returns and the Tier-2
// run-scoped refresh filters to. It is the single source of truth both the
// store's SQL predicate and IsReconcilableNonTerminal derive from.
//
// PR 'pending' is deliberately excluded: a pending PR is intent-only (no
// number, no node id), so there is no backing object to refresh — it can't be
// reconciled until a create writer turns it real. Terminal states (PR
// merged/closed, review submitted/dismissed, branch deleted, every comment /
// issue state) are absent, so a terminal artifact is never re-queried.
//
// Review is absent entirely (TFAC-494): a review is staged TF-side until the
// atomic create+submit at approval, so a pending review draft has no GitHub
// object to reconcile — a never-published draft can't drift out-of-band, and the
// instant it submits it is already terminal.
var reconcilableNonTerminal = map[string]map[string]bool{
	ArtifactKindPullRequest: {ArtifactStatePRDraft: true, ArtifactStatePROpen: true},
	ArtifactKindBranch:      {ArtifactStateBranchPushed: true},
}

// IsReconcilableNonTerminal reports whether an artifact of (kind, state) is
// in the reconciler's working set: non-terminal AND backed by a fetchable
// GitHub object. The Tier-2 run-scoped refresh filters a run's artifacts
// through this so it reconciles exactly what the org-wide Tier-1 query would;
// TestReconcilableNonTerminal pins it equal to the store's SQL predicate.
func IsReconcilableNonTerminal(kind, state string) bool {
	return reconcilableNonTerminal[kind][state]
}
