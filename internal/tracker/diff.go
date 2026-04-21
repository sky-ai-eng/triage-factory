package tracker

import (
	"encoding/json"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// DiffPRSnapshots compares two PR snapshots and returns per-action events for
// every detected change. Each event carries typed metadata from the events
// package, EntityID pointing to the PR entity, and DedupKey for open-set
// discriminators (labels). A zero-value prev (Number == 0) indicates first
// discovery — emits initial events from the current state.
//
// userTeams is the "org/slug" list of teams the session user belongs to,
// used so team-requested reviews also emit pr:review_requested.
//
// Pure function — no IO.
func DiffPRSnapshots(prev, curr domain.PRSnapshot, entityID, username string, userTeams []string) []domain.Event {
	now := time.Now()
	eid := &entityID
	authorIsSelf := curr.Author == username
	var evts []domain.Event

	emit := func(eventType, dedupKey string, metadata any) {
		metaJSON, _ := json.Marshal(metadata)
		evts = append(evts, domain.Event{
			EventType:    eventType,
			EntityID:     eid,
			DedupKey:     dedupKey,
			MetadataJSON: string(metaJSON),
			CreatedAt:    now,
		})
	}

	// --- First discovery: no previous state to diff against ----------------
	// On first discovery we DON'T emit events — the entity just gets created
	// and the snapshot seeds. Events fire on the NEXT poll when we can
	// meaningfully diff. Exception: terminal states (merged/closed) emit
	// immediately since there won't be another diff.
	if prev.Number == 0 {
		if curr.Merged {
			emit(domain.EventGitHubPRMerged, "", events.GitHubPRMergedMetadata{
				Author: curr.Author, AuthorIsSelf: authorIsSelf,
				Repo: curr.Repo, PRNumber: curr.Number,
				MergedBy: "", HeadSHA: curr.HeadSHA, Labels: curr.Labels,
			})
		} else if curr.State == "CLOSED" {
			emit(domain.EventGitHubPRClosed, "", events.GitHubPRClosedMetadata{
				Author: curr.Author, AuthorIsSelf: authorIsSelf,
				Repo: curr.Repo, PRNumber: curr.Number,
				ClosedBy: "", Labels: curr.Labels,
			})
		}
		// For open PRs: no initial event. The next poll will diff against
		// this snapshot and emit real per-action events (CI checks, reviews,
		// review requests, etc.) when first observed.
		return evts
	}

	// --- Entity-terminating transitions ------------------------------------

	if !prev.Merged && curr.Merged {
		emit(domain.EventGitHubPRMerged, "", events.GitHubPRMergedMetadata{
			Author: curr.Author, AuthorIsSelf: authorIsSelf,
			Repo: curr.Repo, PRNumber: curr.Number,
			MergedBy: "", HeadSHA: curr.HeadSHA, Labels: curr.Labels,
		})
	}

	if prev.State != "CLOSED" && curr.State == "CLOSED" && !curr.Merged {
		emit(domain.EventGitHubPRClosed, "", events.GitHubPRClosedMetadata{
			Author: curr.Author, AuthorIsSelf: authorIsSelf,
			Repo: curr.Repo, PRNumber: curr.Number,
			ClosedBy: "", Labels: curr.Labels,
		})
	}

	// --- Draft → Ready for review ------------------------------------------

	if prev.IsDraft && !curr.IsDraft {
		emit(domain.EventGitHubPRReadyForReview, "", events.GitHubPRReadyForReviewMetadata{
			Author: curr.Author, AuthorIsSelf: authorIsSelf,
			Repo: curr.Repo, PRNumber: curr.Number,
			HeadSHA: curr.HeadSHA, Labels: curr.Labels,
		})
	}

	// --- Per-check CI events -----------------------------------------------
	// Emit one event per check-run that transitions to a new conclusion.
	// Identity: check-run ID. Same ID seen again → no event.
	//
	// nil prev.CheckRuns (discovery snapshot, lightweight fragment) is treated
	// as empty — every check in curr is "new." This ensures failing CI on a
	// newly-tracked entity is surfaced on the first full refresh, not deferred
	// to the second poll.
	if curr.CheckRuns != nil {
		prevByID := make(map[int64]domain.CheckRun, len(prev.CheckRuns))
		for _, cr := range prev.CheckRuns {
			prevByID[cr.ID] = cr
		}

		for _, cr := range curr.CheckRuns {
			prevCR, existed := prevByID[cr.ID]

			if domain.IsFailingConclusion(cr.Conclusion) {
				if existed && domain.IsFailingConclusion(prevCR.Conclusion) {
					continue // already failing, old signal
				}
				emit(domain.EventGitHubPRCICheckFailed, "", events.GitHubPRCICheckFailedMetadata{
					Author: curr.Author, AuthorIsSelf: authorIsSelf,
					CheckRunID: cr.ID, CheckName: cr.Name, CheckURL: cr.DetailsURL,
					WorkflowRunID: cr.WorkflowRunID,
					HeadSHA:       curr.HeadSHA, Repo: curr.Repo, PRNumber: curr.Number,
					IsDraft: curr.IsDraft, Labels: curr.Labels,
				})
			} else if cr.Conclusion != "" && !domain.IsFailingConclusion(cr.Conclusion) {
				// Any non-failing completed conclusion counts as "passed":
				// success, neutral, skipped, stale. This matters for the
				// inline close check — a check that was failing and transitions
				// to skipped (e.g., path filter on new commits) needs to emit
				// ci_check_passed so ci_check_failed tasks can close.
				if existed && !domain.IsFailingConclusion(prevCR.Conclusion) && prevCR.Conclusion != "" {
					continue // already non-failing, old signal
				}
				emit(domain.EventGitHubPRCICheckPassed, "", events.GitHubPRCICheckPassedMetadata{
					Author: curr.Author, AuthorIsSelf: authorIsSelf,
					CheckRunID: cr.ID, CheckName: cr.Name, Conclusion: cr.Conclusion,
					WorkflowRunID: cr.WorkflowRunID,
					HeadSHA:       curr.HeadSHA, Repo: curr.Repo, PRNumber: curr.Number,
					IsDraft: curr.IsDraft, Labels: curr.Labels,
				})
			}
		}
	}

	// --- New commits -------------------------------------------------------

	if prev.HeadSHA != "" && curr.HeadSHA != "" && prev.HeadSHA != curr.HeadSHA {
		emit(domain.EventGitHubPRNewCommits, "", events.GitHubPRNewCommitsMetadata{
			Author: curr.Author, AuthorIsSelf: authorIsSelf,
			IsDraft: curr.IsDraft, CommitCount: 0, // count not available in snapshot
			HeadSHA: curr.HeadSHA, PrevHeadSHA: prev.HeadSHA,
			Repo: curr.Repo, PRNumber: curr.Number, Labels: curr.Labels,
		})
	}

	// --- Merge conflicts ---------------------------------------------------

	if prev.Mergeable != "CONFLICTING" && curr.Mergeable == "CONFLICTING" {
		emit(domain.EventGitHubPRConflicts, "", events.GitHubPRConflictsMetadata{
			Author: curr.Author, AuthorIsSelf: authorIsSelf,
			Repo: curr.Repo, PRNumber: curr.Number,
			IsDraft: curr.IsDraft, HeadSHA: curr.HeadSHA, Labels: curr.Labels,
		})
	}

	// --- Review requests ---------------------------------------------------
	// Fire when the session user appears in the request list — directly
	// (username) or via any of their teams (org/slug). Transition is "was
	// not matched before, is matched now" so repeated team-level requests
	// don't re-fire across polls where nothing changed.
	//
	// Suppress entirely when the PR is self-authored: GitHub forbids
	// requesting yourself directly, so the only way this fires on your own
	// PR is via a team you're on (CODEOWNERS auto-assigning your team to
	// paths you own). That's not an "ask" — it's a reviewer-pool artifact
	// — and surfacing it as a task pollutes the queue. The default
	// review_requested rule is match-all, so we can't defer the filtering
	// to predicates without forcing every user to customize it.
	if !authorIsSelf {
		prevMatched := matchesAny(prev.ReviewRequests, username, userTeams)
		currMatched := matchesAny(curr.ReviewRequests, username, userTeams)
		if currMatched && !prevMatched {
			emit(domain.EventGitHubPRReviewRequested, "", events.GitHubPRReviewRequestedMetadata{
				Author: curr.Author, AuthorIsSelf: authorIsSelf,
				Repo: curr.Repo, PRNumber: curr.Number,
				IsDraft: curr.IsDraft, HeadSHA: curr.HeadSHA,
				Labels: curr.Labels, Title: curr.Title,
			})
		}
	}

	// --- Per-review events -------------------------------------------------
	// Emit one event per review state transition, split by review type.
	prevReviews := reviewMap(prev.Reviews)
	currReviews := reviewMap(curr.Reviews)

	for reviewer, currState := range currReviews {
		prevState := prevReviews[reviewer]
		if currState.State == prevState.State {
			continue
		}
		reviewerIsSelf := reviewer == username

		switch currState.State {
		case "CHANGES_REQUESTED":
			emit(domain.EventGitHubPRReviewChangesRequested, "", events.GitHubPRReviewChangesRequestedMetadata{
				Author: curr.Author, AuthorIsSelf: authorIsSelf,
				Reviewer: reviewer, ReviewerIsSelf: reviewerIsSelf,
				ReviewID: 0, Repo: curr.Repo, PRNumber: curr.Number,
				IsDraft: curr.IsDraft, HeadSHA: curr.HeadSHA, Labels: curr.Labels,
			})
		case "APPROVED":
			emit(domain.EventGitHubPRReviewApproved, "", events.GitHubPRReviewApprovedMetadata{
				Author: curr.Author, AuthorIsSelf: authorIsSelf,
				Reviewer: reviewer, ReviewerIsSelf: reviewerIsSelf,
				ReviewID: 0, Repo: curr.Repo, PRNumber: curr.Number,
				IsDraft: curr.IsDraft, HeadSHA: curr.HeadSHA, Labels: curr.Labels,
			})
		case "COMMENTED":
			emit(domain.EventGitHubPRReviewCommented, "", events.GitHubPRReviewCommentedMetadata{
				Author: curr.Author, AuthorIsSelf: authorIsSelf,
				Reviewer: reviewer, ReviewerIsSelf: reviewerIsSelf,
				ReviewID: 0, CommentCount: 0, Repo: curr.Repo, PRNumber: curr.Number,
				IsDraft: curr.IsDraft, HeadSHA: curr.HeadSHA, Labels: curr.Labels,
			})
		case "DISMISSED":
			emit(domain.EventGitHubPRReviewDismissed, "", events.GitHubPRReviewDismissedMetadata{
				Author: curr.Author, AuthorIsSelf: authorIsSelf,
				Reviewer: reviewer, ReviewerIsSelf: reviewerIsSelf,
				ReviewID: 0, Repo: curr.Repo, PRNumber: curr.Number,
				IsDraft: curr.IsDraft, HeadSHA: curr.HeadSHA, Labels: curr.Labels,
			})
		}

		// Also emit review_submitted when session user posted the review.
		if reviewerIsSelf && currState.State != prevState.State {
			emit(domain.EventGitHubPRReviewSubmitted, "", events.GitHubPRReviewSubmittedMetadata{
				Author: curr.Author, AuthorIsSelf: authorIsSelf,
				ReviewerIsSelf: true, Reviewer: username,
				ReviewType: stateToReviewType(currState.State),
				ReviewID:   0, Repo: curr.Repo, PRNumber: curr.Number,
				IsDraft: curr.IsDraft, HeadSHA: curr.HeadSHA, Labels: curr.Labels,
			})
		}
	}

	// --- Label diff --------------------------------------------------------
	// New label → label_added; removed label → label_removed.
	// dedup_key = label name (open-set discriminator).
	prevLabels := toSet(prev.Labels)
	currLabels := toSet(curr.Labels)

	for label := range currLabels {
		if !prevLabels[label] {
			emit(domain.EventGitHubPRLabelAdded, label, events.GitHubPRLabelAddedMetadata{
				Author: curr.Author, AuthorIsSelf: authorIsSelf,
				LabelName: label, Repo: curr.Repo, PRNumber: curr.Number,
				IsDraft: curr.IsDraft, HeadSHA: curr.HeadSHA, Labels: curr.Labels,
			})
		}
	}
	for label := range prevLabels {
		if !currLabels[label] {
			emit(domain.EventGitHubPRLabelRemoved, label, events.GitHubPRLabelRemovedMetadata{
				Author: curr.Author, AuthorIsSelf: authorIsSelf,
				LabelName: label, Repo: curr.Repo, PRNumber: curr.Number,
				IsDraft: curr.IsDraft, HeadSHA: curr.HeadSHA, Labels: curr.Labels,
			})
		}
	}

	return evts
}

// DiffJiraSnapshots compares two Jira issue snapshots and returns per-action
// events. username is needed for assignee_is_self metadata. doneStatuses is
// the configured Done.Members set — any status in it is treated as terminal
// for the purpose of emitting jira:issue:completed.
func DiffJiraSnapshots(prev, curr domain.JiraSnapshot, entityID, username string, doneStatuses []string) []domain.Event {
	terminal := func(s string) bool {
		for _, d := range doneStatuses {
			if d == s {
				return true
			}
		}
		return false
	}
	now := time.Now()
	eid := &entityID
	var evts []domain.Event

	emit := func(eventType, dedupKey string, metadata any) {
		metaJSON, _ := json.Marshal(metadata)
		evts = append(evts, domain.Event{
			EventType:    eventType,
			EntityID:     eid,
			DedupKey:     dedupKey,
			MetadataJSON: string(metaJSON),
			CreatedAt:    now,
		})
	}

	assigneeIsSelf := curr.Assignee != "" && curr.Assignee == username

	if prev.Key == "" {
		// First discovery — emit the matching initial event.
		if terminal(curr.Status) {
			emit(domain.EventJiraIssueCompleted, "", events.JiraIssueCompletedMetadata{
				Assignee: curr.Assignee, AssigneeIsSelf: assigneeIsSelf,
				IssueKey: curr.Key, Project: extractProject(curr.Key),
				IssueType: curr.IssueType, FinalStatus: curr.Status,
			})
		} else if curr.OpenSubtaskCount > 0 {
			// Parent-of-subtasks: suppress assigned/available on first
			// discovery. The ticket is a container, not a work unit — the
			// subtasks hold the atomic work, which will discover separately
			// if they match the poller's JQL. If all subtasks later close,
			// the became_atomic branch below fires the belated discovery.
			// Entity itself is still created by the caller so we keep
			// tracking state changes.
		} else if curr.Assignee != "" {
			emit(domain.EventJiraIssueAssigned, "", events.JiraIssueAssignedMetadata{
				Assignee: curr.Assignee, AssigneeIsSelf: assigneeIsSelf,
				Reporter: "", ReporterIsSelf: false,
				IssueKey: curr.Key, Project: extractProject(curr.Key),
				IssueType: curr.IssueType, Priority: curr.Priority,
				Status: curr.Status, Summary: curr.Summary,
			})
		} else {
			emit(domain.EventJiraIssueAvailable, "", events.JiraIssueAvailableMetadata{
				Reporter: "", ReporterIsSelf: false,
				IssueKey: curr.Key, Project: extractProject(curr.Key),
				IssueType: curr.IssueType, Priority: curr.Priority,
				Status: curr.Status, Summary: curr.Summary,
			})
		}
		return evts
	}

	project := extractProject(curr.Key)

	// Status change — dedup_key = new status name.
	if prev.Status != curr.Status && curr.Status != "" {
		emit(domain.EventJiraIssueStatusChanged, curr.Status, events.JiraIssueStatusChangedMetadata{
			Assignee: curr.Assignee, AssigneeIsSelf: assigneeIsSelf,
			IssueKey: curr.Key, Project: project, IssueType: curr.IssueType,
			OldStatus: prev.Status, NewStatus: curr.Status, Priority: curr.Priority,
		})
		if terminal(curr.Status) {
			emit(domain.EventJiraIssueCompleted, "", events.JiraIssueCompletedMetadata{
				Assignee: curr.Assignee, AssigneeIsSelf: assigneeIsSelf,
				IssueKey: curr.Key, Project: project,
				IssueType: curr.IssueType, FinalStatus: curr.Status,
			})
		}
	}

	// Assignment change. Same subtask gate as first-discovery: if the parent
	// has open subtasks, suppress assigned/available so task creation doesn't
	// sneak in via reassignment after the initial suppression. Without this,
	// a parent tracked-but-not-queued could get reassigned and a task would
	// be created even though the ticket is still a container, not work.
	// The became_atomic branch below handles the belated-discovery path once
	// the decomposition collapses.
	if prev.Assignee != curr.Assignee && curr.OpenSubtaskCount == 0 {
		if curr.Assignee != "" {
			emit(domain.EventJiraIssueAssigned, "", events.JiraIssueAssignedMetadata{
				Assignee: curr.Assignee, AssigneeIsSelf: assigneeIsSelf,
				Reporter: "", ReporterIsSelf: false,
				IssueKey: curr.Key, Project: project,
				IssueType: curr.IssueType, Priority: curr.Priority,
				Status: curr.Status, Summary: curr.Summary,
			})
		} else {
			emit(domain.EventJiraIssueAvailable, "", events.JiraIssueAvailableMetadata{
				Reporter: "", ReporterIsSelf: false,
				IssueKey: curr.Key, Project: project,
				IssueType: curr.IssueType, Priority: curr.Priority,
				Status: curr.Status, Summary: curr.Summary,
			})
		}
	}

	// Priority change — dedup_key = new priority value.
	if prev.Priority != curr.Priority && curr.Priority != "" {
		emit(domain.EventJiraIssuePriorityChanged, curr.Priority, events.JiraIssuePriorityChangedMetadata{
			Assignee: curr.Assignee, AssigneeIsSelf: assigneeIsSelf,
			IssueKey: curr.Key, Project: project,
			OldPriority: prev.Priority, NewPriority: curr.Priority,
		})
	}

	// New comments.
	if curr.CommentCount > prev.CommentCount {
		emit(domain.EventJiraIssueCommented, "", events.JiraIssueCommentedMetadata{
			Assignee: curr.Assignee, AssigneeIsSelf: assigneeIsSelf,
			Commenter: "", CommenterIsSelf: false, CommentID: "",
			IssueKey: curr.Key, Project: project,
		})
	}

	// Became atomic — the last open subtask closed. Belated discovery
	// path: the first-discovery branch suppresses assigned/available when
	// the ticket has open subtasks, so nothing's been created yet. This
	// event runs the same task-creation routing as a fresh assignment.
	// Only fires on the downward transition — if the parent genuinely
	// never had subtasks, this condition is never true.
	if prev.OpenSubtaskCount > 0 && curr.OpenSubtaskCount == 0 && !terminal(curr.Status) {
		emit(domain.EventJiraIssueBecameAtomic, "", events.JiraIssueBecameAtomicMetadata{
			Assignee: curr.Assignee, AssigneeIsSelf: assigneeIsSelf,
			IssueKey: curr.Key, Project: project,
			IssueType: curr.IssueType, Priority: curr.Priority,
			Status: curr.Status, Summary: curr.Summary,
		})
	}

	return evts
}

// --- helpers ---------------------------------------------------------------

func toSet(items []string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, item := range items {
		m[item] = true
	}
	return m
}

// matchesAny reports whether the session user's identities (direct
// username or any team they belong to, in "org/slug" form) overlap with
// the reviewers list.
func matchesAny(reviewers []string, username string, userTeams []string) bool {
	return isReviewerMatch(reviewers, username, userTeams)
}

func reviewMap(reviews []domain.ReviewState) map[string]domain.ReviewState {
	m := make(map[string]domain.ReviewState, len(reviews))
	for _, r := range reviews {
		m[r.Author] = r
	}
	return m
}

func stateToReviewType(state string) string {
	switch state {
	case "APPROVED":
		return "approved"
	case "CHANGES_REQUESTED":
		return "changes_requested"
	case "COMMENTED":
		return "commented"
	case "DISMISSED":
		return "dismissed"
	default:
		return "unknown"
	}
}

// extractProject extracts the project key from a Jira issue key (e.g. "SKY" from "SKY-123").
func extractProject(key string) string {
	for i, c := range key {
		if c == '-' {
			return key[:i]
		}
	}
	return key
}
