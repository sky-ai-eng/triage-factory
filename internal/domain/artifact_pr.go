package domain

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// PRArtifactSnapshot is a point-in-time copy of a pull request's editable surface — its
// title and body. A pull_request artifact carries two: the agent's immutable
// Proposed draft (captured at create) and the mutable Snapshot (refreshed as a
// human edits the draft, and again at approval). Keeping both lets the
// approve-time human-feedback diff compare what the agent drafted against what
// the human shipped without a second source of truth, and lets the proposed
// draft survive abandonment for the audit ledger.
type PRArtifactSnapshot struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// PRArtifactDetails is the kind-specific payload in a pull_request artifact's
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
type PRArtifactDetails struct {
	NodeID     string             `json:"node_id,omitempty"`
	HeadBranch string             `json:"head_branch,omitempty"`
	Base       string             `json:"base,omitempty"`
	Proposed   PRArtifactSnapshot `json:"proposed"`
	Snapshot   PRArtifactSnapshot `json:"snapshot"`
}

// PullRequestTarget is the artifact Target for a PR: owner/repo#<number>.
func PullRequestTarget(repoPath string, number int) string {
	return fmt.Sprintf("%s#%d", repoPath, number)
}

// GitHubPullURL builds the github.com web link for a PR — the human-facing URL
// the audit feed links a row to when the action's build site has no html_url in
// hand (a board-drag, a review-thread reply, a review whose artifact carries no
// URL). Always github.com, matching branchWebURL's documented default (the
// product is GitHub-focused; GHES web hosts aren't modeled). repoPath is a
// validated owner/repo, so it isn't re-escaped.
func GitHubPullURL(repoPath string, number int) string {
	return fmt.Sprintf("https://github.com/%s/pull/%d", repoPath, number)
}

// PullRequestDedupKey is the stable key every writer that touches a given PR
// upserts on: github:pull_request:owner/repo#<number>. The draft PR is created
// for real immediately (no branch-anchored pending phase in this path), so it
// keys on the number from the start.
func PullRequestDedupKey(repoPath string, number int) string {
	return ArtifactDedupKey(ArtifactProviderGitHub, ArtifactKindPullRequest, PullRequestTarget(repoPath, number), "")
}

// NewPullRequestArtifact builds the durable pull_request artifact for a PR the
// agent just opened. repoPath is "owner/repo"; number / nodeID / htmlURL are the
// created PR's coordinates; head / base are its refs; title / body are the
// agent's draft (the proposed snapshot); draft selects the initial lifecycle
// state (draft vs open). Both Proposed and Snapshot start equal to the agent's
// draft.
//
// This is the single source of truth for the PR-artifact shape, so the create
// writer (the exec choke point) and any later reconciliation land on the same
// deduped row. RunID/OrgID/TeamID are stamped by the recording client, not here.
func NewPullRequestArtifact(repoPath string, number int, nodeID, head, base, htmlURL, title, body string, draft bool) Artifact {
	state := ArtifactStatePROpen
	if draft {
		state = ArtifactStatePRDraft
	}
	snap := PRArtifactSnapshot{Title: title, Body: body}
	details, err := json.Marshal(PRArtifactDetails{
		NodeID:     nodeID,
		HeadBranch: head,
		Base:       base,
		Proposed:   snap,
		Snapshot:   snap,
	})
	if err != nil {
		// This fixed shape can't realistically fail to marshal; degrade to
		// empty details rather than dropping the artifact entirely.
		details = nil
	}
	return Artifact{
		Provider:    ArtifactProviderGitHub,
		Kind:        ArtifactKindPullRequest,
		Target:      PullRequestTarget(repoPath, number),
		ExternalID:  strconv.Itoa(number),
		URL:         htmlURL,
		State:       state,
		DedupKey:    PullRequestDedupKey(repoPath, number),
		DetailsJSON: string(details),
	}
}

// MarshalPRArtifactDetails serializes details to a DetailsJSON string. Returns ""
// on a marshal error — the fixed shape can't realistically fail, and callers
// treat "" as empty details (→ SQL NULL).
func MarshalPRArtifactDetails(d PRArtifactDetails) string {
	b, err := json.Marshal(d)
	if err != nil {
		return ""
	}
	return string(b)
}

// ParsePRArtifactDetails unmarshals a pull_request artifact's DetailsJSON.
// Returns the zero value (no error) on empty input so callers can treat a
// detail-less row as "no snapshot" without special-casing.
func ParsePRArtifactDetails(detailsJSON string) (PRArtifactDetails, error) {
	var d PRArtifactDetails
	if strings.TrimSpace(detailsJSON) == "" {
		return d, nil
	}
	if err := json.Unmarshal([]byte(detailsJSON), &d); err != nil {
		return PRArtifactDetails{}, err
	}
	return d, nil
}

// FirstDraftPullRequest returns the first draft pull_request artifact in arts,
// or nil if none. A run with a draft PR is what parks it in pending_approval and
// what an abandon closes; sharing this predicate keeps every consumer (the
// spawner park check, the run-response discriminator, the abandon path) agreeing
// on what "the run has a pending PR" means.
func FirstDraftPullRequest(arts []Artifact) *Artifact {
	for i := range arts {
		if arts[i].Kind == ArtifactKindPullRequest && arts[i].State == ArtifactStatePRDraft {
			return &arts[i]
		}
	}
	return nil
}

// ParsePRTarget splits a pull_request artifact's Target (owner/repo#<number>)
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
