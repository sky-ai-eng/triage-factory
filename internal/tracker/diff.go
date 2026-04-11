package tracker

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/sky-ai-eng/todo-triage/internal/domain"
)

// DiffPRSnapshots compares two PR snapshots and returns events for every detected transition.
// A zero-value prev (Number == 0) indicates first-seen and emits initial events.
// username is the authenticated user, used to classify first-seen events. Pure function — no IO.
func DiffPRSnapshots(prev, curr domain.PRSnapshot, sourceID, username string) []domain.Event {
	now := time.Now()
	var events []domain.Event

	emit := func(eventType string, meta map[string]string) {
		events = append(events, domain.Event{
			EventType: eventType,
			SourceID:  sourceID,
			Metadata:  mustJSON(meta),
			CreatedAt: now,
		})
	}

	if prev.Number == 0 {
		// First snapshot — emit the initial event based on current state
		return initialPREvents(curr, sourceID, username, now)
	}

	// --- State transitions ---

	// Merged
	if !prev.Merged && curr.Merged {
		emit(domain.EventGitHubPRMerged, map[string]string{})
	}

	// Closed without merging
	if prev.State != "CLOSED" && curr.State == "CLOSED" && !curr.Merged {
		emit(domain.EventGitHubPRClosed, map[string]string{})
	}

	// Draft → Ready for review
	if prev.IsDraft && !curr.IsDraft {
		emit(domain.EventGitHubPRReadyForReview, map[string]string{})
	}

	// --- CI transitions ---
	//
	// Identity for CI failures is the check-run ID (monotonic per execution),
	// not the aggregate rollup. This fixes two scenarios the old scalar
	// transition check missed:
	//
	//   B: FAILURE → fix pushed → poller sees FAILURE again before seeing
	//      PENDING. Old code: prev == curr == FAILURE, no fire. New code:
	//      the new execution has a new ID not in prev, fires.
	//
	//   C: check A fails → event fires → fix → A passes, B newly fails.
	//      Old code: aggregate stays FAILURE, no fire. New code: B's new
	//      failing ID isn't in prev, fires.
	//
	// Guard: nil prev.CheckRuns means "unknown prior state" (an old snapshot
	// from before this field existed). Skip the whole CI section in that case
	// to avoid spurious events on the first poll after this field lands. An
	// empty-but-non-nil slice is distinguishable and still gets evaluated
	// (prevByID is empty, any curr failure fires as a new execution — which
	// is correct).
	if prev.CheckRuns != nil {
		prevByID := make(map[int64]domain.CheckRun, len(prev.CheckRuns))
		for _, cr := range prev.CheckRuns {
			prevByID[cr.ID] = cr
		}

		// New or newly-failing check runs. Two cases fire here:
		//   1. A failing CheckRun with an ID we haven't seen before (new
		//      execution — retry or new commit).
		//   2. A failing CheckRun with an ID we *have* seen but whose prior
		//      conclusion wasn't failing (same execution completing as
		//      failed — pending → failure on the same run).
		var newFailing []domain.CheckRun
		for _, cr := range curr.CheckRuns {
			if !domain.IsFailingConclusion(cr.Conclusion) {
				continue
			}
			prevCR, existed := prevByID[cr.ID]
			if existed && domain.IsFailingConclusion(prevCR.Conclusion) {
				continue // already failing last poll, not a new signal
			}
			newFailing = append(newFailing, cr)
		}
		if len(newFailing) > 0 {
			primary := newFailing[0]
			meta := map[string]string{
				"count":                fmt.Sprintf("%d", len(newFailing)),
				"primary_check_run_id": fmt.Sprintf("%d", primary.ID),
				"primary_check_name":   primary.Name,
				"primary_conclusion":   primary.Conclusion,
				"primary_details_url":  primary.DetailsURL,
			}
			if primary.WorkflowRunID != 0 {
				meta["primary_workflow_run_id"] = fmt.Sprintf("%d", primary.WorkflowRunID)
			}
			// Full list as JSON so consumers that care about every failing
			// check (auto-delegation prompts, task memory) can parse it.
			if blob, err := json.Marshal(newFailing); err == nil {
				meta["failing_checks"] = string(blob)
			}
			emit(domain.EventGitHubPRCIFailed, meta)
		}

		// Aggregate pass transition. Preserves the old scalar-era semantics
		// of EventGitHubPRCIPassed: "the rollup just became all-green." Stays
		// an aggregate signal rather than per-check because users care about
		// "my PR is ready" not "check #3 passed."
		prevStatus := domain.CIStatusFromCheckRuns(prev.CheckRuns)
		currStatus := domain.CIStatusFromCheckRuns(curr.CheckRuns)
		if currStatus == "success" && prevStatus != "success" {
			emit(domain.EventGitHubPRCIPassed, map[string]string{
				"prev": prevStatus, "new": currStatus,
			})
		}
	}

	// --- New commits (head SHA changed) ---
	// An empty prev.HeadSHA means "unknown prior state" (first poll after the
	// field was added, or a refresh that failed to surface it). Don't fire a
	// spurious event in either case — only emit when both sides are known and
	// genuinely differ.
	if prev.HeadSHA != "" && curr.HeadSHA != "" && prev.HeadSHA != curr.HeadSHA {
		emit(domain.EventGitHubPRNewCommits, map[string]string{
			"prev": prev.HeadSHA, "new": curr.HeadSHA,
		})
	}

	// --- Mergeable state (conflicts) ---
	if prev.Mergeable != "CONFLICTING" && curr.Mergeable == "CONFLICTING" {
		emit(domain.EventGitHubPRConflicts, map[string]string{
			"prev": prev.Mergeable, "new": curr.Mergeable,
		})
	}

	// --- Review requests ---
	// Only fire when the authenticated user is the one being requested.
	prevRR := toSet(prev.ReviewRequests)
	currRR := toSet(curr.ReviewRequests)
	if username != "" && currRR[username] && !prevRR[username] {
		emit(domain.EventGitHubPRReviewRequested, map[string]string{
			"requested_reviewer": username,
		})
	}

	// --- Review state changes (only on PRs you authored) ---
	if curr.Author == username {
		prevReviews := reviewMap(prev.Reviews)
		currReviews := reviewMap(curr.Reviews)
		for author, currState := range currReviews {
			prevState := prevReviews[author]
			if currState.State == prevState.State {
				continue // no change for this reviewer
			}

			switch currState.State {
			case "APPROVED":
				emit(domain.EventGitHubPRApproved, map[string]string{
					"reviewer": author, "prev_state": prevState.State,
				})
			case "CHANGES_REQUESTED":
				emit(domain.EventGitHubPRChangesReq, map[string]string{
					"reviewer": author, "prev_state": prevState.State,
				})
			case "COMMENTED", "DISMISSED":
				emit(domain.EventGitHubPRReviewReceived, map[string]string{
					"reviewer": author, "state": currState.State,
				})
			}
		}
	}

	// --- Mentions (approximated by comment count increase) ---
	// We can't detect @mentions without reading comment bodies, but a new comment
	// on a PR where you're involved is worth surfacing. The poller's search query
	// for mentions:{user} handles actual @mention detection at the discovery layer.

	return events
}

// DiffJiraSnapshots compares two Jira issue snapshots and returns events.
func DiffJiraSnapshots(prev, curr domain.JiraSnapshot, sourceID string) []domain.Event {
	now := time.Now()
	var events []domain.Event

	emit := func(eventType string, meta map[string]string) {
		events = append(events, domain.Event{
			EventType: eventType,
			SourceID:  sourceID,
			Metadata:  mustJSON(meta),
			CreatedAt: now,
		})
	}

	if prev.Key == "" {
		// First snapshot
		return initialJiraEvents(curr, sourceID, now)
	}

	// Status change
	if prev.Status != curr.Status && curr.Status != "" {
		emit(domain.EventJiraIssueStatusChanged, map[string]string{
			"prev_status": prev.Status, "new_status": curr.Status,
		})
		// Also check for terminal
		if isJiraTerminal(curr.Status) {
			emit(domain.EventJiraIssueCompleted, map[string]string{
				"status": curr.Status,
			})
		}
	}

	// Assignment change
	if prev.Assignee != curr.Assignee {
		if curr.Assignee != "" {
			emit(domain.EventJiraIssueAssigned, map[string]string{
				"prev_assignee": prev.Assignee, "new_assignee": curr.Assignee,
			})
		}
		// If assignee was removed, the issue became available again
		if curr.Assignee == "" && prev.Assignee != "" {
			emit(domain.EventJiraIssueAvailable, map[string]string{
				"prev_assignee": prev.Assignee,
			})
		}
	}

	// Priority change
	if prev.Priority != curr.Priority && curr.Priority != "" {
		emit(domain.EventJiraIssuePriorityChanged, map[string]string{
			"prev_priority": prev.Priority, "new_priority": curr.Priority,
		})
	}

	// New comments
	if curr.CommentCount > prev.CommentCount {
		emit(domain.EventJiraIssueCommented, map[string]string{
			"prev_count": fmt.Sprintf("%d", prev.CommentCount),
			"new_count":  fmt.Sprintf("%d", curr.CommentCount),
		})
	}

	return events
}

// initialPREvents returns the events for a newly-discovered PR.
// username is used to determine the relationship: authored, review requested, or mentioned.
func initialPREvents(snap domain.PRSnapshot, sourceID, username string, now time.Time) []domain.Event {
	var events []domain.Event
	add := func(eventType string) {
		events = append(events, domain.Event{
			EventType: eventType,
			SourceID:  sourceID,
			Metadata:  mustJSON(map[string]string{"reason": "first_seen"}),
			CreatedAt: now,
		})
	}

	if snap.Merged {
		add(domain.EventGitHubPRMerged)
		return events
	}

	if snap.State == "CLOSED" {
		add(domain.EventGitHubPRClosed)
		return events
	}

	// Emit the most specific event for why we discovered this PR
	for _, rr := range snap.ReviewRequests {
		if rr == username {
			add(domain.EventGitHubPRReviewRequested)
			return events
		}
	}

	if snap.Author == username {
		add(domain.EventGitHubPROpened)
	} else {
		add(domain.EventGitHubPRMentioned)
	}

	return events
}

// initialJiraEvents returns the events for a newly-discovered Jira issue.
func initialJiraEvents(snap domain.JiraSnapshot, sourceID string, now time.Time) []domain.Event {
	var events []domain.Event
	add := func(eventType string) {
		events = append(events, domain.Event{
			EventType: eventType,
			SourceID:  sourceID,
			Metadata:  mustJSON(map[string]string{"reason": "first_seen"}),
			CreatedAt: now,
		})
	}

	if isJiraTerminal(snap.Status) {
		add(domain.EventJiraIssueCompleted)
	} else if snap.Assignee != "" {
		add(domain.EventJiraIssueAssigned)
	} else {
		add(domain.EventJiraIssueAvailable)
	}
	return events
}

func isJiraTerminal(status string) bool {
	s := status
	return s == "Done" || s == "Closed" || s == "Resolved"
}

// --- helpers ---

func toSet(items []string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, item := range items {
		m[item] = true
	}
	return m
}

func reviewMap(reviews []domain.ReviewState) map[string]domain.ReviewState {
	m := make(map[string]domain.ReviewState, len(reviews))
	for _, r := range reviews {
		m[r.Author] = r
	}
	return m
}
