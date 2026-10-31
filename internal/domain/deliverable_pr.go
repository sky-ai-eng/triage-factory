package domain

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// PRDeliverableSnapshot is a point-in-time copy of a pull request's editable surface — its
// title and body. A pull_request deliverable carries two: the agent's immutable
// Proposed draft (captured at create) and the mutable Snapshot (refreshed as a
// human edits the draft, and again at approval). Keeping both lets the
// approve-time human-feedback diff compare what the agent drafted against what
// the human shipped without a second source of truth, and lets the proposed
// draft survive abandonment for the audit ledger.
type PRDeliverableSnapshot struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// PRDeliverableDetails is the kind-specific payload in a pull_request deliverable's
// DetailsJSON:
//
//   - NodeID: the PR's durable GraphQL global node id — the handle reconciliation
//     keys on, distinct from the per-repo integer number.
//   - HeadBranch / Base: the PR's refs.
//   - Proposed: the agent's first-draft {title, body}. Write-once — set at create
//     and never modified, so it stays the baseline for the approve-time
//     human-feedback diff and survives an abandon.
//   - Snapshot: the latest known {title, body} (mirrors the live PR). Refreshed on
//     every human edit (PATCH) and at approval.
//   - Resolution: who moved the row into the state it holds — one of the
//     PRResolution* values, stamped by the verb that resolved it (approve,
//     dismiss, reject) or by the reconciler when it observed the change on
//     GitHub. The state alone cannot say: a closed PR was dismissed with its
//     branch kept, rejected with its branch deleted, or closed by someone on
//     GitHub, and the agent-facing resolution note has to read differently for
//     each. It lives on the PR row because that note is derived from this row
//     alone — the resume ledger re-reads it with no join. Stored rows written
//     before the field existed carry none, and read as the TF verb their
//     state implies.
type PRDeliverableDetails struct {
	NodeID     string                `json:"node_id,omitempty"`
	HeadBranch string                `json:"head_branch,omitempty"`
	Base       string                `json:"base,omitempty"`
	Proposed   PRDeliverableSnapshot `json:"proposed"`
	Snapshot   PRDeliverableSnapshot `json:"snapshot"`
	Resolution string                `json:"resolution,omitempty"`
}

// PRResolution* is the closed vocabulary of PRDeliverableDetails.Resolution.
const (
	// PRResolutionApproved: a human marked the draft ready through TF.
	PRResolutionApproved = "approved"
	// PRResolutionDismissed: a human closed the draft through TF, branch kept.
	PRResolutionDismissed = "dismissed"
	// PRResolutionRejected: a human closed the draft through TF and had its
	// head branch deleted from the upstream.
	PRResolutionRejected = "rejected"
	// PRResolutionGitHub: the reconciler observed the transition on GitHub —
	// nobody resolved it through TF.
	PRResolutionGitHub = "github"
)

// StampPRResolution returns detailsJSON with Resolution set to resolution.
// Unparseable details are returned unchanged: the caller is recording who
// moved the row, and a corrupt payload is not made worse by leaving it be.
func StampPRResolution(detailsJSON, resolution string) string {
	d, err := ParsePRDeliverableDetails(detailsJSON)
	if err != nil {
		return detailsJSON
	}
	d.Resolution = resolution
	return MarshalPRDeliverableDetails(d)
}

// PullRequestTarget is the deliverable Target for a PR: owner/repo#<number>.
func PullRequestTarget(repoPath string, number int) string {
	return fmt.Sprintf("%s#%d", repoPath, number)
}

// GitHubPullURLBase builds the web link for a PR on a specific GitHub host base
// — github.com, a GHES host, or a GHEC data-residency host — so a human-facing
// link points at the server the object actually lives on. base is the org's
// user-facing web base (e.g. "https://github.com" or "https://ghe.example.com"),
// as reported by the credential resolver's BaseURLFor; a trailing slash is
// trimmed. An empty base names no GitHub, and there is no link on it: "".
// repoPath is a validated owner/repo, so it isn't re-escaped.
func GitHubPullURLBase(base, repoPath string, number int) string {
	base = strings.TrimRight(base, "/")
	if base == "" {
		return ""
	}
	return fmt.Sprintf("%s/%s/pull/%d", base, repoPath, number)
}

// PullRequestDedupKey is the stable key every writer that touches a given PR
// upserts on: github:pull_request:<node id>, the pull request's GraphQL node
// id. A repository rename does not change it, and a pull request in another
// repository under a reused name has its own. "" for an empty node id: there
// is nothing stable to key the row on.
func PullRequestDedupKey(nodeID string) string {
	if nodeID == "" {
		return ""
	}
	return ObjectKey(ProviderGitHub, DeliverableKindPullRequest, nodeID, "")
}

// NewPullRequestDeliverable builds the durable pull_request deliverable for a PR the
// agent just opened. repoPath is "owner/repo"; number / nodeID / htmlURL are the
// created PR's coordinates; head / base are its refs; title / body are the
// agent's draft (the proposed snapshot); draft selects the initial lifecycle
// state (draft vs open). Both Proposed and Snapshot start equal to the agent's
// draft.
//
// The row is keyed on the node id (PullRequestDedupKey), which also goes on
// the deliverable's node_id column and in its details: the column is what the
// reconciler's backstop matches a pr_created ledger row against. A caller
// with no node id has no key to write under and records nothing.
//
// This is the single source of truth for the PR-deliverable shape, so the create
// writer (the exec choke point) and any later reconciliation land on the same
// deduped row. ConversationID/OrgID/TeamID are stamped by the recording client, not here.
func NewPullRequestDeliverable(repoPath string, number int, nodeID, head, base, htmlURL, title, body string, draft bool) Deliverable {
	state := DeliverableStatePROpen
	if draft {
		state = DeliverableStatePRDraft
	}
	snap := PRDeliverableSnapshot{Title: title, Body: body}
	details, err := json.Marshal(PRDeliverableDetails{
		NodeID:     nodeID,
		HeadBranch: head,
		Base:       base,
		Proposed:   snap,
		Snapshot:   snap,
	})
	if err != nil {
		// This fixed shape can't realistically fail to marshal; degrade to
		// empty details rather than dropping the deliverable entirely.
		details = nil
	}
	return Deliverable{
		Provider:    ProviderGitHub,
		Kind:        DeliverableKindPullRequest,
		Target:      PullRequestTarget(repoPath, number),
		ExternalID:  strconv.Itoa(number),
		URL:         htmlURL,
		NodeID:      nodeID,
		State:       state,
		DedupKey:    PullRequestDedupKey(nodeID),
		DetailsJSON: string(details),
	}
}

// MarshalPRDeliverableDetails serializes details to a DetailsJSON string. Returns ""
// on a marshal error — the fixed shape can't realistically fail, and callers
// treat "" as empty details (→ SQL NULL).
func MarshalPRDeliverableDetails(d PRDeliverableDetails) string {
	b, err := json.Marshal(d)
	if err != nil {
		return ""
	}
	return string(b)
}

// ParsePRDeliverableDetails unmarshals a pull_request deliverable's DetailsJSON.
// Returns the zero value (no error) on empty input so callers can treat a
// detail-less row as "no snapshot" without special-casing.
func ParsePRDeliverableDetails(detailsJSON string) (PRDeliverableDetails, error) {
	var d PRDeliverableDetails
	if strings.TrimSpace(detailsJSON) == "" {
		return d, nil
	}
	if err := json.Unmarshal([]byte(detailsJSON), &d); err != nil {
		return PRDeliverableDetails{}, err
	}
	return d, nil
}

// isDraftPullRequest is the single-deliverable predicate for "an unresolved draft
// PR" — a pull_request still in state=draft. FirstDraftPullRequest and
// UnresolvedDeliverableCounts both go through it so the "unresolved PR" definition
// lives in exactly one place.
func isDraftPullRequest(a Deliverable) bool {
	return a.Kind == DeliverableKindPullRequest && a.State == DeliverableStatePRDraft
}

// FirstDraftPullRequest returns the first draft pull_request deliverable in arts,
// or nil if none. A draft PR is an unresolved deliverable (the derived approval
// signal) and what an abandon closes; sharing this predicate keeps
// every consumer (HasUnresolvedDeliverables, the abandon path) agreeing on what "the
// conversation has an unresolved PR" means.
func FirstDraftPullRequest(arts []Deliverable) *Deliverable {
	for i := range arts {
		if isDraftPullRequest(arts[i]) {
			return &arts[i]
		}
	}
	return nil
}

// AllDraftPullRequests returns every draft pull_request deliverable in arts (the
// plural of FirstDraftPullRequest), in slice order. The deliverable set is plural
// per conversation — a conversation can open several draft PRs — so the projection
// (pending_deliverable_ids) and the task-level resolve-all teardown both iterate the
// whole set rather than just the first. Shares isDraftPullRequest so it can't
// drift from the single-deliverable predicate.
func AllDraftPullRequests(arts []Deliverable) []Deliverable {
	var out []Deliverable
	for i := range arts {
		if isDraftPullRequest(arts[i]) {
			out = append(out, arts[i])
		}
	}
	return out
}

// ParsePRTarget splits a pull_request deliverable's Target (owner/repo#<number>)
// into its parts. ok=false when the shape doesn't match — a caller acting on the
// PR (edit / approve / close) needs all three coordinates, so it must fail
// rather than guess.
func ParsePRTarget(target string) (owner, repo string, number int, ok bool) {
	hash := strings.LastIndex(target, "#")
	if hash <= 0 || hash == len(target)-1 {
		return "", "", 0, false
	}
	repoPath := target[:hash]
	n, err := strconv.Atoi(target[hash+1:])
	if err != nil || n <= 0 {
		return "", "", 0, false
	}
	parts := strings.Split(repoPath, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", 0, false
	}
	return parts[0], parts[1], n, true
}
