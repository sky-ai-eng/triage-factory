package domain

import (
	"net/url"
	"strings"
	"time"
)

// Artifact is one row in the artifacts table — the single durable,
// conversation-attributed, polymorphic record of something a conversation
// produced in an external system (a pushed branch, a draft/open PR, a draft/submitted
// review, a Jira/Linear issue, a comment). The (Provider, Kind) pair
// discriminates the shape. See TFAC-455.
//
// Artifacts are deduped on (OrgID, Scope, DedupKey) so the same logical object
// upserts to one row no matter which capture writer (exec choke point,
// pre-push hook, git-proxy backstop, reconciliation) saw it first.
// Build the key with ArtifactDedupKey and the scope with ExternalObjectScope.
//
// What a row stands for depends on the kind. A pull request, comment or
// message row is the object TF wrote, one row per object, owned by the
// conversation that wrote it. A review draft is one conversation's own, staged
// TF-side, so its key names that conversation. A branch and a Jira or Linear
// issue are objects many conversations act on, so their key ends in the acting
// conversation's id and each conversation that acts on one gets a row of its
// own: a run is credited with its own push or issue update, rather than the
// act landing on the row of whichever run touched the object first.
//
// TeamID is denormalized from the owning conversation so reads scope by team
// exactly like conversations. ConversationID is nullable (empty string here) so
// a row survives a conversation purge for audit — the FK is ON DELETE SET NULL.
type Artifact struct {
	ID string `json:"id"`
	// ConversationID is the conversation that produced this artifact — for a
	// per-conversation kind, the conversation its key names. Empty after the
	// conversation is purged (FK ON DELETE SET NULL) — the artifact outlives it
	// for the audit ledger.
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

	// Target is the display key of what the artifact is about: its entity's
	// key ('owner/repo#123', 'PROJ-123', a Linear identifier, a Slack thread
	// key), or the repository's 'owner/repo' for a branch, which has no
	// entity. ExternalID is the provider-native id of the backing object (PR
	// number / review id / Jira issue id / comment id / branch ref); empty
	// until the object exists. URL links to it; empty until created.
	Target     string `json:"target"`
	ExternalID string `json:"external_id,omitempty"`
	URL        string `json:"url,omitempty"`

	// EntityID is the entity the artifact is about: the pull request a pull
	// request, review or comment artifact is on, the Jira or Linear issue,
	// the Slack thread. A rename of the entity's key moves Target with it,
	// selected by this column, whatever the provider. Empty for a branch, and
	// for a row whose writer could not resolve its entity. Preserve-on-empty,
	// like ExternalID; the FK is ON DELETE SET NULL.
	EntityID string `json:"entity_id,omitempty"`

	// NodeID is the backing object's GraphQL node id, where the object has one
	// and a writer learned it. A GitHub comment carries it: a GraphQL delete
	// names a comment by node id alone, so a delete that arrives without the
	// comment's number finds the row through it
	// (ArtifactStore.GetByNodeIDSystem). A pull request carries it too: the
	// reconciler's backstop matches a pr_created ledger row to its artifact by
	// node id, which a repository rename does not change. Preserve-on-empty,
	// like ExternalID.
	NodeID string `json:"node_id,omitempty"`

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

	// pull_request: 'pending' is intent-only (not yet on GitHub); the rest
	// mirror GitHub's PR lifecycle.
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

	// comment: a row exists only for a comment TF posted, so a comment row is
	// how a reader tells TF's comments from people's. 'posted' once it lands;
	// 'deleted' when a run later removes it (the row persists, marked deleted —
	// retiring a comment never drops its artifact). An edit leaves the row as
	// it is, and an edit or a delete of a comment TF did not post leaves no
	// row: the audit ledger records both.
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
// "...:a:b". Today's resources (GitHub node ids, registry row ids, Linear
// UUIDs, JiraIssueResource, SlackMessageResource, refs/heads/x) carry no
// colons; a future provider passing something URL-like must encode it first.
//
// resource and anchor are the caller's choice of *stable* dedup
// coordinates — they need NOT equal the row's Artifact.Target /
// Artifact.ExternalID, which may evolve over the artifact's life. A resource
// is an id, never a name: a name changes with a rename, and a name GitHub or
// Jira has freed can be given to another object, whose artifacts would then
// land on the old one's rows.
//
//   - resource: the provider's stable id for the object the artifact is
//     about, or the registry row id where the object has none — the pull
//     request's GraphQL node id for a PR and for a review of it, the
//     repository's registry row id for a branch, JiraIssueResource (the site
//     and the issue id) for anything on a Jira issue, the issue's UUID for
//     anything on a Linear issue, and SlackMessageResource (the channel's
//     slack_channels row id and the message's ts) for a Slack message. The
//     key is unique only within the row's Scope, so two hosts', sites' or
//     workspaces' artifacts never share a row; a Jira resource carries the
//     site as well, which is the shape stored keys hold. A rename moves the
//     row's Target without touching its key.
//   - anchor: an optional stable sub-discriminator appended when resource
//     alone isn't unique — a comment's id on its issue, the conversation that
//     owns a review draft, or the conversation acting on a Jira or Linear
//     issue. A branch takes both its ref and its conversation
//     (BranchDedupKey). An issue acted on outside any run has no conversation
//     and no anchor.
//
// Examples:
//
//	ArtifactDedupKey("github", "pull_request", "<PR node id>", "")                         => "github:pull_request:<PR node id>"
//	ArtifactDedupKey("github", "review",       "<PR node id>", "<conversation id>")        => "github:review:<PR node id>:<conversation id>"
//	BranchDedupKey("<repository id>", "refs/heads/x", "<conversation id>")                  => "git:branch:<repository id>:refs/heads/x:<conversation id>"
//	ArtifactDedupKey("jira",   "issue",        JiraIssueResource(site, "10042"), "<conversation id>") => "jira:issue:https%3A%2F%2Facme.atlassian.net/10042:<conversation id>"
//	ArtifactDedupKey("jira",   "comment",      JiraIssueResource(site, "10042"), "20311") => "jira:comment:https%3A%2F%2Facme.atlassian.net/10042:20311"
//	ArtifactDedupKey("linear", "comment",      "<issue uuid>", "<comment id>")             => "linear:comment:<issue uuid>:<comment id>"
//	ArtifactDedupKey("slack",  "message",      SlackMessageResource("<channel row id>", ts), "") => "slack:message:<channel row id>/<ts>"
//
// Some stored rows carry a key that names an object by a name instead: a pull
// request, review or branch row that leads with owner/repo, whose repository
// no registry row identified, and a Jira row keyed on an issue key or a bare
// comment id, whose issue no entity named the id of. Nothing writes those
// shapes.
func ArtifactDedupKey(provider, kind, resource, anchor string) string {
	key := provider + ":" + kind + ":" + resource
	if anchor != "" {
		key += ":" + anchor
	}
	return key
}

// SlackMessageResource is the dedup-key resource of a Slack message artifact:
// the channel's slack_channels row id and the message's ts. A Slack message
// has no id of its own beyond its ts within its channel, and the channel's
// Slack id can change (Slack gives a private channel a new one when it is
// shared through Slack Connect) while its registry row does not, so the key
// names the row. Neither half carries a ':'. "" when either half is unknown.
func SlackMessageResource(channelRow, ts string) string {
	if channelRow == "" || ts == "" {
		return ""
	}
	return channelRow + "/" + ts
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
// other source. "" when there is no id.
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
// from its dedup key: the scope (when the key names one) and the provider id.
// A Jira or Linear artifact's key names its issue; a GitHub pull request's or
// review's names the pull request's node id. ok=false for an artifact that
// names none — a branch, a GitHub comment, a Slack artifact, or a stored row
// whose key still spells a name (a Jira issue key, a repository's owner/repo).
func ArtifactEntityIdentity(a Artifact) (scope, externalID string, ok bool) {
	if a.Provider == ArtifactProviderGitHub {
		nodeID, ok := ArtifactPullRequestNodeID(a)
		return "", nodeID, ok
	}
	p, rest, found := strings.Cut(a.DedupKey, ":")
	if !found || p != a.Provider {
		return "", "", false
	}
	_, rest, found = strings.Cut(rest, ":")
	if !found {
		return "", "", false
	}
	resource, _, _ := strings.Cut(rest, ":")
	switch a.Provider {
	case ArtifactProviderJira:
		return ParseJiraIssueResource(resource)
	case ArtifactProviderLinear:
		return "", resource, resource != ""
	}
	return "", "", false
}

// ArtifactPullRequestNodeID returns the GraphQL node id of the pull request a
// GitHub pull_request or review artifact is keyed on (PullRequestDedupKey,
// ReviewDedupKey). ok=false for any other artifact, and for a stored row whose
// key still leads with owner/repo#N: a node id is base64, which never holds a
// '#'.
func ArtifactPullRequestNodeID(a Artifact) (string, bool) {
	if a.Provider != ArtifactProviderGitHub || (a.Kind != ArtifactKindPullRequest && a.Kind != ArtifactKindReview) {
		return "", false
	}
	rest, found := strings.CutPrefix(a.DedupKey, ArtifactProviderGitHub+":"+a.Kind+":")
	if !found {
		return "", false
	}
	nodeID, _, _ := strings.Cut(rest, ":")
	if nodeID == "" || strings.Contains(nodeID, "#") {
		return "", false
	}
	return nodeID, true
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
