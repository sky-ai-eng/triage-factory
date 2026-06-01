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
// resolver answers "is this requested reviewer a TF-known identity?" so the
// review-request scan can emit one event per requested user/team that maps to a
// routing target. A nil resolver means no reviewer is TF-known — review-request
// events are suppressed, which is the right degraded behavior.
//
// Not a pure function for the review-request branch: the resolver may consult
// stores (multi-mode). Every other branch is pure.
func DiffPRSnapshots(prev, curr domain.PRSnapshot, entityID, username string, resolver ReviewerResolver) []domain.Event {
	now := time.Now()
	eid := &entityID
	authorIsSelf := curr.Author == username
	var evts []domain.Event

	// emit records an event with no specific source timestamp. Falls back
	// to the entity's PR-level updatedAt when available (covers events
	// like ready_for_review, label_added/removed, review_requested whose
	// triggering action bumps the PR's updatedAt). Without a usable
	// fallback, occurred_at stays zero-valued and chronological consumers
	// degrade to created_at.
	emit := func(eventType, dedupKey string, metadata any) {
		emitWithFallback(eventType, dedupKey, curr.UpdatedAt, metadata, eid, now, &evts)
	}

	// emitAt records an event with a specific source timestamp parsed
	// from the GitHub ISO-8601 string on the snapshot (commit
	// committedAt, check-run completedAt, review submittedAt) or a
	// timeline lookup. Blank or unparseable values degrade to the
	// entity-level updatedAt fallback via emit().
	//
	// Goes through the shared external-time parser so a fractional-
	// seconds shape (RFC3339Nano) doesn't silently miss the way it
	// would with a single-layout time.Parse(time.RFC3339).
	emitAt := func(eventType, dedupKey, sourceAt string, metadata any) {
		t, ok := domain.ParseExternalTime(sourceAt)
		if !ok {
			emit(eventType, dedupKey, metadata)
			return
		}
		metaJSON, _ := json.Marshal(metadata)
		evts = append(evts, domain.Event{
			EventType:    eventType,
			EntityID:     eid,
			DedupKey:     dedupKey,
			MetadataJSON: string(metaJSON),
			OccurredAt:   t,
			CreatedAt:    now,
		})
	}

	// emitNoSource records an event we know has no honest source time
	// (e.g., pr:conflicts, where GitHub computes mergeable async and
	// exposes no transition timestamp). occurred_at stays zero so callers
	// don't mistake the entity's last-edit time for the conflict moment.
	emitNoSource := func(eventType, dedupKey string, metadata any) {
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
			emitAt(domain.EventGitHubPRMerged, "", curr.MergedAt, events.GitHubPRMergedMetadata{
				Author: curr.Author,
				Repo:   curr.Repo, PRNumber: curr.Number,
				MergedBy: "", HeadSHA: curr.HeadSHA, Labels: curr.Labels,
			})
		} else if curr.State == "CLOSED" {
			emitAt(domain.EventGitHubPRClosed, "", curr.ClosedAt, events.GitHubPRClosedMetadata{
				Author: curr.Author,
				Repo:   curr.Repo, PRNumber: curr.Number,
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
		emitAt(domain.EventGitHubPRMerged, "", curr.MergedAt, events.GitHubPRMergedMetadata{
			Author: curr.Author,
			Repo:   curr.Repo, PRNumber: curr.Number,
			MergedBy: "", HeadSHA: curr.HeadSHA, Labels: curr.Labels,
		})
	}

	if prev.State != "CLOSED" && curr.State == "CLOSED" && !curr.Merged {
		emitAt(domain.EventGitHubPRClosed, "", curr.ClosedAt, events.GitHubPRClosedMetadata{
			Author: curr.Author,
			Repo:   curr.Repo, PRNumber: curr.Number,
			ClosedBy: "", Labels: curr.Labels,
		})
	}

	// --- Draft → Ready for review ------------------------------------------
	// Source time: ReadyForReviewEvent.createdAt from timelineItems when
	// present. emitAt falls back through updatedAt → detection time when
	// the timeline tail doesn't reach far enough back.

	if prev.IsDraft && !curr.IsDraft {
		emitAt(domain.EventGitHubPRReadyForReview, "", lookupReadyForReviewTime(curr.Timeline), events.GitHubPRReadyForReviewMetadata{
			Author: curr.Author,
			Repo:   curr.Repo, PRNumber: curr.Number,
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
				emitAt(domain.EventGitHubPRCICheckFailed, "", cr.CompletedAt, events.GitHubPRCICheckFailedMetadata{
					Author:     curr.Author,
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
				emitAt(domain.EventGitHubPRCICheckPassed, "", cr.CompletedAt, events.GitHubPRCICheckPassedMetadata{
					Author:     curr.Author,
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
		emitAt(domain.EventGitHubPRNewCommits, "", curr.HeadCommittedAt, events.GitHubPRNewCommitsMetadata{
			Author:  curr.Author,
			IsDraft: curr.IsDraft, CommitCount: 0, // count not available in snapshot
			HeadSHA: curr.HeadSHA, PrevHeadSHA: prev.HeadSHA,
			Repo: curr.Repo, PRNumber: curr.Number, Labels: curr.Labels,
		})
	}

	// --- Merge conflicts ---------------------------------------------------

	if prev.Mergeable != "CONFLICTING" && curr.Mergeable == "CONFLICTING" {
		// pr:conflicts has no honest source time — GitHub computes
		// mergeable asynchronously and exposes no transition timestamp.
		// updatedAt would be a misleading proxy because base-branch
		// advances that flip mergeable don't always bump it.
		emitNoSource(domain.EventGitHubPRConflicts, "", events.GitHubPRConflictsMetadata{
			Author: curr.Author,
			Repo:   curr.Repo, PRNumber: curr.Number,
			IsDraft: curr.IsDraft, HeadSHA: curr.HeadSHA, Labels: curr.Labels,
		})
	}

	// --- Review requests (per-reviewer) ------------------------------------
	// The open-set discriminator is the *requested reviewer*. Scan each
	// reviewer that transitioned in/out of the request list and emit a
	// per-identity event (dedup_key = "user:<login>" / "team:<org>/<slug>")
	// for every TF-known identity. The snapshot diff is the sole re-emit
	// guard: a reviewer present last cycle and this cycle is neither added
	// nor removed, so it doesn't re-fire.
	//
	// Suppress entirely when the PR is self-authored: GitHub forbids
	// requesting yourself directly, so the only way this fires on your own
	// PR is via a team you're on (CODEOWNERS auto-assigning your team to
	// paths you own). That's not an "ask" — it's a reviewer-pool artifact —
	// and surfacing it pollutes the queue. (In multi mode username is empty,
	// so authorIsSelf is false and the scan always runs.)
	if !authorIsSelf {
		prevReq := toSet(prev.ReviewRequests)
		currReq := toSet(curr.ReviewRequests)

		for _, reviewer := range curr.ReviewRequests {
			if prevReq[reviewer] {
				continue // not newly added
			}
			login, team, known := resolveReviewer(resolver, reviewer)
			if !known {
				continue
			}
			// Source time: this reviewer's ReviewRequestedEvent.createdAt.
			// Falls back through updatedAt → detection time.
			sourceAt := lookupReviewRequestedTimeFor(curr.Timeline, reviewer)
			emitAt(domain.EventGitHubPRReviewRequested, reviewerDedupKey(reviewer), sourceAt, events.GitHubPRReviewRequestedMetadata{
				Author: curr.Author,
				Repo:   curr.Repo, PRNumber: curr.Number,
				IsDraft: curr.IsDraft, HeadSHA: curr.HeadSHA,
				Labels: curr.Labels, Title: curr.Title,
				RequestedLogin: login, RequestedTeam: team,
			})
		}

		for _, reviewer := range prev.ReviewRequests {
			if currReq[reviewer] {
				continue // still requested
			}
			login, team, known := resolveReviewer(resolver, reviewer)
			if !known {
				continue
			}
			emit(domain.EventGitHubPRReviewRequestRemoved, reviewerDedupKey(reviewer), events.GitHubPRReviewRequestRemovedMetadata{
				Author: curr.Author,
				Repo:   curr.Repo, PRNumber: curr.Number,
				IsDraft: curr.IsDraft, HeadSHA: curr.HeadSHA,
				Labels: curr.Labels, Title: curr.Title,
				RequestedLogin: login, RequestedTeam: team,
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

		switch currState.State {
		case "CHANGES_REQUESTED":
			emitAt(domain.EventGitHubPRReviewChangesRequested, "", currState.SubmittedAt, events.GitHubPRReviewChangesRequestedMetadata{
				Author:   curr.Author,
				Reviewer: reviewer,
				ReviewID: 0, Repo: curr.Repo, PRNumber: curr.Number,
				IsDraft: curr.IsDraft, HeadSHA: curr.HeadSHA, Labels: curr.Labels,
			})
		case "APPROVED":
			emitAt(domain.EventGitHubPRReviewApproved, "", currState.SubmittedAt, events.GitHubPRReviewApprovedMetadata{
				Author:   curr.Author,
				Reviewer: reviewer,
				ReviewID: 0, Repo: curr.Repo, PRNumber: curr.Number,
				IsDraft: curr.IsDraft, HeadSHA: curr.HeadSHA, Labels: curr.Labels,
			})
		case "COMMENTED":
			emitAt(domain.EventGitHubPRReviewCommented, "", currState.SubmittedAt, events.GitHubPRReviewCommentedMetadata{
				Author:   curr.Author,
				Reviewer: reviewer,
				ReviewID: 0, CommentCount: 0, Repo: curr.Repo, PRNumber: curr.Number,
				IsDraft: curr.IsDraft, HeadSHA: curr.HeadSHA, Labels: curr.Labels,
			})
		case "DISMISSED":
			emitAt(domain.EventGitHubPRReviewDismissed, "", currState.SubmittedAt, events.GitHubPRReviewDismissedMetadata{
				Author:   curr.Author,
				Reviewer: reviewer,
				ReviewID: 0, Repo: curr.Repo, PRNumber: curr.Number,
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
			emitAt(domain.EventGitHubPRLabelAdded, label, lookupLabelTime(curr.Timeline, "labeled", label), events.GitHubPRLabelAddedMetadata{
				Author:    curr.Author,
				LabelName: label, Repo: curr.Repo, PRNumber: curr.Number,
				IsDraft: curr.IsDraft, HeadSHA: curr.HeadSHA, Labels: curr.Labels,
			})
		}
	}
	for label := range prevLabels {
		if !currLabels[label] {
			emitAt(domain.EventGitHubPRLabelRemoved, label, lookupLabelTime(curr.Timeline, "unlabeled", label), events.GitHubPRLabelRemovedMetadata{
				Author:    curr.Author,
				LabelName: label, Repo: curr.Repo, PRNumber: curr.Number,
				IsDraft: curr.IsDraft, HeadSHA: curr.HeadSHA, Labels: curr.Labels,
			})
		}
	}

	return evts
}

// DiffJiraSnapshots compares two Jira issue snapshots and returns per-action
// events. doneStatuses is the configured Done.Members set — any status in
// it is treated as terminal for the purpose of emitting
// jira:issue:completed.
//
// SKY-270: the actor-identity fields on emitted metadata
// (Assignee/AssigneeAccountID, Reporter/ReporterAccountID,
// Commenter/CommenterAccountID) are copied verbatim from the snapshot.
// Predicate matching against assignee_in / reporter_in / commenter_in
// allowlists runs downstream — there is no per-user derivation here.
// Reporter and commenter account IDs are not yet captured by the
// snapshot (the Jira API client doesn't expose them on the issue or
// comment list), so those fields land empty in metadata; rules that
// scope on reporter_in / commenter_in degrade to "no match" naturally
// until that plumbing arrives.
func DiffJiraSnapshots(prev, curr domain.JiraSnapshot, entityID string, doneStatuses []string) []domain.Event {
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

	// All Jira events emitted from this diff correspond to field changes
	// that bump fields.updated. Use it as the source time so the factory's
	// chain animation orders by the actual change moment rather than when
	// the poller noticed. Falls back to detection time when updated is
	// blank (older snapshots that predate the field).
	emit := func(eventType, dedupKey string, metadata any) {
		emitWithFallback(eventType, dedupKey, curr.UpdatedAt, metadata, eid, now, &evts)
	}

	if prev.Key == "" {
		// First discovery — emit the matching initial event.
		if terminal(curr.Status) {
			emit(domain.EventJiraIssueCompleted, "", events.JiraIssueCompletedMetadata{
				Assignee: curr.Assignee, AssigneeAccountID: curr.AssigneeAccountID,
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
				Assignee: curr.Assignee, AssigneeAccountID: curr.AssigneeAccountID,
				IssueKey: curr.Key, Project: extractProject(curr.Key),
				IssueType: curr.IssueType, Priority: curr.Priority,
				Status: curr.Status, Summary: curr.Summary,
			})
		} else {
			emit(domain.EventJiraIssueAvailable, "", events.JiraIssueAvailableMetadata{
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
			Assignee: curr.Assignee, AssigneeAccountID: curr.AssigneeAccountID,
			IssueKey: curr.Key, Project: project, IssueType: curr.IssueType,
			OldStatus: prev.Status, NewStatus: curr.Status, Priority: curr.Priority,
		})
		if terminal(curr.Status) {
			emit(domain.EventJiraIssueCompleted, "", events.JiraIssueCompletedMetadata{
				Assignee: curr.Assignee, AssigneeAccountID: curr.AssigneeAccountID,
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
				Assignee: curr.Assignee, AssigneeAccountID: curr.AssigneeAccountID,
				IssueKey: curr.Key, Project: project,
				IssueType: curr.IssueType, Priority: curr.Priority,
				Status: curr.Status, Summary: curr.Summary,
			})
		} else {
			emit(domain.EventJiraIssueAvailable, "", events.JiraIssueAvailableMetadata{
				IssueKey: curr.Key, Project: project,
				IssueType: curr.IssueType, Priority: curr.Priority,
				Status: curr.Status, Summary: curr.Summary,
			})
		}
	}

	// Priority change — dedup_key = new priority value.
	if prev.Priority != curr.Priority && curr.Priority != "" {
		emit(domain.EventJiraIssuePriorityChanged, curr.Priority, events.JiraIssuePriorityChangedMetadata{
			Assignee: curr.Assignee, AssigneeAccountID: curr.AssigneeAccountID,
			IssueKey: curr.Key, Project: project,
			OldPriority: prev.Priority, NewPriority: curr.Priority,
		})
	}

	// New comments.
	if curr.CommentCount > prev.CommentCount {
		emit(domain.EventJiraIssueCommented, "", events.JiraIssueCommentedMetadata{
			Assignee: curr.Assignee, AssigneeAccountID: curr.AssigneeAccountID,
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
			Assignee: curr.Assignee, AssigneeAccountID: curr.AssigneeAccountID,
			IssueKey: curr.Key, Project: project,
			IssueType: curr.IssueType, Priority: curr.Priority,
			Status: curr.Status, Summary: curr.Summary,
		})
	}

	return evts
}

// --- helpers ---------------------------------------------------------------

// lookupLabelTime returns the createdAt of the most recent timeline item
// matching (kind, label name). Empty when the timeline tail (last 20
// items) doesn't include the matching event — the caller's emitAt path
// degrades to updatedAt → detection time, which is the right behavior:
// we'd rather be slightly off than wrong, and a label change far enough
// back that it slipped past the 20-item window is rarely the user's
// most-pressing signal.
//
// kind ∈ {"labeled", "unlabeled"}. Walk in reverse because GraphQL's
// timelineItems(last: N) returns chronological-ascending; the most
// recent matching event is the right answer (a label can be added then
// removed then added again — the diff fires because the LATEST state is
// "added," so the latest LabeledEvent is the moment we care about).
func lookupLabelTime(timeline []domain.TimelineEvent, kind, label string) string {
	for i := len(timeline) - 1; i >= 0; i-- {
		t := timeline[i]
		if t.Kind == kind && t.Label == label {
			return t.CreatedAt
		}
	}
	return ""
}

// lookupReviewRequestedTimeFor returns the createdAt of the most recent
// ReviewRequestedEvent naming exactly this reviewer (login, or "org/slug" for
// a team), so the per-reviewer event's timestamp lines up with the request
// that actually triggered it. Empty when the timeline tail doesn't reach the
// matching event; the caller's emitAt degrades to updatedAt → detection time.
func lookupReviewRequestedTimeFor(timeline []domain.TimelineEvent, reviewer string) string {
	for i := len(timeline) - 1; i >= 0; i-- {
		t := timeline[i]
		if t.Kind == "review_requested" && t.Reviewer == reviewer {
			return t.CreatedAt
		}
	}
	return ""
}

// lookupReadyForReviewTime returns the createdAt of the most recent
// ReadyForReviewEvent on the timeline. Typically only one fires per PR
// lifecycle (you mark ready once), but if a PR was converted back to
// draft and then re-readied, the most recent transition is the one
// matching the diff — same walk-in-reverse logic as the label lookup.
func lookupReadyForReviewTime(timeline []domain.TimelineEvent) string {
	for i := len(timeline) - 1; i >= 0; i-- {
		if timeline[i].Kind == "ready_for_review" {
			return timeline[i].CreatedAt
		}
	}
	return ""
}

// emitWithFallback appends an event using fallbackAt as occurred_at when it
// parses, otherwise leaves occurred_at zero so chronological consumers
// degrade to created_at. Shared between the PR and Jira diffs since both
// use a "no specific timestamp on the snapshot, but the entity-level
// updatedAt is a fair approximation" pattern.
//
// fallbackAt may be a GitHub RFC3339 timestamp ("2026-04-25T18:30:00Z")
// or a Jira-flavored one ("2026-04-25T18:30:00.123+0000" — note the
// missing colon in the offset). domain.ParseExternalTime tries the full
// known layout set so a less-common Jira shape doesn't silently drop
// occurred_at and degrade to created_at ordering.
func emitWithFallback(eventType, dedupKey, fallbackAt string, metadata any, entityID *string, now time.Time, evts *[]domain.Event) {
	metaJSON, _ := json.Marshal(metadata)
	evt := domain.Event{
		EventType:    eventType,
		EntityID:     entityID,
		DedupKey:     dedupKey,
		MetadataJSON: string(metaJSON),
		CreatedAt:    now,
	}
	if t, ok := domain.ParseExternalTime(fallbackAt); ok {
		evt.OccurredAt = t
	}
	*evts = append(*evts, evt)
}

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

// extractProject extracts the project key from a Jira issue key (e.g. "SKY" from "SKY-123").
func extractProject(key string) string {
	for i, c := range key {
		if c == '-' {
			return key[:i]
		}
	}
	return key
}
