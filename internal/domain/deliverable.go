package domain

import (
	"net/url"
	"strings"
	"time"
)

// Deliverable is one row in the deliverables table: work an agent prepared
// that a person acts on, owned by the conversation that made it. There are two
// kinds. A pull request moves draft → open | closed | merged; a review moves
// pending → submitted | dismissed, with its staged comments and body in
// DetailsJSON. Every other object a run changes is an artifact, read from the
// external-action ledger (Artifact), never a deliverable.
//
// Deliverables are deduped on (OrgID, Scope, DedupKey) so the same pull request
// upserts to one row whichever writer (the exec choke point, the reconciler's
// backstop) saw it first. Build the key with ObjectKey and the scope with
// ExternalObjectScope.
//
// TeamID is denormalized from the owning conversation so reads scope by team
// exactly like conversations. ConversationID is nullable (empty string here) so
// a row survives a conversation purge for audit — the FK is ON DELETE SET NULL.
type Deliverable struct {
	ID string `json:"id"`
	// ConversationID is the conversation that produced this deliverable. Empty
	// after the conversation is purged (FK ON DELETE SET NULL) — the deliverable
	// outlives it for the audit ledger.
	ConversationID string `json:"conversation_id,omitempty"`
	OrgID          string `json:"org_id"`
	TeamID         string `json:"team_id"`

	// Provider + Kind are the polymorphic discriminators. Use the
	// Provider* / DeliverableKind* consts.
	Provider string `json:"provider"`
	Kind     string `json:"kind"`

	// Scope is the provider namespace the object lives in: the GitHub host,
	// the value the pull request's entity is scoped under. Target and DedupKey
	// are only unique inside it. Required on every write.
	Scope string `json:"scope"`

	// Target is the display key of the pull request the deliverable is about
	// ('owner/repo#123'). ExternalID is the provider-native id of the backing
	// object (the PR number, the review id); empty until the object exists.
	// URL links to it; empty until created.
	Target     string `json:"target"`
	ExternalID string `json:"external_id,omitempty"`
	URL        string `json:"url,omitempty"`

	// EntityID is the entity of the pull request the deliverable is or is a
	// review of. A rename of the entity's key moves Target with it, selected
	// by this column. Empty for a row whose writer could not resolve its
	// entity. Preserve-on-empty, like ExternalID; the FK is ON DELETE SET NULL.
	EntityID string `json:"entity_id,omitempty"`

	// NodeID is the pull request's GraphQL node id, where a writer learned it:
	// the reconciler's backstop matches a pr_created ledger row to its
	// deliverable by node id, which a repository rename does not change.
	// Preserve-on-empty, like ExternalID.
	NodeID string `json:"node_id,omitempty"`

	// State is the per-Kind lifecycle position. Use the DeliverableState*
	// consts; not DB-constrained so the set stays extensible.
	State string `json:"state"`

	// DedupKey is the stable natural key Upsert conflicts on. Built by
	// ObjectKey so every writer that sees the same logical
	// object lands on the same row.
	DedupKey string `json:"dedup_key"`

	// DetailsJSON is optional kind-specific payload. Empty string
	// serializes to SQL NULL.
	DetailsJSON string `json:"details_json,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Provider discriminators, shared by deliverables (always github) and the
// external-action ledger.
const (
	ProviderGitHub = "github"
	ProviderJira   = "jira"
	ProviderLinear = "linear"
	ProviderSlack  = "slack"
	// ProviderNetwork is the provider for an action whose target is a
	// bare network destination rather than an object in a named system — the
	// sandbox egress proxy's refused CONNECT, whose host is arbitrary
	// (pypi.org, api.github.com). It exists so the external-action row has a
	// provider that doesn't misfile a blocked host as a GitHub or Jira write.
	ProviderNetwork = "network"
)

// Deliverable kind discriminators.
const (
	DeliverableKindPullRequest = "pull_request"
	DeliverableKindReview      = "review"
)

// Deliverable state lifecycle values, grouped by kind. App-validated only —
// there is no DB CHECK on state.
//
// Some values intentionally alias across kinds (DeliverableStatePRPending and
// DeliverableStateReviewPending are both "pending"), so state is only
// meaningful read together with Kind — always branch on Deliverable.Kind
// first, never on the raw state string alone. The per-kind named consts
// exist to nudge callers to the right one for their kind; the equal string
// values mean a cross-kind mix-up won't surface as a type or string error.
const (
	// pull_request: 'pending' is intent-only (not yet on GitHub); the rest
	// mirror GitHub's PR lifecycle.
	DeliverableStatePRPending = "pending"
	DeliverableStatePRDraft   = "draft"
	DeliverableStatePROpen    = "open"
	DeliverableStatePRMerged  = "merged"
	DeliverableStatePRClosed  = "closed"

	// review: 'pending' is a draft staged TF-side; 'submitted' once it lands
	// on the PR; 'dismissed' if discarded.
	DeliverableStateReviewPending   = "pending"
	DeliverableStateReviewSubmitted = "submitted"
	DeliverableStateReviewDismissed = "dismissed"
)

// Providers is the closed provider vocabulary: what the audit ledger records an
// action against, which the Usage feeds' provider filter validates with.
func Providers() []string {
	return []string{
		ProviderGitHub, ProviderJira, ProviderLinear,
		ProviderSlack, ProviderNetwork,
	}
}

// ObjectKey builds a stable, provider-natural key, provider:kind:resource[:anchor]:
// the object_key an external action names the object it changed by, and the
// dedup key a pull request or review deliverable is upserted on. The same
// object maps to the same key whichever writer observed it.
//
// The key is ':'-delimited, so no segment may itself contain a ':' —
// ("a:b", "") and ("a", "b") would otherwise collapse to the same
// "...:a:b". Today's resources (GitHub node ids, Linear UUIDs,
// JiraIssueResource, SlackMessageResource) carry no colons; a future provider
// passing something URL-like must encode it first.
//
// resource and anchor are the caller's choice of *stable* coordinates. A
// resource is an id, never a name: a name changes with a rename, and a name
// GitHub or Jira has freed can be given to another object.
//
//   - resource: the provider's stable id for the object the key is about —
//     the pull request's GraphQL node id for a PR deliverable and for a review
//     of it, JiraIssueResource (the site and the issue id) for anything on a
//     Jira issue, the issue's UUID for anything on a Linear issue, and
//     SlackMessageResource (the channel's slack_channels row id and the
//     message's ts) for a Slack message. A key is unique only within its
//     scope; a Jira resource carries the site as well.
//   - anchor: an optional stable sub-discriminator appended when resource
//     alone isn't unique — a comment's id on its issue, the conversation that
//     owns a review draft. Empty when resource is already unique.
//
// Examples:
//
//	ObjectKey("github", "pull_request", "<PR node id>", "")                         => "github:pull_request:<PR node id>"
//	ObjectKey("github", "review",       "<PR node id>", "<conversation id>")        => "github:review:<PR node id>:<conversation id>"
//	ObjectKey("jira",   "issue",        JiraIssueResource(site, "10042"), "")      => "jira:issue:https%3A%2F%2Facme.atlassian.net/10042"
//	ObjectKey("jira",   "comment",      JiraIssueResource(site, "10042"), "20311") => "jira:comment:https%3A%2F%2Facme.atlassian.net/10042:20311"
//	ObjectKey("linear", "comment",      "<issue uuid>", "<comment id>")             => "linear:comment:<issue uuid>:<comment id>"
//	ObjectKey("slack",  "message",      SlackMessageResource("<channel row id>", ts), "") => "slack:message:<channel row id>/<ts>"
//
// Some stored deliverables carry a key that names their pull request by
// owner/repo#N instead, whose repository no registry row identified. Nothing
// writes that shape.
func ObjectKey(provider, kind, resource, anchor string) string {
	key := provider + ":" + kind + ":" + resource
	if anchor != "" {
		key += ":" + anchor
	}
	return key
}

// SlackMessageResource is the object-key resource of a Slack message: the
// channel's slack_channels row id and the message's ts. A Slack message
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

// JiraIssueResource is the object-key resource every object on a Jira issue is
// keyed under: the issue's site and its numeric id. Both halves are
// needed. The key a person reads changes when the issue moves or its project's
// key is renamed, and the id repeats across sites, so an org with two Jira
// sites has two issue 10042s. The site is query-escaped, which leaves no ':'
// in it (ObjectKey's separator) and no '/' (this one's), so the pair
// splits back apart unambiguously. "" when either half is unknown: there is no
// stable resource to key on.
func JiraIssueResource(site, issueID string) string {
	if site == "" || issueID == "" {
		return ""
	}
	return url.QueryEscape(site) + "/" + issueID
}

// ParseJiraIssueResource splits a JiraIssueResource back into its site and
// issue id. ok=false for anything else — a key spelling an issue key, which
// carries no id.
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

// EntityResource is the object-key resource segment objects on the entity
// carrying (source, scope, externalID) are keyed under: the issue's site and id
// for Jira (JiraIssueResource), the provider id itself for every other source.
// "" when there is no id.
func EntityResource(source, scope, externalID string) string {
	if externalID == "" {
		return ""
	}
	if source == ProviderJira {
		return JiraIssueResource(scope, externalID)
	}
	return externalID
}

// DeliverablePullRequestNodeID returns the GraphQL node id of the pull request a
// GitHub pull_request or review deliverable is keyed on (PullRequestDedupKey,
// ReviewDedupKey). ok=false for any other deliverable, and for a stored row whose
// key still leads with owner/repo#N: a node id is base64, which never holds a
// '#'.
func DeliverablePullRequestNodeID(a Deliverable) (string, bool) {
	if a.Provider != ProviderGitHub || (a.Kind != DeliverableKindPullRequest && a.Kind != DeliverableKindReview) {
		return "", false
	}
	rest, found := strings.CutPrefix(a.DedupKey, ProviderGitHub+":"+a.Kind+":")
	if !found {
		return "", false
	}
	nodeID, _, _ := strings.Cut(rest, ":")
	if nodeID == "" || strings.Contains(nodeID, "#") {
		return "", false
	}
	return nodeID, true
}

// reconcilableNonTerminal lists, per Kind, the states the reconciler treats
// as non-terminal AND backed by a fetchable GitHub object — the exact set
// Deliverables.ListNonTerminalBySystem returns and the conversation-scoped
// refresh filters to. It is the single source of truth both the store's SQL
// predicate and IsReconcilableNonTerminal derive from.
//
// PR 'pending' is deliberately excluded: a pending PR is intent-only (no
// number, no node id), so there is no backing object to refresh. Terminal
// states (PR merged/closed) are absent, so a terminal deliverable is never
// re-queried.
//
// Review is absent entirely: a review is staged TF-side until the atomic
// create+submit at approval, so a pending review draft has no GitHub object to
// reconcile, and the instant it submits it is already terminal.
var reconcilableNonTerminal = map[string]map[string]bool{
	DeliverableKindPullRequest: {DeliverableStatePRDraft: true, DeliverableStatePROpen: true},
}

// IsReconcilableNonTerminal reports whether a deliverable of (kind, state) is
// in the reconciler's working set: non-terminal AND backed by a fetchable
// GitHub object. The conversation-scoped refresh filters a conversation's
// deliverables through this so it reconciles exactly what the org-wide query
// would; the ListNonTerminal store tests pin it equal to the store's SQL
// predicate.
func IsReconcilableNonTerminal(kind, state string) bool {
	return reconcilableNonTerminal[kind][state]
}
