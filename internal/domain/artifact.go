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
	// RunID is the run that produced this artifact. Empty after the run
	// is purged (FK ON DELETE SET NULL) — the artifact outlives it for
	// the audit ledger.
	RunID  string `json:"run_id,omitempty"`
	OrgID  string `json:"org_id"`
	TeamID string `json:"team_id"`

	// Provider + Kind are the polymorphic discriminators. Use the
	// ArtifactProvider* / ArtifactKind* consts.
	Provider string `json:"provider"`
	Kind     string `json:"kind"`

	// Target is the resource key: 'owner/repo', 'owner/repo#123',
	// 'SKY-123'. ExternalID is the provider-native id of the backing
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
)

// Artifact kind discriminators.
const (
	ArtifactKindBranch      = "branch"
	ArtifactKindPullRequest = "pull_request"
	ArtifactKindReview      = "review"
	ArtifactKindIssue       = "issue"
	ArtifactKindComment     = "comment"
)

// Artifact state lifecycle values, grouped by kind. App-validated only —
// there is no DB CHECK, so the set is extensible per the locked design.
const (
	// branch
	ArtifactStateBranchPushed = "pushed"

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

	// issue (Jira / future Linear)
	ArtifactStateIssueCreated = "created"
	ArtifactStateIssueUpdated = "updated"

	// comment
	ArtifactStateCommentPosted = "posted"
)

// ArtifactDedupKey builds the stable, provider-natural key Upsert
// conflicts on: provider:kind:target[:externalID]. The same logical
// artifact maps to the same key regardless of which writer observed it,
// so a PR seen via exec and again via reconciliation is one row.
//
// Examples:
//
//	ArtifactDedupKey("github", "pull_request", "owner/repo#123", "")            => "github:pull_request:owner/repo#123"
//	ArtifactDedupKey("git",    "branch",       "owner/repo", "refs/heads/x")    => "git:branch:owner/repo:refs/heads/x"
//	ArtifactDedupKey("jira",   "issue",        "SKY-123", "")                   => "jira:issue:SKY-123"
//
// Pending→real PR: a 'pending' PR has no number yet, so writers key it on
// the branch ref it will open from (e.g. ArtifactDedupKey("github",
// "pull_request", "owner/repo", "refs/heads/x")). When the real PR is
// created the writer keys on the same branch ref, so the row upserts in
// place — state flips pending→open and external_id/url fill in. The key
// must stay stable across that transition; keying the real PR on the
// number instead would mint a second row.
func ArtifactDedupKey(provider, kind, target, externalID string) string {
	key := provider + ":" + kind + ":" + target
	if externalID != "" {
		key += ":" + externalID
	}
	return key
}
