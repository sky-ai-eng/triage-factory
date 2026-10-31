package tracker

import (
	"encoding/json"
	"strconv"
	"strings"
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
// reviews is every review submitted since prev, oldest first, with TF's marked
// (githubNewReviews). The snapshot holds only the newest, so the caller reads
// them. A prev without the review marker has none read, and the diff compares
// each reviewer's latest review instead.
//
// comments is every conversation comment made since prev, oldest first, with
// TF's marked (githubNewComments): the snapshot holds only the newest.
//
// repositoryID is the entity's repository registry row, which every event's
// metadata carries for repo_in filters to match.
//
// self is the GitHub account a local-mode poll acts as, the zero account in
// multi mode; it decides whether the PR is the user's own (see the review
// request scan).
//
// The mentions in reviews and comments, and the ones curr's body has that
// prev's did not, emit github:pr:mentioned for each identity the resolver
// knows (prMentions).
//
// Not a pure function for the review-request and mention branches: the
// resolver may consult stores (multi-mode). Every other branch is pure.
func DiffPRSnapshots(prev, curr domain.PRSnapshot, entityID, repositoryID string, self domain.GitHubAccount, resolver ReviewerResolver, reviews []NewReview, comments []NewComment) []domain.Event {
	now := time.Now()
	eid := &entityID
	authorIsSelf := self.Is(curr.Author, curr.AuthorID)
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
				Author: curr.Author, AuthorID: curr.AuthorID,
				Repo: curr.Repo, RepositoryID: repositoryID, PRNumber: curr.Number,
				MergedBy: curr.MergedBy, MergedByID: curr.MergedByID, HeadSHA: curr.HeadSHA, Labels: curr.Labels,
			})
		} else if curr.State == "CLOSED" {
			emitAt(domain.EventGitHubPRClosed, "", curr.ClosedAt, events.GitHubPRClosedMetadata{
				Author: curr.Author, AuthorID: curr.AuthorID,
				Repo: curr.Repo, RepositoryID: repositoryID, PRNumber: curr.Number,
				ClosedBy: curr.ClosedBy, ClosedByID: curr.ClosedByID, Labels: curr.Labels,
			})
		}
		// For open PRs: no initial event. The next poll will diff against
		// this snapshot and emit real per-action events (CI checks, reviews,
		// review requests, etc.) when first observed.
		return evts
	}

	// A missing fingerprint is an unknown baseline, never a body deletion.
	if prev.BodyHash != "" && curr.BodyHash != "" && prev.BodyHash != curr.BodyHash {
		emitAt(domain.EventGitHubPRBodyUpdated, "", curr.UpdatedAt, events.GitHubPRBodyUpdatedMetadata{
			Author: curr.Author, AuthorID: curr.AuthorID, Repo: curr.Repo, RepositoryID: repositoryID, PRNumber: curr.Number,
			IsDraft: curr.IsDraft, HeadSHA: curr.HeadSHA, Labels: curr.Labels,
			Title: curr.Title, PreviousBodyHash: prev.BodyHash, BodyHash: curr.BodyHash,
		})
	}

	// --- Entity-terminating transitions ------------------------------------

	if !prev.Merged && curr.Merged {
		emitAt(domain.EventGitHubPRMerged, "", curr.MergedAt, events.GitHubPRMergedMetadata{
			Author: curr.Author, AuthorID: curr.AuthorID,
			Repo: curr.Repo, RepositoryID: repositoryID, PRNumber: curr.Number,
			MergedBy: curr.MergedBy, MergedByID: curr.MergedByID, HeadSHA: curr.HeadSHA, Labels: curr.Labels,
		})
	}

	if prev.State != "CLOSED" && curr.State == "CLOSED" && !curr.Merged {
		emitAt(domain.EventGitHubPRClosed, "", curr.ClosedAt, events.GitHubPRClosedMetadata{
			Author: curr.Author, AuthorID: curr.AuthorID,
			Repo: curr.Repo, RepositoryID: repositoryID, PRNumber: curr.Number,
			ClosedBy: curr.ClosedBy, ClosedByID: curr.ClosedByID, Labels: curr.Labels,
		})
	}

	// --- Draft → Ready for review ------------------------------------------
	// Source time: ReadyForReviewEvent.createdAt from timelineItems when
	// present. emitAt falls back through updatedAt → detection time when
	// the timeline tail doesn't reach far enough back.

	if prev.IsDraft && !curr.IsDraft {
		emitAt(domain.EventGitHubPRReadyForReview, "", lookupReadyForReviewTime(curr.Timeline), events.GitHubPRReadyForReviewMetadata{
			Author: curr.Author, AuthorID: curr.AuthorID,
			Repo: curr.Repo, RepositoryID: repositoryID, PRNumber: curr.Number,
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
					Author: curr.Author, AuthorID: curr.AuthorID,
					CheckRunID: cr.ID, CheckName: cr.Name, CheckURL: cr.DetailsURL,
					WorkflowRunID: cr.WorkflowRunID,
					HeadSHA:       curr.HeadSHA, Repo: curr.Repo, RepositoryID: repositoryID, PRNumber: curr.Number,
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
					Author: curr.Author, AuthorID: curr.AuthorID,
					CheckRunID: cr.ID, CheckName: cr.Name, Conclusion: cr.Conclusion,
					WorkflowRunID: cr.WorkflowRunID,
					HeadSHA:       curr.HeadSHA, Repo: curr.Repo, RepositoryID: repositoryID, PRNumber: curr.Number,
					IsDraft: curr.IsDraft, Labels: curr.Labels,
				})
			}
		}
	}

	// --- New commits -------------------------------------------------------

	if prev.HeadSHA != "" && curr.HeadSHA != "" && prev.HeadSHA != curr.HeadSHA {
		emitAt(domain.EventGitHubPRNewCommits, "", curr.HeadCommittedAt, events.GitHubPRNewCommitsMetadata{
			Author: curr.Author, AuthorID: curr.AuthorID,
			IsDraft: curr.IsDraft, CommitCount: 0, // count not available in snapshot
			HeadSHA: curr.HeadSHA, PrevHeadSHA: prev.HeadSHA,
			Repo: curr.Repo, RepositoryID: repositoryID, PRNumber: curr.Number, Labels: curr.Labels,
		})
	}

	// --- Merge conflicts ---------------------------------------------------

	if prev.Mergeable != "CONFLICTING" && curr.Mergeable == "CONFLICTING" {
		// pr:conflicts has no honest source time — GitHub computes
		// mergeable asynchronously and exposes no transition timestamp.
		// updatedAt would be a misleading proxy because base-branch
		// advances that flip mergeable don't always bump it.
		emitNoSource(domain.EventGitHubPRConflicts, "", events.GitHubPRConflictsMetadata{
			Author: curr.Author, AuthorID: curr.AuthorID,
			Repo: curr.Repo, RepositoryID: repositoryID, PRNumber: curr.Number,
			IsDraft: curr.IsDraft, HeadSHA: curr.HeadSHA, Labels: curr.Labels,
		})
	}

	// A resolution has no honest source time either, for the same reason.
	if prev.Mergeable == "CONFLICTING" && curr.Mergeable == "MERGEABLE" {
		emitNoSource(domain.EventGitHubPRConflictsResolved, "", events.GitHubPRConflictsResolvedMetadata{
			Author: curr.Author, AuthorID: curr.AuthorID,
			Repo: curr.Repo, RepositoryID: repositoryID, PRNumber: curr.Number,
			IsDraft: curr.IsDraft, HeadSHA: curr.HeadSHA, Labels: curr.Labels,
		})
	}

	// --- Review requests (per-reviewer) ------------------------------------
	// The open-set discriminator is the *requested reviewer*. Scan each
	// reviewer that transitioned in/out of the request list and emit a
	// per-identity event for every TF-known identity, keyed by the
	// reviewer's GitHub id ("user_id:<id>" / "team_id:<id>") or, read
	// without one, its handle ("user:<login>" / "team:<org>/<slug>"). The
	// snapshot diff is the sole re-emit guard: a reviewer present last cycle
	// and this cycle is neither added nor removed, so it doesn't re-fire.
	// Presence is RequestedReviewer.Same: by id when both snapshots carry
	// one, so a renamed user or team is neither added nor removed, and by
	// handle against a snapshot stored before ids were recorded, so the
	// first poll after them finds no change.
	//
	// Suppress entirely when the PR is self-authored: GitHub forbids
	// requesting yourself directly, so the only way this fires on your own
	// PR is via a team you're on (CODEOWNERS auto-assigning your team to
	// paths you own). That's not an "ask" — it's a reviewer-pool artifact —
	// and surfacing it pollutes the queue. (In multi mode username is empty,
	// so authorIsSelf is false and the scan always runs.)
	if !authorIsSelf {
		for _, reviewer := range curr.RequestedReviewers() {
			if prev.HasRequestedReviewer(reviewer) {
				continue // not newly added
			}
			if _, _, known := resolveReviewer(resolver, reviewer); !known {
				continue
			}
			login, team, userID, teamID := requestedIdentityFields(reviewer)
			// Source time: this reviewer's ReviewRequestedEvent.createdAt.
			// Falls back through updatedAt → detection time.
			sourceAt := lookupReviewRequestedTimeFor(curr.Timeline, reviewer.Handle)
			emitAt(domain.EventGitHubPRReviewRequested, reviewerDedupKey(reviewer), sourceAt, events.GitHubPRReviewRequestedMetadata{
				Author: curr.Author, AuthorID: curr.AuthorID,
				Repo: curr.Repo, RepositoryID: repositoryID, PRNumber: curr.Number,
				IsDraft: curr.IsDraft, HeadSHA: curr.HeadSHA,
				Labels: curr.Labels, Title: curr.Title,
				RequestedLogin: login, RequestedTeam: team,
				RequestedUserID: userID, RequestedTeamID: teamID,
			})
		}

		for _, reviewer := range prev.RequestedReviewers() {
			if curr.HasRequestedReviewer(reviewer) {
				continue // still requested
			}
			if _, _, known := resolveReviewer(resolver, reviewer); !known {
				continue
			}
			login, team, userID, teamID := requestedIdentityFields(reviewer)
			emit(domain.EventGitHubPRReviewRequestRemoved, reviewerDedupKey(reviewer), events.GitHubPRReviewRequestRemovedMetadata{
				Author: curr.Author, AuthorID: curr.AuthorID,
				Repo: curr.Repo, RepositoryID: repositoryID, PRNumber: curr.Number,
				IsDraft: curr.IsDraft, HeadSHA: curr.HeadSHA,
				Labels: curr.Labels, Title: curr.Title,
				RequestedLogin: login, RequestedTeam: team,
				RequestedUserID: userID, RequestedTeamID: teamID,
			})
		}
	}

	// --- Review events -----------------------------------------------------
	// emitReview emits the event for one review by its state. paths is nil
	// for a review whose comments were not read.
	emitReview := func(state, reviewer, reviewerID string, reviewID int64, commentCount int, paths []string, submittedAt string) {
		if paths == nil {
			paths = []string{}
		}
		switch state {
		case "CHANGES_REQUESTED":
			emitAt(domain.EventGitHubPRReviewChangesRequested, "", submittedAt, events.GitHubPRReviewChangesRequestedMetadata{
				Author: curr.Author, AuthorID: curr.AuthorID, Reviewer: reviewer, ReviewerID: reviewerID,
				ReviewID: reviewID, CommentCount: commentCount, Paths: paths,
				Repo: curr.Repo, RepositoryID: repositoryID, PRNumber: curr.Number,
				IsDraft: curr.IsDraft, HeadSHA: curr.HeadSHA, Labels: curr.Labels,
			})
		case "APPROVED":
			emitAt(domain.EventGitHubPRReviewApproved, "", submittedAt, events.GitHubPRReviewApprovedMetadata{
				Author: curr.Author, AuthorID: curr.AuthorID, Reviewer: reviewer, ReviewerID: reviewerID,
				ReviewID: reviewID, CommentCount: commentCount, Paths: paths,
				Repo: curr.Repo, RepositoryID: repositoryID, PRNumber: curr.Number,
				IsDraft: curr.IsDraft, HeadSHA: curr.HeadSHA, Labels: curr.Labels,
			})
		case "COMMENTED":
			emitAt(domain.EventGitHubPRReviewCommented, "", submittedAt, events.GitHubPRReviewCommentedMetadata{
				Author: curr.Author, AuthorID: curr.AuthorID, Reviewer: reviewer, ReviewerID: reviewerID,
				ReviewID: reviewID, CommentCount: commentCount, Paths: paths,
				Repo: curr.Repo, RepositoryID: repositoryID, PRNumber: curr.Number,
				IsDraft: curr.IsDraft, HeadSHA: curr.HeadSHA, Labels: curr.Labels,
			})
		case "DISMISSED":
			emitAt(domain.EventGitHubPRReviewDismissed, "", submittedAt, events.GitHubPRReviewDismissedMetadata{
				Author: curr.Author, AuthorID: curr.AuthorID, Reviewer: reviewer, ReviewerID: reviewerID,
				ReviewID: reviewID, CommentCount: commentCount, Paths: paths,
				Repo: curr.Repo, RepositoryID: repositoryID, PRNumber: curr.Number,
				IsDraft: curr.IsDraft, HeadSHA: curr.HeadSHA, Labels: curr.Labels,
			})
		}
	}
	prevReviews := newReviewIndex(prev.Reviews)
	currReviews := newReviewIndex(curr.Reviews)

	if prev.ReviewMarker == nil {
		// A snapshot without the review marker: one event per reviewer whose
		// latest review changed state, by that state.
		for _, currState := range curr.Reviews {
			prevState := prevReviews.prevOf(currReviews, currState)
			if currState.State == prevState.State {
				continue
			}
			emitReview(currState.State, currState.Author, currState.AuthorID, currState.DatabaseID, currState.CommentCount, nil, currState.SubmittedAt)
		}
	} else {
		// One event per review submitted since prev, by its state. TF's own
		// reviews and thread replies emit nothing, nor does a review with no
		// author to attribute it to. A review already dismissed by the time it
		// was read is left to the dismissal check below.
		for _, r := range reviews {
			if r.ByTF || r.Author == "" || r.State == "DISMISSED" {
				continue
			}
			emitReview(r.State, r.Author, r.AuthorID, r.ID, r.CommentCount, r.Paths, r.SubmittedAt)
		}
		// A dismissal changes a review already submitted rather than adding
		// one: a reviewer's latest review turned DISMISSED.
		for _, currState := range curr.Reviews {
			if currState.State != "DISMISSED" || prevReviews.prevOf(currReviews, currState).State == "DISMISSED" {
				continue
			}
			emitReview(currState.State, currState.Author, currState.AuthorID, currState.DatabaseID, currState.CommentCount, nil, currState.SubmittedAt)
		}
	}

	// --- Conversation comments ---------------------------------------------
	// One event for the comments TF did not post, however many, attributed to
	// the newest of them and timed at its creation. A bot's comment counts.
	if newest, ids, ok := commentedBy(comments); ok {
		commentID, _ := strconv.ParseInt(newest.ID, 10, 64)
		emitWithFallback(domain.EventGitHubPRCommented, "", commentedAt(newest, curr.UpdatedAt), events.GitHubPRCommentedMetadata{
			Author: curr.Author, AuthorID: curr.AuthorID, Commenter: newest.Author, CommenterLogins: commentedByLogins(comments),
			CommenterID: newest.AuthorID, CommenterIDs: ids, CommentID: commentID,
			Repo: curr.Repo, RepositoryID: repositoryID, PRNumber: curr.Number,
			IsDraft: curr.IsDraft, HeadSHA: curr.HeadSHA, Labels: curr.Labels,
		}, eid, now, &evts)
	}

	// --- Mentions ----------------------------------------------------------
	// One event per user or team TF knows that was newly mentioned in a
	// comment, a review or the body, keyed and resolved by the handle the
	// text spelled (a mention carries no id) and timed at the mention's
	// comment or review. A body mention has no time of its own and takes the
	// PR's updatedAt.
	for _, m := range prMentions(prev, curr, reviews, comments) {
		appendMentioned(&evts, eid, repositoryID, now, curr, m, resolver)
	}

	// --- Label diff --------------------------------------------------------
	// New label → label_added; removed label → label_removed.
	// dedup_key = label name (open-set discriminator).
	prevLabels := toSet(prev.Labels)
	currLabels := toSet(curr.Labels)

	for label := range currLabels {
		if !prevLabels[label] {
			emitAt(domain.EventGitHubPRLabelAdded, label, lookupLabelTime(curr.Timeline, "labeled", label), events.GitHubPRLabelAddedMetadata{
				Author: curr.Author, AuthorID: curr.AuthorID,
				LabelName: label, Repo: curr.Repo, RepositoryID: repositoryID, PRNumber: curr.Number,
				IsDraft: curr.IsDraft, HeadSHA: curr.HeadSHA, Labels: curr.Labels,
			})
		}
	}
	for label := range prevLabels {
		if !currLabels[label] {
			emitAt(domain.EventGitHubPRLabelRemoved, label, lookupLabelTime(curr.Timeline, "unlabeled", label), events.GitHubPRLabelRemovedMetadata{
				Author: curr.Author, AuthorID: curr.AuthorID,
				LabelName: label, Repo: curr.Repo, RepositoryID: repositoryID, PRNumber: curr.Number,
				IsDraft: curr.IsDraft, HeadSHA: curr.HeadSHA, Labels: curr.Labels,
			})
		}
	}

	return evts
}

// DiffJiraSnapshots compares two Jira issue snapshots and returns per-action
// events. doneStatuses is the configured Done.Members set — any status in
// it is treated as terminal for the purpose of emitting
// jira:issue:completed. A zero prev (no id, no key) is first discovery; a
// stored snapshot captured before issue ids were recorded has a key and is
// not.
//
// A new key — the issue moved to another project, or its project's key was
// renamed — is jira:issue:key_changed, emitted ahead of every other event the
// diff finds, since the others describe the issue under the key it has now.
// The caller has already renamed the entity.
//
// Status comparisons go through JiraStatusRef.SameStatus, so a status renamed
// in Jira is not a transition: the id is unchanged, no status-change event is
// emitted, and the snapshot simply stores the new spelling. status_changed's
// dedup_key is the new status's id (jiraValueDedupKey), so a status renamed
// while a task waits on it does not open a second task the next time the
// issue enters it.
//
// The assignee and the priority are compared the same way, by id with the name
// as the fallback for a snapshot that predates the id (JiraSnapshot.SameAssignee,
// SamePriority). A display name change or a priority renamed in Jira is not a
// transition, and two people who share a display name are two assignees.
// priority_changed's dedup_key is the new priority's id. The metadata carries
// the status, priority and issue type ids beside their names: the ids are what
// predicates match, and the names are what a reader of the event sees.
//
// Labels are compared by name, which is a Jira label's identity: it has no id
// and cannot be renamed. label_added / label_removed are keyed on the name, as
// on GitHub, and are not gated on open subtasks the way assigned / available
// are, because someone added the label on purpose.
//
// The actor-identity fields on emitted metadata
// (Assignee/AssigneeAccountID, Reporter/ReporterAccountID) are copied
// verbatim from the snapshot. Predicate matching against assignee_in /
// reporter_in allowlists runs downstream — there is no per-user derivation
// here. The reporter is not diffed: a reporter change does not change
// whether the issue needs attention, so it emits nothing.
//
// comments is every comment made since prev, oldest first, with TF's marked
// (jiraNewComments). The snapshot holds only a count and the newest comment,
// so the caller reads them.
//
// projects names the registry rows the events carry as jira_project_id and
// old_jira_project_id (JiraEventProjects). The project key every event shows
// is the one the read spelled (curr.ProjectKey, and prev.ProjectKey for
// key_changed's old_project metadata), never one parsed out of an issue key.
func DiffJiraSnapshots(prev, curr domain.JiraSnapshot, entityID string, projects JiraEventProjects, doneStatuses []domain.JiraStatusRef, comments []NewComment) []domain.Event {
	terminal := func(snap domain.JiraSnapshot) bool {
		return domain.ContainsStatus(doneStatuses, snap.StatusRef())
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

	if prev.ID == "" && prev.Key == "" {
		// First discovery — emit the matching initial event.
		if terminal(curr) {
			emit(domain.EventJiraIssueCompleted, "", events.JiraIssueCompletedMetadata{
				Assignee: curr.Assignee, AssigneeAccountID: curr.AssigneeAccountID,
				IssueKey: curr.Key, IssueID: curr.ID, Project: curr.ProjectKey, JiraProjectID: projects.Current,
				IssueType: curr.IssueType, IssueTypeID: curr.IssueTypeID, FinalStatus: curr.Status, FinalStatusID: curr.StatusID,
			})
		} else if curr.OpenSubtaskCount > 0 {
			// Parent-of-subtasks: suppress assigned/available on first
			// discovery. The ticket is a container, not a work unit — the
			// subtasks hold the atomic work, which will discover separately
			// if they match the poller's JQL. If all subtasks later close,
			// the became_atomic branch below fires the belated discovery.
			// Entity itself is still created by the caller so we keep
			// tracking state changes.
		} else if curr.HasAssignee() {
			emit(domain.EventJiraIssueAssigned, "", events.JiraIssueAssignedMetadata{
				Assignee: curr.Assignee, AssigneeAccountID: curr.AssigneeAccountID,
				Reporter: curr.Reporter, ReporterAccountID: curr.ReporterAccountID,
				IssueKey: curr.Key, IssueID: curr.ID, Project: curr.ProjectKey, JiraProjectID: projects.Current,
				IssueType: curr.IssueType, IssueTypeID: curr.IssueTypeID, Priority: curr.Priority, PriorityID: curr.PriorityID,
				Status: curr.Status, StatusID: curr.StatusID, Summary: curr.Summary, Labels: curr.Labels,
			})
		} else {
			emit(domain.EventJiraIssueAvailable, "", events.JiraIssueAvailableMetadata{
				Reporter: curr.Reporter, ReporterAccountID: curr.ReporterAccountID,
				IssueKey: curr.Key, IssueID: curr.ID, Project: curr.ProjectKey, JiraProjectID: projects.Current,
				IssueType: curr.IssueType, IssueTypeID: curr.IssueTypeID, Priority: curr.Priority, PriorityID: curr.PriorityID,
				Status: curr.Status, StatusID: curr.StatusID, Summary: curr.Summary, Labels: curr.Labels,
			})
		}
		return evts
	}

	evts = append(evts, jiraKeyChangedEvents(prev, curr, entityID, projects)...)

	project := curr.ProjectKey
	if prev.BodyHash != "" && curr.BodyHash != "" && prev.BodyHash != curr.BodyHash {
		emit(domain.EventJiraIssueBodyUpdated, "", events.JiraIssueBodyUpdatedMetadata{
			Assignee: curr.Assignee, AssigneeAccountID: curr.AssigneeAccountID,
			IssueKey: curr.Key, IssueID: curr.ID, Project: project, JiraProjectID: projects.Current, IssueType: curr.IssueType, IssueTypeID: curr.IssueTypeID,
			Summary: curr.Summary, Labels: curr.Labels,
			PreviousBodyHash: prev.BodyHash, BodyHash: curr.BodyHash,
		})
	}

	// Status change — dedup_key = the new status's id.
	if !prev.StatusRef().SameStatus(curr.StatusRef()) && curr.Status != "" {
		emit(domain.EventJiraIssueStatusChanged, jiraValueDedupKey(curr.StatusID, curr.Status), events.JiraIssueStatusChangedMetadata{
			Assignee: curr.Assignee, AssigneeAccountID: curr.AssigneeAccountID,
			IssueKey: curr.Key, IssueID: curr.ID, Project: project, JiraProjectID: projects.Current, IssueType: curr.IssueType, IssueTypeID: curr.IssueTypeID,
			OldStatus: prev.Status, OldStatusID: prev.StatusID, NewStatus: curr.Status, NewStatusID: curr.StatusID,
			Priority: curr.Priority, PriorityID: curr.PriorityID,
		})
		if terminal(curr) {
			emit(domain.EventJiraIssueCompleted, "", events.JiraIssueCompletedMetadata{
				Assignee: curr.Assignee, AssigneeAccountID: curr.AssigneeAccountID,
				IssueKey: curr.Key, IssueID: curr.ID, Project: project, JiraProjectID: projects.Current,
				IssueType: curr.IssueType, IssueTypeID: curr.IssueTypeID, FinalStatus: curr.Status, FinalStatusID: curr.StatusID,
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
	if !prev.SameAssignee(curr) && curr.OpenSubtaskCount == 0 {
		if curr.HasAssignee() {
			emit(domain.EventJiraIssueAssigned, "", events.JiraIssueAssignedMetadata{
				Assignee: curr.Assignee, AssigneeAccountID: curr.AssigneeAccountID,
				Reporter: curr.Reporter, ReporterAccountID: curr.ReporterAccountID,
				IssueKey: curr.Key, IssueID: curr.ID, Project: project, JiraProjectID: projects.Current,
				IssueType: curr.IssueType, IssueTypeID: curr.IssueTypeID, Priority: curr.Priority, PriorityID: curr.PriorityID,
				Status: curr.Status, StatusID: curr.StatusID, Summary: curr.Summary, Labels: curr.Labels,
			})
		} else {
			emit(domain.EventJiraIssueAvailable, "", events.JiraIssueAvailableMetadata{
				Reporter: curr.Reporter, ReporterAccountID: curr.ReporterAccountID,
				IssueKey: curr.Key, IssueID: curr.ID, Project: project, JiraProjectID: projects.Current,
				IssueType: curr.IssueType, IssueTypeID: curr.IssueTypeID, Priority: curr.Priority, PriorityID: curr.PriorityID,
				Status: curr.Status, StatusID: curr.StatusID, Summary: curr.Summary, Labels: curr.Labels,
			})
		}
	}

	// Priority change — dedup_key = the new priority's id.
	if !prev.SamePriority(curr) && curr.Priority != "" {
		emit(domain.EventJiraIssuePriorityChanged, jiraValueDedupKey(curr.PriorityID, curr.Priority), events.JiraIssuePriorityChangedMetadata{
			Assignee: curr.Assignee, AssigneeAccountID: curr.AssigneeAccountID,
			IssueKey: curr.Key, IssueID: curr.ID, Project: project, JiraProjectID: projects.Current,
			OldPriority: prev.Priority, OldPriorityID: prev.PriorityID, NewPriority: curr.Priority, NewPriorityID: curr.PriorityID,
		})
	}

	addedLabels, removedLabels := labelChanges(prev.Labels, curr.Labels)
	for _, label := range addedLabels {
		emit(domain.EventJiraIssueLabelAdded, label, jiraLabelMetadata(curr, projects.Current, label))
	}
	for _, label := range removedLabels {
		emit(domain.EventJiraIssueLabelRemoved, label, jiraLabelMetadata(curr, projects.Current, label))
	}

	// New comments: one event for the ones TF did not post, however many,
	// attributed to the newest of them and timed at its creation.
	if newest, authors, ok := commentedBy(comments); ok {
		emitWithFallback(domain.EventJiraIssueCommented, "", commentedAt(newest, curr.UpdatedAt), events.JiraIssueCommentedMetadata{
			Assignee: curr.Assignee, AssigneeAccountID: curr.AssigneeAccountID,
			Commenter: newest.Author, CommenterAccountID: newest.AuthorID, CommenterAccountIDs: authors,
			CommentID: newest.ID, IssueKey: curr.Key, IssueID: curr.ID, Project: project, JiraProjectID: projects.Current,
		}, eid, now, &evts)
	}

	// Became atomic — the last open subtask closed. Belated discovery
	// path: the first-discovery branch suppresses assigned/available when
	// the ticket has open subtasks, so nothing's been created yet. This
	// event runs the same task-creation routing as a fresh assignment.
	// Only fires on the downward transition — if the parent genuinely
	// never had subtasks, this condition is never true.
	if prev.OpenSubtaskCount > 0 && curr.OpenSubtaskCount == 0 && !terminal(curr) {
		emit(domain.EventJiraIssueBecameAtomic, "", events.JiraIssueBecameAtomicMetadata{
			Assignee: curr.Assignee, AssigneeAccountID: curr.AssigneeAccountID,
			Reporter: curr.Reporter, ReporterAccountID: curr.ReporterAccountID,
			IssueKey: curr.Key, IssueID: curr.ID, Project: project, JiraProjectID: projects.Current,
			IssueType: curr.IssueType, IssueTypeID: curr.IssueTypeID, Priority: curr.Priority, PriorityID: curr.PriorityID,
			Status: curr.Status, StatusID: curr.StatusID, Summary: curr.Summary, Labels: curr.Labels,
		})
	}

	return evts
}

// JiraEventProjects is the registry rows a Jira diff's events name. Current is
// the jira_projects row of the project a fresh read says the issue is in,
// which every event carries as jira_project_id. Previous is the row its entity
// referenced before the read, which key_changed carries as
// old_jira_project_id: the same row after a project key rename, another after
// a move.
type JiraEventProjects struct {
	Current, Previous string
}

// jiraLabelMetadata is the metadata of a label event on curr, for label;
// jiraProjectID is the registry row of curr's project.
func jiraLabelMetadata(curr domain.JiraSnapshot, jiraProjectID, label string) events.JiraIssueLabelMetadata {
	return events.JiraIssueLabelMetadata{
		Assignee: curr.Assignee, AssigneeAccountID: curr.AssigneeAccountID,
		Reporter: curr.Reporter, ReporterAccountID: curr.ReporterAccountID,
		IssueKey: curr.Key, IssueID: curr.ID, Project: curr.ProjectKey, JiraProjectID: jiraProjectID,
		IssueType: curr.IssueType, IssueTypeID: curr.IssueTypeID, Priority: curr.Priority, PriorityID: curr.PriorityID,
		Status: curr.Status, StatusID: curr.StatusID, Summary: curr.Summary,
		LabelName: label, Labels: curr.Labels,
	}
}

// jiraValueDedupKey is the dedup_key of a status or priority change: the new
// value's id, which a rename in Jira does not move, or its name when the read
// carried no id.
func jiraValueDedupKey(id, name string) string {
	if id != "" {
		return id
	}
	return name
}

// jiraKeyChangedEvents is key_changed for an issue whose key went from prev's
// to curr's, or nothing when it did not change. Its source time is curr's
// updated, as for every other Jira transition: a move bumps it.
func jiraKeyChangedEvents(prev, curr domain.JiraSnapshot, entityID string, projects JiraEventProjects) []domain.Event {
	if prev.Key == "" || curr.Key == "" || prev.Key == curr.Key {
		return nil
	}
	var evts []domain.Event
	emitWithFallback(domain.EventJiraIssueKeyChanged, "", curr.UpdatedAt, events.JiraIssueKeyChangedMetadata{
		Assignee: curr.Assignee, AssigneeAccountID: curr.AssigneeAccountID,
		IssueKey: curr.Key, IssueID: curr.ID, Project: curr.ProjectKey, JiraProjectID: projects.Current,
		IssueType: curr.IssueType, IssueTypeID: curr.IssueTypeID, Summary: curr.Summary,
		OldIssueKey: prev.Key, OldProject: prev.ProjectKey, OldJiraProjectID: projects.Previous,
	}, &entityID, time.Now(), &evts)
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

// labelChanges is the labels in curr and not in prev, in curr's order, and the
// ones in prev and not in curr, in prev's, each named once.
func labelChanges(prev, curr []string) (added, removed []string) {
	prevSet, currSet := toSet(prev), toSet(curr)
	seen := map[string]bool{}
	for _, l := range curr {
		if !prevSet[l] && !seen[l] {
			seen[l] = true
			added = append(added, l)
		}
	}
	for _, l := range prev {
		if !currSet[l] && !seen[l] {
			seen[l] = true
			removed = append(removed, l)
		}
	}
	return added, removed
}

func toSet(items []string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, item := range items {
		m[item] = true
	}
	return m
}

// reviewIndex is a snapshot's latest review per reviewer, looked up by
// GitHub's id for the reviewer's account and by login.
type reviewIndex struct {
	byLogin map[string]domain.ReviewState
	byID    map[string]domain.ReviewState
}

func newReviewIndex(reviews []domain.ReviewState) reviewIndex {
	ix := reviewIndex{byLogin: make(map[string]domain.ReviewState, len(reviews)), byID: map[string]domain.ReviewState{}}
	for _, r := range reviews {
		ix.byLogin[r.Author] = r
		if r.AuthorID != "" {
			ix.byID[r.AuthorID] = r
		}
	}
	return ix
}

// prevOf is the snapshot's review by the reviewer of r, a review from the
// snapshot curr indexes. The reviewer is found by GitHub's id when r carries
// one and this snapshot holds a review under it, so a renamed reviewer is the
// same reviewer; otherwise by login, unless both reviews carry ids that
// differ, which makes the login another account's. A stored snapshot can key
// a bot's review under its bare slug (GraphQL's spelling), so a bot reviewer
// the snapshot holds no review for falls back to that slug and diffs as the
// same reviewer rather than a new one. It does not fall back when curr also
// holds a review under the bare slug: that reviewer is a person whose login
// matches the bot's slug (an App may be named after its owner's account), and
// the stored review may be theirs.
func (ix reviewIndex) prevOf(curr reviewIndex, r domain.ReviewState) domain.ReviewState {
	if r.AuthorID != "" {
		if p, ok := ix.byID[r.AuthorID]; ok {
			return p
		}
	}
	sameAccount := func(p domain.ReviewState) bool {
		return r.AuthorID == "" || p.AuthorID == "" || p.AuthorID == r.AuthorID
	}
	if p, ok := ix.byLogin[r.Author]; ok {
		if sameAccount(p) {
			return p
		}
		return domain.ReviewState{}
	}
	slug, isBot := strings.CutSuffix(r.Author, domain.GitHubBotLoginSuffix)
	if !isBot {
		return domain.ReviewState{}
	}
	if _, person := curr.byLogin[slug]; person {
		return domain.ReviewState{}
	}
	if p, ok := ix.byLogin[slug]; ok && sameAccount(p) {
		return p
	}
	return domain.ReviewState{}
}
