package routing

import (
	"context"
	"encoding/json"
	"log"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// cancelActiveRunsForTask asks the spawner to abort any non-terminal runs
// on the task. Called before a task transitions to done/dismissed so the
// agent stops work on a task the system has decided is resolved.
//
// Errors are logged and swallowed. "no active run" from the spawner is
// expected when a run races us to natural completion between the DB
// lookup and the cancel call — the run ends up terminal either way and
// the task close will still land. Cancellation itself is fire-and-forget;
// the spawner's handleCancelled writes the cancelled status asynchronously.
func (r *Router) cancelActiveRunsForTask(orgID, taskID string) {
	if r.spawner == nil {
		return
	}
	ids, err := r.agentRuns.ActiveIDsForTaskSystem(context.Background(), orgID, taskID)
	if err != nil {
		log.Printf("[router] active-run lookup for task %s failed: %v", taskID, err)
		return
	}
	for _, id := range ids {
		if err := r.spawner.Cancel(orgID, id, ""); err != nil {
			log.Printf("[router] cancel run %s on close of task %s: %v", id, taskID, err)
		}
	}
}

// closeEntity cascades entity → tasks → runs: enumerate active tasks,
// cancel any in-flight run on each, then flip the entity to closed and
// batch-close its tasks with close_reason="entity_closed".
//
// Cancellation happens before the task close SQL so the spawner stops
// work as promptly as possible. The cancel is async (handleCancelled
// runs off a context done channel) but the task row is authoritative —
// subsequent callers see 'done' immediately, and the run lands on
// 'cancelled' when its goroutine unwinds.
func (r *Router) closeEntity(orgID, entityID string) (int, error) {
	if tasks, err := r.tasks.FindActiveByEntitySystem(context.Background(), orgID, entityID); err != nil {
		// Non-fatal: better to cascade-close the entity than to abort
		// because we couldn't enumerate tasks for cancellation. Any
		// orphaned runs can be cleaned up by the existing startup
		// worktree.Cleanup pass.
		log.Printf("[router] entity close: list active tasks for %s failed: %v", entityID, err)
	} else {
		for _, t := range tasks {
			r.cancelActiveRunsForTask(orgID, t.ID)
		}
	}

	if err := r.entities.CloseSystem(context.Background(), orgID, entityID); err != nil {
		return 0, err
	}
	closed, err := r.tasks.CloseAllForEntitySystem(context.Background(), orgID, entityID, "entity_closed")
	if err != nil {
		return closed, err
	}
	if closed > 0 {
		log.Printf("[lifecycle] entity %s closed → %d tasks cascade-closed", entityID, closed)
	}
	return closed, nil
}

// closeTaskWithAudit closes a task and records the closing event in task_events
// so the full close timeline is reconstructable. All inline close checks use
// this instead of calling dbpkg.CloseTask directly.
//
// Also cancels any in-flight run on the task — task state is the
// authoritative invalidation surface, so runs and queued firings derive
// from it. Without the cancel, an inline close check that closes a task
// mid-run would leave the agent churning on work the system already
// considers resolved.
func (r *Router) closeTaskWithAudit(orgID, taskID, closingEventID, closeReason, closeEventType string) error {
	r.cancelActiveRunsForTask(orgID, taskID)
	if err := r.tasks.CloseSystem(context.Background(), orgID, taskID, closeReason, closeEventType); err != nil {
		return err
	}
	if closingEventID != "" {
		_ = r.tasks.RecordEventSystem(context.Background(), orgID, taskID, closingEventID, "closed")
	}
	return nil
}

func (r *Router) runInlineCloseChecks(orgID string, evt domain.Event, entityID string) bool {
	switch evt.EventType {
	case domain.EventGitHubPRCICheckPassed:
		return r.closeCheckCIPassed(orgID, evt, entityID)
	case domain.EventGitHubPRReviewApproved,
		domain.EventGitHubPRReviewCommented,
		domain.EventGitHubPRReviewDismissed:
		// A submitted review does two things: (a) satisfies the reviewer's
		// own review_requested obligation, and (b) may resolve the author's
		// outstanding changes_requested. Run both — they touch different
		// tasks (the reviewer's request vs the author's changes).
		reviewed := r.closeReviewerRequestOnReview(orgID, evt, entityID)
		resolved := r.closeCheckReviewResolved(orgID, evt, entityID)
		return reviewed || resolved
	case domain.EventGitHubPRReviewChangesRequested:
		// Requesting changes is still a review — the reviewer fulfilled their
		// review_requested obligation. (The changes themselves spawn the
		// author-side task via the normal rule path, not here.)
		return r.closeReviewerRequestOnReview(orgID, evt, entityID)
	case domain.EventGitHubPRReviewRequestRemoved:
		return r.closeCheckReviewRequestRemoved(orgID, evt, entityID)
	case domain.EventJiraIssueAssigned:
		return r.closeCheckJiraReassigned(orgID, evt, entityID)
	}
	return false
}

// closeCheckCIPassed: if no failing check-runs remain on this entity at the
// latest SHA, close active ci_check_failed tasks.
func (r *Router) closeCheckCIPassed(orgID string, evt domain.Event, entityID string) bool {
	// Parse metadata to get head_sha.
	var meta events.GitHubPRCICheckPassedMetadata
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
		return false
	}

	// Query: any active ci_check_failed tasks still open on this entity?
	failedTasks, err := r.tasks.FindActiveByEntityAndTypeSystem(context.Background(), orgID, entityID, domain.EventGitHubPRCICheckFailed)
	if err != nil || len(failedTasks) == 0 {
		return false
	}

	// Check entity snapshot for remaining failures at the current SHA.
	entity, err := r.entities.GetSystem(context.Background(), orgID, entityID)
	if err != nil || entity == nil {
		return false
	}
	var snap domain.PRSnapshot
	if err := json.Unmarshal([]byte(entity.SnapshotJSON), &snap); err != nil {
		return false
	}

	// If any check is still failing at the latest SHA, don't close.
	for _, cr := range snap.CheckRuns {
		if domain.IsFailingConclusion(cr.Conclusion) {
			return false
		}
	}

	// All green — close the failure tasks.
	closed := false
	for _, t := range failedTasks {
		if err := r.closeTaskWithAudit(orgID, t.ID, evt.ID, "auto_closed_by_event", domain.EventGitHubPRCICheckPassed); err != nil {
			log.Printf("[router] failed to close ci_check_failed task %s: %v", t.ID, err)
		} else {
			log.Printf("[router] inline-closed task %s (ci_check_failed → ci_check_passed)", t.ID)
			closed = true
		}
	}
	return closed
}

// closeCheckReviewResolved: if the reviewer's prior state was
// changes_requested and no other reviewer still has outstanding
// changes_requested, close active review_changes_requested tasks.
func (r *Router) closeCheckReviewResolved(orgID string, evt domain.Event, entityID string) bool {
	// We need to know which reviewer just changed state. Parse metadata.
	var reviewer string
	switch evt.EventType {
	case domain.EventGitHubPRReviewApproved:
		var meta events.GitHubPRReviewApprovedMetadata
		if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
			return false
		}
		reviewer = meta.Reviewer
	case domain.EventGitHubPRReviewCommented:
		var meta events.GitHubPRReviewCommentedMetadata
		if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
			return false
		}
		reviewer = meta.Reviewer
	case domain.EventGitHubPRReviewDismissed:
		var meta events.GitHubPRReviewDismissedMetadata
		if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
			return false
		}
		reviewer = meta.Reviewer
	}
	if reviewer == "" {
		return false
	}

	// Check entity snapshot: does this reviewer's prior state include
	// changes_requested, and is no other reviewer still requesting changes?
	entity, err := r.entities.GetSystem(context.Background(), orgID, entityID)
	if err != nil || entity == nil {
		return false
	}
	var snap domain.PRSnapshot
	if err := json.Unmarshal([]byte(entity.SnapshotJSON), &snap); err != nil {
		return false
	}

	anyOutstandingChanges := false
	for _, rs := range snap.Reviews {
		if rs.State == "CHANGES_REQUESTED" && rs.Author != reviewer {
			anyOutstandingChanges = true
			break
		}
	}
	if anyOutstandingChanges {
		return false
	}

	// Close review_changes_requested tasks on this entity.
	tasks, err := r.tasks.FindActiveByEntityAndTypeSystem(context.Background(), orgID, entityID, domain.EventGitHubPRReviewChangesRequested)
	if err != nil {
		return false
	}
	closed := false
	for _, t := range tasks {
		if err := r.closeTaskWithAudit(orgID, t.ID, evt.ID, "auto_closed_by_event", evt.EventType); err != nil {
			log.Printf("[router] failed to close changes_requested task %s: %v", t.ID, err)
		} else {
			log.Printf("[router] inline-closed task %s (review resolved by %s)", t.ID, reviewer)
			closed = true
		}
	}
	return closed
}

// closeReviewerRequestOnReview: a reviewer submitted a review (any type:
// approved / commented / changes_requested / dismissed), so their
// review_requested obligation is satisfied — close that reviewer's per-reviewer
// task (keyed "user:<reviewer>"), and only that one. A review by reviewer A
// must not close reviewer B's task on the same PR.
//
// Driven off the typed review events (which fire for EVERY reviewer in both
// local and multi mode and carry Reviewer), not a self-only "submitted" event:
// the close is per-reviewer by identity, mode-agnostic, and independent of
// whether GitHub happens to drop the reviewer from the requested list (a
// comment-only review may not, so review_request_removed alone wouldn't cover
// it). A team review_requested task (keyed "team:<org>/<slug>") is NOT closed
// here — an individual's review doesn't satisfy a team request; that closes via
// the membership-dismissal review_request_removed (closeCheckReviewRequestRemoved).
//
// The reviewer is always an individual (github teams don't submit reviews), so
// the key is always the user namespace.
func (r *Router) closeReviewerRequestOnReview(orgID string, evt domain.Event, entityID string) bool {
	// All four typed review metadata structs carry a top-level "reviewer".
	var m struct {
		Reviewer string `json:"reviewer"`
	}
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &m); err != nil || m.Reviewer == "" {
		return false
	}
	dedupKey := events.ReviewerDedupKeyUser(m.Reviewer)

	tasks, err := r.tasks.FindActiveByEntityAndTypeSystem(context.Background(), orgID, entityID, domain.EventGitHubPRReviewRequested)
	if err != nil {
		return false
	}
	closed := false
	for _, t := range tasks {
		if t.DedupKey != dedupKey {
			continue // a different reviewer's task — leave it open
		}
		if err := r.closeTaskWithAudit(orgID, t.ID, evt.ID, "auto_closed_by_event", evt.EventType); err != nil {
			log.Printf("[router] failed to close review_requested task %s: %v", t.ID, err)
		} else {
			log.Printf("[router] inline-closed task %s (reviewed by %s via %s)", t.ID, m.Reviewer, evt.EventType)
			closed = true
		}
	}
	return closed
}

// closeCheckReviewRequestRemoved: a requested reviewer was dropped from the
// PR's requested-reviewers list (reviewed or request rescinded). Close the
// review_requested task keyed to that reviewer.
//
// review_requested tasks are now per-reviewer (dedup_key =
// "user:<login>" / "team:<org>/<slug>"), so a removal closes only the one
// task whose dedup_key matches the removal event's — dropping reviewer A must
// not close B's task. A legacy removal carrying no dedup_key closes every
// review_requested task on the entity (the pre-ticket behavior).
func (r *Router) closeCheckReviewRequestRemoved(orgID string, evt domain.Event, entityID string) bool {
	tasks, err := r.tasks.FindActiveByEntityAndTypeSystem(context.Background(), orgID, entityID, domain.EventGitHubPRReviewRequested)
	if err != nil {
		return false
	}
	closed := false
	for _, t := range tasks {
		if evt.DedupKey != "" && t.DedupKey != evt.DedupKey {
			continue // different reviewer's task — leave it open
		}
		if err := r.closeTaskWithAudit(orgID, t.ID, evt.ID, "auto_closed_by_event", domain.EventGitHubPRReviewRequestRemoved); err != nil {
			log.Printf("[router] failed to close review_requested task %s: %v", t.ID, err)
		} else {
			log.Printf("[router] inline-closed task %s (review request removed: %s)", t.ID, evt.DedupKey)
			closed = true
		}
	}
	return closed
}

// closeCheckJiraReassigned: when a Jira issue is reassigned, retire any active
// jira:issue:assigned / jira:issue:available task on the entity that is NOT
// still owned by the new assignee, so the prior owner's task closes and the new
// jira:issue:assigned can mint a fresh task owned by the new assignee's team via
// the owning-team ladder.
//
// Member-aware (the assignee-routing world): the new assignee is resolved to
// its owning team(s) via the same reverse identity lookup the router uses, and
// a task already owned by one of those teams is left open — a re-emit for the
// same member must not close-and-recreate that member's own task (it would lose
// the task's claim / in-flight run). A reassignment to ANOTHER TF member closes
// the prior owner's task (the new event mints the new owner's); a reassignment
// to a non-member closes the member's task and the ladder mints nothing. Full
// cross-team ownership transfer of an in-flight task is out of scope.
//
// A NULL-owned task (assignee bound to two TF users → ambiguous owner) is not
// skipped — it closes on any reassignment, since there's no single owning team
// to match the new assignee against. The "self" display-name fallback below
// keeps the local Server/DC path (no Atlassian accountId) from auto-closing a
// task still assigned to the local user.
func (r *Router) closeCheckJiraReassigned(orgID string, evt domain.Event, entityID string) bool {
	var meta events.JiraIssueAssignedMetadata
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
		return false
	}

	// Resolve the new assignee → owning team(s). The account-id reverse lookup
	// is the member-aware core; on Server/DC issues that carry no accountId it
	// resolves nobody, and the local-user display-name fallback below stands in
	// to avoid closing a task still assigned to the local user.
	newOwnerTeams := map[string]struct{}{}
	for _, tid := range r.assigneeTeams(orgID, evt) {
		newOwnerTeams[tid] = struct{}{}
	}
	if r.users != nil && meta.AssigneeAccountID == "" && meta.Assignee != "" {
		// Identity is host-scoped: resolve the org's Jira host (admin pool —
		// this subscriber carries no claims), then read the local user's
		// binding. An unresolvable host or absent row leaves the name empty,
		// degrading to "don't treat as assigned-to-me" exactly as before.
		var jiraHost string
		if r.orgs != nil {
			if orgSet, serr := r.orgs.GetSettingsSystem(context.Background(), orgID); serr == nil {
				jiraHost = orgSet.JiraBaseURL
			}
		}
		if _, localDisplayName, err := r.users.GetJiraIdentitySystem(context.Background(), runmode.LocalDefaultUserID, jiraHost); err == nil {
			if localDisplayName != "" && strings.EqualFold(meta.Assignee, localDisplayName) {
				return false // display-name fallback — still assigned to the local user
			}
		}
	}

	// Close active assigned/available tasks that the new assignee does not own.
	closed := false
	for _, eventType := range []string{domain.EventJiraIssueAssigned, domain.EventJiraIssueAvailable} {
		tasks, err := r.tasks.FindActiveByEntityAndTypeSystem(context.Background(), orgID, entityID, eventType)
		if err != nil {
			continue
		}
		for _, t := range tasks {
			if owner := teamIDValue(&t); owner != "" {
				if _, ok := newOwnerTeams[owner]; ok {
					continue // still owned by the new assignee — not a reassignment-away
				}
			}
			if err := r.closeTaskWithAudit(orgID, t.ID, evt.ID, "auto_closed_by_event", domain.EventJiraIssueAssigned); err != nil {
				log.Printf("[router] failed to close %s task %s: %v", eventType, t.ID, err)
			} else {
				log.Printf("[router] inline-closed task %s (jira reassigned away)", t.ID)
				closed = true
			}
		}
	}
	return closed
}
