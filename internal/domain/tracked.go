package domain

import (
	"sort"
	"time"
)

// TrackedItem represents a GitHub PR or Jira issue we're actively monitoring for state changes.
type TrackedItem struct {
	Source       string // "github" | "jira"
	SourceID     string // "owner/repo#42" (GitHub) or "SKY-45" (Jira)
	TaskID       string // FK to tasks table
	NodeID       string // GitHub GraphQL node ID (empty for Jira)
	Snapshot     string // JSON-serialized snapshot
	TrackedSince time.Time
	LastPolled   *time.Time
	TerminalAt   *time.Time // non-nil = merged/closed/done
}

// PRSnapshot is the extracted state we store for a GitHub pull request.
// Every field here can trigger events when it changes between poll cycles.
type PRSnapshot struct {
	// Identity
	Number   int    `json:"number"`
	Title    string `json:"title"`
	Author   string `json:"author"`    // login of the PR author
	Repo     string `json:"repo"`      // "owner/repo"
	HeadRepo string `json:"head_repo"` // fork repo if different
	URL      string `json:"url"`

	// State
	State     string `json:"state"` // OPEN, CLOSED, MERGED
	IsDraft   bool   `json:"is_draft"`
	Merged    bool   `json:"merged"`
	Mergeable string `json:"mergeable"` // MERGEABLE, CONFLICTING, UNKNOWN

	// Branches
	HeadRef string `json:"head_ref"`
	BaseRef string `json:"base_ref"`
	HeadSHA string `json:"head_sha"`

	// Size
	Additions    int `json:"additions"`
	Deletions    int `json:"deletions"`
	ChangedFiles int `json:"changed_files"`

	// CI — structured per-check-run data, scoped to the current head SHA.
	// Deduped by Name at ingest time (latest execution wins by highest ID), so
	// this represents "the current state of each named check" not the full
	// history of executions. nil means "unknown prior state" (old snapshot
	// from before this field existed); empty slice means "polled, no checks".
	CheckRuns []CheckRun `json:"check_runs"`

	// Reviews
	ReviewRequests []string      `json:"review_requests"` // logins of users/teams with pending requests
	Reviews        []ReviewState `json:"reviews"`         // latest review per reviewer
	ReviewCount    int           `json:"review_count"`    // total reviews submitted

	// Metadata
	Labels       []string `json:"labels"`
	CommentCount int      `json:"comment_count"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
	MergedAt     string   `json:"merged_at,omitempty"`
	ClosedAt     string   `json:"closed_at,omitempty"`
}

// ReviewState captures one reviewer's latest review.
type ReviewState struct {
	Author      string `json:"author"`
	State       string `json:"state"` // APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED, PENDING
	SubmittedAt string `json:"submitted_at"`
}

// CheckRun is a single execution of a CI check on a commit.
//
// ID is GitHub's database ID — monotonically increasing and unique per
// execution, so re-running a workflow on the same SHA produces a new ID.
// That's why ID (not Name) is the identity key for re-trigger detection:
// "same SHA, same name, new ID" means a re-run, and "new ID with a failing
// conclusion" means we have a new failure to act on.
//
// DetailsURL is GitHub's details_url / detailsUrl — the "more info" link the
// CI provider attaches to the check run. For GitHub Actions checks this is
// the workflow-run/job page (/actions/runs/N/job/M); for third-party CI
// systems it's wherever the provider wants users to land. This is NOT the
// narrower GitHub check-run page URL (/runs/N) — we deliberately store the
// details URL because (a) it's the more useful human-facing link across
// providers and (b) parseWorkflowRunIDFromURL depends on the Actions URL
// shape exposed here.
//
// WorkflowRunID is the GitHub Actions workflow run database ID, pulled from
// the GraphQL workflowRun field on the containing check suite. It's zero for
// check runs from third-party CI systems (Supabase, Circle, etc.) — those
// can't be fetched via the Actions log-download endpoint anyway.
type CheckRun struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Status        string `json:"status"`     // queued | in_progress | completed
	Conclusion    string `json:"conclusion"` // success | failure | cancelled | timed_out | action_required | neutral | skipped | stale | ""
	CompletedAt   string `json:"completed_at"`
	DetailsURL    string `json:"details_url"`
	WorkflowRunID int64  `json:"workflow_run_id,omitempty"`
}

// IsFailingConclusion reports whether a check-run conclusion is one we treat
// as a failure worth surfacing as an event. Kept in one place so the poller,
// the diff logic, and any UI badge code agree on what "failing" means.
func IsFailingConclusion(conclusion string) bool {
	switch conclusion {
	case "failure", "timed_out", "cancelled", "action_required":
		return true
	}
	return false
}

// CIStatusFromCheckRuns derives a lowercase aggregate CI status from a list of
// check runs. Returns "failure" if any check failed, "pending" if any check is
// still running, "success" if all completed non-failing, or "" if the list is
// empty. Values match domain.Task.CIStatus so the tracker can use this
// directly when building tasks from snapshots.
//
// The success bucket is intentionally permissive — *any* completed check whose
// conclusion isn't in IsFailingConclusion counts as success-like. That covers
// "success", "neutral", "skipped", "stale" (post-rebase, treated as
// non-blocking by GitHub), empty conclusion on a completed check, and any
// future values GitHub adds to the enum. This matches how the aggregate
// counts in internal/github/status.go classify check runs, so the two paths
// can't drift out of sync.
func CIStatusFromCheckRuns(runs []CheckRun) string {
	if len(runs) == 0 {
		return ""
	}
	var hasFailure, hasPending, hasSuccess bool
	for _, r := range runs {
		if r.Status != "completed" {
			hasPending = true
			continue
		}
		if IsFailingConclusion(r.Conclusion) {
			hasFailure = true
			continue
		}
		// Completed and not failing — treat as success-like regardless of
		// the specific conclusion string.
		hasSuccess = true
	}
	switch {
	case hasFailure:
		return "failure"
	case hasPending:
		return "pending"
	case hasSuccess:
		return "success"
	}
	return ""
}

// DedupCheckRunsByName collapses multiple executions of the same check name
// down to the latest (highest ID). GitHub assigns check-run IDs monotonically
// at creation time, so the highest ID is always the most recent execution.
//
// Returns a deterministically-sorted slice (by Name ascending) so the
// serialized snapshot compares byte-stable across polls when the underlying
// data hasn't changed. Input nil or empty returns a non-nil empty slice so
// callers can distinguish "polled, no checks" from "unknown prior state".
func DedupCheckRunsByName(runs []CheckRun) []CheckRun {
	if len(runs) == 0 {
		return []CheckRun{}
	}
	byName := make(map[string]CheckRun, len(runs))
	for _, r := range runs {
		if existing, ok := byName[r.Name]; ok && existing.ID >= r.ID {
			continue
		}
		byName[r.Name] = r
	}
	out := make([]CheckRun, 0, len(byName))
	for _, r := range byName {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// JiraSnapshot is the extracted state we store for a Jira issue.
type JiraSnapshot struct {
	Key          string   `json:"key"`
	Summary      string   `json:"summary"`
	Status       string   `json:"status"`
	Assignee     string   `json:"assignee"`
	Priority     string   `json:"priority"`
	Labels       []string `json:"labels"`
	IssueType    string   `json:"issue_type"`
	ParentKey    string   `json:"parent_key"`
	CommentCount int      `json:"comment_count"`
	URL          string   `json:"url"`
}
