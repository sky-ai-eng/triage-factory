package domain

import (
	"sort"
)

// PRSnapshot is the extracted state we store for a GitHub pull request.
// Every field here can trigger events when it changes between poll cycles.
type PRSnapshot struct {
	// Identity
	NodeID   string `json:"node_id"` // GitHub GraphQL node ID — stored in snapshot for entity-based refresh
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
	// HeadCommittedAt is the git-side commit time of the current HEAD (ISO
	// 8601 from GitHub's commit.committedDate). Used as the source time
	// for new_commits events so the factory's chain animation orders
	// transitions by when the push actually happened rather than when we
	// polled. Empty when the field is missing (pre-field snapshots).
	HeadCommittedAt string `json:"head_committed_at,omitempty"`

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

	// Timeline carries a tail of PR-side events (label add/remove, review
	// request, ready-for-review) with their per-action createdAt
	// timestamps, pulled from GraphQL timelineItems on the same PR query
	// (no extra HTTP request). Used by the diff layer to attach honest
	// source times to events that have no per-field timestamp on the
	// snapshot itself.
	//
	// json:"-" so the timeline doesn't bloat snapshot_json — it's a
	// transient metadata channel, not state we diff against. Each poll
	// brings fresh timeline data; we just need it during the diff that
	// consumes the same wire response.
	Timeline []TimelineEvent `json:"-"`
}

// TimelineEvent is one entry from GitHub's PullRequest.timelineItems.
// Kind discriminates which fields are populated. Only the event types we
// actually use for source-time enrichment are modeled here — adding a new
// kind means extending both the GraphQL fragment and this type.
type TimelineEvent struct {
	Kind      string `json:"kind"`       // labeled | unlabeled | review_requested | ready_for_review
	CreatedAt string `json:"created_at"` // RFC3339 from GraphQL
	// Label is the label name for kind ∈ {labeled, unlabeled}.
	Label string `json:"label,omitempty"`
	// Reviewer is "login" for User reviewers or "org/slug" for Team
	// reviewers; populated for kind = review_requested. Mirrors the
	// shape used by PRSnapshot.ReviewRequests so diff lookups can
	// match on equality.
	Reviewer string `json:"reviewer,omitempty"`
}

// ReviewState captures one reviewer's latest review.
type ReviewState struct {
	// ID is the review's GraphQL node id, when known. Populated by the PR
	// refresh path so the artifact reconciler (TFAC-464) can match a pending
	// review artifact (keyed on this id) to its submitted/dismissed state.
	// Empty on snapshots from before the field existed and on paths that
	// don't request it; the review-transition diff keys on Author+State and
	// never reads ID, so its absence is inert there.
	ID          string `json:"id,omitempty"`
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
// empty. Used by dashboard and display code for aggregate CI status badges.
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

// JiraSnapshot is the extracted state we diff against on each poll to emit
// per-action events. Keep it small — large bulk text (issue descriptions,
// PR bodies) lives on entities.description instead so diff reads don't
// drag it through every refresh cycle.
type JiraSnapshot struct {
	Key     string `json:"key"`
	Summary string `json:"summary"`
	Status  string `json:"status"`
	// StatusID is Jira's identifier for the status Status names. It is the
	// stable half of the pair: a workflow status can be renamed, and matching
	// on the id is what keeps rule membership and change detection right when
	// it is. Empty on snapshots captured before the field existed, and on any
	// response without one — every comparison falls back to the name there.
	StatusID string `json:"status_id,omitempty"`
	Assignee string `json:"assignee"` // display name (UI surfaces)
	// AssigneeAccountID is the Atlassian stable identifier — accountId
	// on Cloud, legacy key on Server / DC. Captured alongside the
	// display name in issueToState so the predicate matcher
	// (assignee_in / commenter_in / reporter_in) has a stable
	// comparison target. Empty on snapshots that predate the field.
	AssigneeAccountID string   `json:"assignee_account_id,omitempty"`
	Priority          string   `json:"priority"`
	Labels            []string `json:"labels"`
	IssueType         string   `json:"issue_type"`
	ParentKey         string   `json:"parent_key"`
	CommentCount      int      `json:"comment_count"`
	URL               string   `json:"url"`
	// CreatedAt is Jira's ISO-8601 timestamp for when the ticket was created
	// (fields.created). Used for newest-first ordering in carry-over. Empty
	// on snapshots that predate this field — callers should fall back to the
	// entity's TF-side created_at when sort-critical.
	CreatedAt string `json:"created_at,omitempty"`
	// UpdatedAt is Jira's last-modified timestamp on the issue (fields.updated).
	// Used by the diff layer as a fallback source time for events without a
	// per-action timestamp on the snapshot (status changes, assignment
	// changes, priority changes, new comments). Better than detection time
	// without firing a separate changelog API call. Empty on snapshots that
	// predate this field — diff falls back to detection time.
	UpdatedAt string `json:"updated_at,omitempty"`
	// OpenSubtaskCount is the number of this issue's child subtasks whose
	// status is NOT in the configured Done.Members set. Used to suppress
	// task creation for parent-of-subtasks tickets (the decomposition
	// exists for a reason — delegating the parent is almost always wrong)
	// and to fire jira:issue:became_atomic when an issue transitions from
	// "has open subtasks" to "none open".
	OpenSubtaskCount int `json:"open_subtask_count"`
}

// StatusRef pairs the snapshot's status name with its id, for comparison
// against a rule's members through JiraStatusRef.SameStatus.
func (s JiraSnapshot) StatusRef() JiraStatusRef {
	return JiraStatusRef{ID: s.StatusID, Name: s.Status}
}
