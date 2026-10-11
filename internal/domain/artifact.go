package domain

import (
	"net/url"
	"strings"
	"time"
)

// Artifact is one row in the artifacts table — the single durable,
// conversation-attributed, polymorphic record of something a conversation
// produced in an external system (a pushed branch, a draft/open PR, a draft/submitted
// review, a Jira/Linear issue, a comment). One row per external object;
// the (Provider, Kind) pair discriminates the shape. See TFAC-455.
//
// Artifacts are deduped on (OrgID, Scope, DedupKey) so the same logical object
// upserts to one row no matter which capture writer (exec choke point,
// pre-push hook, git-proxy backstop, reconciliation) saw it first.
// Build the key with ArtifactDedupKey and the scope with ExternalObjectScope.
//
// TeamID is denormalized from the owning conversation so reads scope by team
// exactly like conversations. ConversationID is nullable (empty string here) so
// a row survives a conversation purge for audit — the FK is ON DELETE SET NULL.
type Artifact struct {
	ID string `json:"id"`
	// ConversationID is the conversation that produced this artifact. Empty
	// after the conversation is purged (FK ON DELETE SET NULL) — the artifact
	// outlives it for the audit ledger.
	ConversationID string `json:"conversation_id,omitempty"`
	OrgID          string `json:"org_id"`
	TeamID         string `json:"team_id"`

	// Provider + Kind are the polymorphic discriminators. Use the
	// ArtifactProvider* / ArtifactKind* consts.
	Provider string `json:"provider"`
	Kind     string `json:"kind"`

	// Scope is the provider namespace the object lives in, the value an
	// entity of the same provider is scoped under: the GitHub host for a
	// github or git artifact, the Jira site, the Linear workspace id,
	// SlackScope. Target and DedupKey are only unique inside it. Required on
	// every write.
	Scope string `json:"scope"`

	// Target is the resource key: 'owner/repo', 'owner/repo#123',
	// or a Jira-style issue key (e.g. 'PROJ-123'). ExternalID is the provider-native id of the backing
	// object (PR number / review id / Jira issue id / comment id / branch
	// ref); empty until the object exists. URL links to it; empty until
	// created.
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
	// ArtifactProviderNetwork is the provider for an action whose target is a
	// bare network destination rather than an object in a named system — the
	// sandbox egress proxy's refused CONNECT, whose host is arbitrary
	// (pypi.org, api.github.com). No artifact is ever written under it; it
	// exists so the external-action row has a provider that doesn't misfile a
	// blocked host as a GitHub or Jira write.
	ArtifactProviderNetwork = "network"
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
	// so "did this conversation create or only update the issue?" is NOT recoverable
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

// ArtifactKinds is the closed kind vocabulary, and ArtifactStates the union of
// every per-kind state above. Read APIs validate a caller's ?kind= / ?state=
// filter against them so a typo is a rejected request rather than an
// authoritative-looking empty page.
func ArtifactKinds() []string {
	return []string{
		ArtifactKindBranch, ArtifactKindPullRequest, ArtifactKindReview,
		ArtifactKindIssue, ArtifactKindComment, ArtifactKindMessage,
	}
}

// artifactStatesByKind is every kind's full lifecycle, one entry per kind in
// ArtifactKinds. ArtifactStates is derived from it rather than hand-listed, so
// a state added to a kind above joins the read filters' vocabulary with it — a
// separately maintained union drifts silently, and the failure mode is a valid
// request rejected as a typo.
var artifactStatesByKind = map[string][]string{
	ArtifactKindBranch: {ArtifactStateBranchPushed, ArtifactStateBranchDeleted},
	ArtifactKindPullRequest: {
		ArtifactStatePRPending, ArtifactStatePRDraft, ArtifactStatePROpen,
		ArtifactStatePRMerged, ArtifactStatePRClosed,
	},
	ArtifactKindReview: {
		ArtifactStateReviewPending, ArtifactStateReviewSubmitted, ArtifactStateReviewDismissed,
	},
	ArtifactKindIssue:   {ArtifactStateIssueCreated, ArtifactStateIssueUpdated},
	ArtifactKindComment: {ArtifactStateCommentPosted, ArtifactStateCommentDeleted},
	ArtifactKindMessage: {ArtifactStateMessagePosted},
}

// ArtifactStates is the deduplicated union of the per-kind lifecycles. A state
// is only meaningful read with its kind (values alias across kinds), so this is
// a filter vocabulary, not a per-kind contract. Iteration is over ArtifactKinds
// rather than the map, so the order is stable for the "want one of …" message.
func ArtifactStates() []string {
	out := make([]string, 0, len(artifactStatesByKind))
	seen := map[string]bool{}
	for _, kind := range ArtifactKinds() {
		for _, state := range artifactStatesByKind[kind] {
			if seen[state] {
				continue
			}
			seen[state] = true
			out = append(out, state)
		}
	}
	return out
}

// ArtifactProviders is the closed provider vocabulary, for the same use.
func ArtifactProviders() []string {
	return []string{
		ArtifactProviderGitHub, ArtifactProviderJira, ArtifactProviderLinear,
		ArtifactProviderGit, ArtifactProviderSlack, ArtifactProviderNetwork,
	}
}

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
//     JiraIssueResource (the site and the issue id) for anything on a Jira
//     issue, and the issue's UUID for anything on a Linear issue. Never a
//     Jira key or a Linear identifier: a move or a key rename changes it, and
//     another site or workspace in the same org can have an issue under the
//     same one. The key is unique only within the row's Scope, so two
//     sites' or workspaces' artifacts never share a row; a Jira resource
//     carries the site as well, which is the shape stored keys hold. An
//     entity rename moves the row's Target without touching its key
//     (EntityArtifactResource).
//   - anchor: an optional stable sub-discriminator appended when resource
//     alone isn't unique — a branch ref for a branch, a PR whose number
//     isn't known yet (see below), or a comment's id on its issue. Empty when
//     resource is already unique (an issue itself).
//
// Examples:
//
//	ArtifactDedupKey("github", "pull_request", "owner/repo#123", "")          => "github:pull_request:owner/repo#123"
//	ArtifactDedupKey("git",    "branch",       "owner/repo", "refs/heads/x")  => "git:branch:owner/repo:refs/heads/x"
//	ArtifactDedupKey("jira",   "issue",        JiraIssueResource(site, "10042"), "")         => "jira:issue:https%3A%2F%2Facme.atlassian.net/10042"
//	ArtifactDedupKey("jira",   "comment",      JiraIssueResource(site, "10042"), "20311")    => "jira:comment:https%3A%2F%2Facme.atlassian.net/10042:20311"
//	ArtifactDedupKey("linear", "comment",      "<issue uuid>", "<comment id>") => "linear:comment:<issue uuid>:<comment id>"
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

// JiraIssueResource is the dedup-key resource every artifact about a Jira
// issue is keyed under: the issue's site and its numeric id. Both halves are
// needed. The key a person reads changes when the issue moves or its project's
// key is renamed, and the id repeats across sites, so an org with two Jira
// sites has two issue 10042s. The site is query-escaped, which leaves no ':'
// in it (ArtifactDedupKey's separator) and no '/' (this one's), so the pair
// splits back apart unambiguously. "" when either half is unknown: there is no
// stable resource to key on.
func JiraIssueResource(site, issueID string) string {
	if site == "" || issueID == "" {
		return ""
	}
	return url.QueryEscape(site) + "/" + issueID
}

// ParseJiraIssueResource splits a JiraIssueResource back into its site and
// issue id. ok=false for anything else — an artifact keyed on an issue key,
// which carries no id.
func ParseJiraIssueResource(resource string) (site, issueID string, ok bool) {
	escaped, id, found := strings.Cut(resource, "/")
	if !found || escaped == "" || id == "" {
		return "", "", false
	}
	site, err := url.QueryUnescape(escaped)
	if err != nil || site == "" {
		return "", "", false
	}
	return site, id, true
}

// EntityArtifactResource is the dedup-key resource segment artifacts about the
// entity carrying (source, scope, externalID) are keyed under: the issue's
// site and id for Jira (JiraIssueResource), the provider id itself for every
// other source. "" when there is no id. It is what an entity rename matches
// artifacts by, inside the entity's scope, so it never names a display key.
func EntityArtifactResource(source, scope, externalID string) string {
	if externalID == "" {
		return ""
	}
	if source == ArtifactProviderJira {
		return JiraIssueResource(scope, externalID)
	}
	return externalID
}

// ArtifactEntityIdentity reads the identity of the entity an artifact is about
// off its dedup key: the scope (when the key names one) and the provider id.
// ok=false for a key that names none — a provider whose artifacts are keyed on
// display keys (GitHub, Slack), or a Jira artifact keyed on an issue key.
func ArtifactEntityIdentity(provider, dedupKey string) (scope, externalID string, ok bool) {
	p, rest, found := strings.Cut(dedupKey, ":")
	if !found || p != provider {
		return "", "", false
	}
	_, rest, found = strings.Cut(rest, ":")
	if !found {
		return "", "", false
	}
	resource, _, _ := strings.Cut(rest, ":")
	switch provider {
	case ArtifactProviderJira:
		return ParseJiraIssueResource(resource)
	case ArtifactProviderLinear:
		return "", resource, resource != ""
	}
	return "", "", false
}

// ArtifactKeyHasResource reports whether key, as ArtifactDedupKey builds it,
// is under provider and has resource as its resource segment, whole: the
// resource "uuid-4" never matches "uuid-41".
func ArtifactKeyHasResource(key, provider, resource string) bool {
	p, rest, ok := strings.Cut(key, ":")
	if !ok || p != provider || resource == "" {
		return false
	}
	_, rest, ok = strings.Cut(rest, ":")
	if !ok {
		return false
	}
	got, _, _ := strings.Cut(rest, ":")
	return got == resource
}

// reconcilableNonTerminal lists, per Kind, the states the reconciler
// (TFAC-464) treats as non-terminal AND backed by a fetchable GitHub object —
// the exact set Artifacts.ListNonTerminalBySystem returns and the Tier-2
// conversation-scoped refresh filters to. It is the single source of truth both the
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
// GitHub object. The Tier-2 conversation-scoped refresh filters a
// conversation's artifacts through this so it reconciles exactly what the org-wide Tier-1 query would;
// the ListNonTerminal store tests pin it equal to the store's SQL predicate.
func IsReconcilableNonTerminal(kind, state string) bool {
	return reconcilableNonTerminal[kind][state]
}
