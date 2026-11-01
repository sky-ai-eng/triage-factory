package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// A closing event can be the result of the very run working on the task it
// closes: the run pushed the fix CI then passed on, merged its pull request,
// or moved its issue to done. Closing the task then would stop that run
// before it finishes. So when the task's own run caused the event, the close
// is held on the task (domain.TaskCloseDeferral) and applied by the sweep
// below once the run ends. Every other task the event closes closes as it
// always does.
//
// A relation declares whether a run can cause it with a runCauseMatcher. A
// relation with none never defers.

// runCauseMatcher reports whether an action blueprintRunID's agent took
// explains evt, the event about to close one of that run's task. A read
// error is err, and joins the close phase's routing obligation like a
// prepare error does.
type runCauseMatcher func(ctx context.Context, r *Router, orgID string, evt domain.Event, entityID, blueprintRunID string) (bool, error)

// DefaultCloseDeferralSweepInterval is how often the brain looks at the
// deferred closes for runs that have ended.
const DefaultCloseDeferralSweepInterval = 30 * time.Second

// CausedByRunPush matches an event whose metadata head_sha is a commit the
// run's agent pushed (a branch_pushed row's SHA). It matches on the SHA
// alone, with no repository or branch, because a SHA names one commit, and a
// fork's head repository needs no resolving to compare one. An event with no
// head_sha matches nothing.
func CausedByRunPush(ctx context.Context, r *Router, orgID string, evt domain.Event, _ string, blueprintRunID string) (bool, error) {
	var meta struct {
		HeadSHA string `json:"head_sha"`
	}
	if json.Unmarshal([]byte(evt.MetadataJSON), &meta) != nil || meta.HeadSHA == "" {
		return false, nil
	}
	pushes, err := r.externalActions.ListAgentActionsForBlueprintRunSystem(ctx, orgID, blueprintRunID, domain.ActionBranchPushed)
	if err != nil {
		return false, err
	}
	for _, a := range pushes {
		if domain.ParseBranchPushSHA(a.DetailJSON) == meta.HeadSHA {
			return true, nil
		}
	}
	return false, nil
}

// causedByRunActionOnEntity matches an event when the run's agent took the
// action actions names for the event's type on the event's entity, since the
// entity's previous event of that type: a pr_merged row explains
// github:pr:merged, a pr_closed row github:pr:closed. A pull request merges
// once, so a merge has no previous one and any pr_merged row on it counts.
func causedByRunActionOnEntity(actions map[string]string) runCauseMatcher {
	return func(ctx context.Context, r *Router, orgID string, evt domain.Event, entityID, blueprintRunID string) (bool, error) {
		action, ok := actions[evt.EventType]
		if !ok || entityID == "" {
			return false, nil
		}
		rows, err := r.externalActions.ListAgentActionsForBlueprintRunSystem(ctx, orgID, blueprintRunID, action)
		if err != nil {
			return false, err
		}
		rows, err = r.rowsSincePreviousEvent(ctx, orgID, evt, entityID, rows)
		return len(rows) > 0, err
	}
}

// causedByRunTransition matches an issue's completion when the run's newest
// issue_transitioned row on the event's entity, since the entity's previous
// completion, moved it to the status the event says it finished in, ignoring
// case. Only the newest counts: a run that moved the issue to done and then
// back has not caused its completion.
func causedByRunTransition(ctx context.Context, r *Router, orgID string, evt domain.Event, entityID, blueprintRunID string) (bool, error) {
	final := completionFinalStatus(evt)
	if final == "" || entityID == "" {
		return false, nil
	}
	rows, err := r.externalActions.ListAgentActionsForBlueprintRunSystem(ctx, orgID, blueprintRunID, domain.ActionIssueTransitioned)
	if err != nil {
		return false, err
	}
	rows, err = r.rowsSincePreviousEvent(ctx, orgID, evt, entityID, rows)
	if err != nil || len(rows) == 0 {
		return false, err
	}
	// Newest first, by occurred_at then id: the first row is the run's last
	// move of the issue.
	return strings.EqualFold(rows[0].ToState, final), nil
}

// rowsSincePreviousEvent narrows a run's ledger rows, in their order, to
// those on entityID written after the entity's previous event of evt's type,
// or to every row on entityID when the entity has none. A row explains at
// most one closing event of its type on its entity, the first recorded after
// it: once the run closed a pull request and a person reopened it, the
// person's next close is not the run's.
//
// The comparison is between when TF wrote the row and when it recorded the
// previous event, in the poll that observed that close. A row that explains
// evt was written after a poll saw the entity open again, so at least a poll
// cycle after that close, which is more than SQLite's whole-second ledger
// stamps (rounded down) or skew between the database's clock and the
// recording process's can move it. A row written
// just before the previous close could land after it only by skew, and would
// then defer the later close as well; it never stops a run on its own close.
func (r *Router) rowsSincePreviousEvent(ctx context.Context, orgID string, evt domain.Event, entityID string, rows []domain.ExternalAction) ([]domain.ExternalAction, error) {
	var onEntity []domain.ExternalAction
	for _, a := range rows {
		if a.EntityID == entityID {
			onEntity = append(onEntity, a)
		}
	}
	// Most runs did nothing to the entity, so the events read waits for a
	// row it could narrow.
	if len(onEntity) == 0 {
		return nil, nil
	}
	prev, err := r.events.LatestForEntityTypeBeforeSystem(ctx, orgID, entityID, evt.EventType, evt.ID)
	if err != nil {
		return nil, fmt.Errorf("read the previous %s on entity %s: %w", evt.EventType, entityID, err)
	}
	if prev == nil {
		return onEntity, nil
	}
	return slices.DeleteFunc(onEntity, func(a domain.ExternalAction) bool {
		return !a.OccurredAt.After(prev.CreatedAt)
	}), nil
}

// completionFinalStatus is the status name a Jira or Linear completion says
// the issue finished in, or "" for any other event or unreadable metadata.
func completionFinalStatus(evt domain.Event) string {
	switch evt.EventType {
	case domain.EventJiraIssueCompleted:
		var m events.JiraIssueCompletedMetadata
		if json.Unmarshal([]byte(evt.MetadataJSON), &m) == nil {
			return m.FinalStatus
		}
	case domain.EventLinearIssueCompleted:
		var m events.LinearIssueCompletedMetadata
		if json.Unmarshal([]byte(evt.MetadataJSON), &m) == nil {
			return m.FinalStatus
		}
	}
	return ""
}

// causingRun returns the task's running blueprint run when that run caused
// evt, or "" when it did not: the relation declares no matcher, the task has
// no running run, no action of its agent explains the event, or the run is
// stopped. A run a person stopped, or one parked on a stall or a lost
// sandbox, gets no deferral — its task closes now.
//
// The ledger match comes before the stopped check because it is the usual
// "no": most runs on a task did not cause the event closing it, and the
// stopped check reads the run's conversations.
func (r *Router) causingRun(ctx context.Context, orgID string, evt domain.Event, entityID string, rel *closeRelation, taskID string) (string, error) {
	if rel.causedByRun == nil {
		return "", nil
	}
	run, err := r.blueprints.RunningRunForTaskSystem(ctx, orgID, taskID)
	if err != nil {
		return "", fmt.Errorf("read task %s's running blueprint run: %w", taskID, err)
	}
	// A run someone called off is stopped already, whatever its
	// conversation shows while the kill lands.
	if run == nil || run.CancelRequested {
		return "", nil
	}
	caused, err := rel.causedByRun(ctx, r, orgID, evt, entityID, run.ID)
	if err != nil {
		return "", fmt.Errorf("match %s against blueprint run %s: %w", evt.EventType, run.ID, err)
	}
	if !caused {
		return "", nil
	}
	stopped, err := r.newestStepConversationStopped(ctx, orgID, run.ID)
	if err != nil || stopped {
		return "", err
	}
	return run.ID, nil
}

// blueprintRunStopped reports whether a run still marked running has stopped
// working: someone called it off (cancel_requested, which the kill follows),
// or its newest step conversation is.
func (r *Router) blueprintRunStopped(ctx context.Context, orgID string, run *domain.BlueprintRun) (bool, error) {
	if run.CancelRequested {
		return true, nil
	}
	return r.newestStepConversationStopped(ctx, orgID, run.ID)
}

// newestStepConversationStopped reports whether the run's newest step
// conversation (highest blueprint_step_index) is stopped: failed, or parked
// open without its step's conclusion. A run with no conversation yet is not
// stopped.
func (r *Router) newestStepConversationStopped(ctx context.Context, orgID, blueprintRunID string) (bool, error) {
	convs, err := r.blueprints.ConversationsForBlueprintSystem(ctx, orgID, blueprintRunID)
	if err != nil {
		return false, fmt.Errorf("read blueprint run %s's conversations: %w", blueprintRunID, err)
	}
	newest := newestStepConversation(convs)
	if newest == nil {
		return false, nil
	}
	return newest.Status == domain.StatusFailed || (newest.Status == domain.StatusOpen && !newest.Concluded()), nil
}

// newestStepConversation is the conversation at the highest step index, the
// later one of two at the same index (the list is in start order within a
// step), or nil for none.
func newestStepConversation(convs []domain.Conversation) *domain.Conversation {
	var newest *domain.Conversation
	for i := range convs {
		c := &convs[i]
		if newest == nil || stepIndexOf(c) >= stepIndexOf(newest) {
			newest = c
		}
	}
	return newest
}

func stepIndexOf(c *domain.Conversation) int {
	if c.BlueprintStepIndex == nil {
		return -1
	}
	return *c.BlueprintStepIndex
}

// terminatingDeferrals is the set CloseTerminalSystem defers for a
// terminating event: each active task the relation closes whose own running
// run caused the event, mapped to that run. nil when the relation declares no
// matcher.
func (r *Router) terminatingDeferrals(ctx context.Context, orgID string, evt domain.Event, entityID string, rel *closeRelation) (map[string]string, error) {
	if rel.causedByRun == nil {
		return nil, nil
	}
	tasks, err := r.tasks.FindActiveByEntitySystem(ctx, orgID, entityID)
	if err != nil {
		return nil, fmt.Errorf("list active tasks: %w", err)
	}
	var deferTo map[string]string
	for _, t := range tasks {
		if !slices.Contains(rel.closes, t.EventType) {
			continue
		}
		runID, err := r.causingRun(ctx, orgID, evt, entityID, rel, t.ID)
		if err != nil {
			return nil, err
		}
		if runID == "" {
			continue
		}
		if deferTo == nil {
			deferTo = map[string]string{}
		}
		deferTo[t.ID] = runID
	}
	return deferTo, nil
}

// --- the sweep --------------------------------------------------------------

// RunCloseDeferralSweep applies the deferred closes whose runs have ended,
// every interval, across every org, until ctx is cancelled. A brain worker:
// exactly one process runs it.
func (r *Router) RunCloseDeferralSweep(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		r.sweepCloseDeferrals(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// sweepCloseDeferrals runs one pass over every active task holding a
// deferral. A failure on one deferral is logged and leaves it for the next
// pass; its siblings still settle.
func (r *Router) sweepCloseDeferrals(ctx context.Context) {
	deferrals, err := r.tasks.ListActiveCloseDeferralsSystem(ctx)
	if err != nil {
		routerLog.ErrorContext(ctx, "close deferral sweep: list deferrals failed", "error", err)
		return
	}
	closedIn := map[string]bool{}
	for _, d := range deferrals {
		if ctx.Err() != nil {
			return
		}
		closed, err := r.settleCloseDeferral(ctx, d)
		if err != nil {
			routerLog.ErrorContext(ctx, "close deferral sweep: settle failed",
				"task_id", d.TaskID, "event_id", d.EventID, "blueprint_run_id", d.BlueprintRunID, "error", err)
			continue
		}
		if closed {
			closedIn[d.OrgID] = true
		}
	}
	for orgID := range closedIn {
		r.broadcastTasksUpdated(orgID)
	}
}

// settleCloseDeferral decides one deferral by its run's state. A run that
// ended, or is still running but stopped, gets the deferred close; a run
// still working, or between steps, is left for the next pass. closed reports
// whether the task closed.
func (r *Router) settleCloseDeferral(ctx context.Context, d domain.TaskCloseDeferral) (closed bool, err error) {
	run, err := r.blueprints.GetRunSystem(ctx, d.OrgID, d.BlueprintRunID)
	if err != nil {
		return false, fmt.Errorf("read blueprint run: %w", err)
	}
	if run == nil {
		return false, fmt.Errorf("blueprint run %s not found", d.BlueprintRunID)
	}
	switch run.Status {
	case domain.BlueprintRunStatusCompleted:
		return r.applyCloseDeferral(ctx, d, true)
	case domain.BlueprintRunStatusAborted, domain.BlueprintRunStatusFailed, domain.BlueprintRunStatusCancelled:
		return r.applyCloseDeferral(ctx, d, false)
	case domain.BlueprintRunStatusRunning:
		stopped, err := r.blueprintRunStopped(ctx, d.OrgID, run)
		if err != nil || !stopped {
			return false, err
		}
		return r.applyCloseDeferral(ctx, d, false)
	}
	return false, nil
}

// applyCloseDeferral performs the close the stored event owed the task, if
// the relation still admits it against current state: a typed relation runs
// its prepare and keep for this one task, and a terminating one closes it
// with its own reason while its entity is still closed. The close is the
// ordinary one (closeTaskWithAudit), so the stop, the cancel intent, the
// teardown and the audit row naming the stored event are what an undeferred
// close writes. A relation that no longer admits it (CI failing again, a
// reopened pull request) drops the deferral without closing.
//
// runCompleted narrows that to terminating events. A completed run has
// finished its work, so a task still open after it is waiting on a person to
// act on a deliverable. That still works while the entity is live, so a
// typed deferral is dropped. On a closed entity nothing can act on the task
// any more, so the terminating close still applies, with the teardown the
// undeferred close would have run.
func (r *Router) applyCloseDeferral(ctx context.Context, d domain.TaskCloseDeferral, runCompleted bool) (closed bool, err error) {
	evt, err := r.events.GetSystem(ctx, d.OrgID, d.EventID)
	if err != nil {
		return false, fmt.Errorf("read closing event: %w", err)
	}
	if evt == nil {
		return false, fmt.Errorf("closing event %s not found", d.EventID)
	}
	if runCompleted && terminatingRelationFor(evt.EventType) == nil {
		return false, r.clearCloseDeferral(ctx, d)
	}
	task, err := r.tasks.GetSystem(ctx, d.OrgID, d.TaskID)
	if err != nil {
		return false, fmt.Errorf("read task: %w", err)
	}
	// The list read only active tasks, but one can close between that read
	// and this one, and a close attempt on it would still write an audit
	// row naming this event.
	if task == nil || task.Status == "done" || task.Status == "dismissed" {
		return false, nil
	}

	reason, admitted, err := r.deferredCloseReason(ctx, d.OrgID, *evt, *task)
	if err != nil {
		return false, err
	}
	if !admitted {
		routerLog.InfoContext(ctx, "deferred close no longer holds; dropped",
			"task_id", task.ID, "on_event", evt.EventType)
		return false, r.clearCloseDeferral(ctx, d)
	}
	if err := r.closeTaskWithAudit(ctx, d.OrgID, task.ID, evt.ID, reason, evt.EventType); err != nil {
		return false, fmt.Errorf("close task: %w", err)
	}
	routerLog.InfoContext(ctx, "closed task on its deferred close", "task_id", task.ID, "on_event", evt.EventType, "reason", reason)
	// The task is no longer active, so a failed clear is never read again.
	if err := r.clearCloseDeferral(ctx, d); err != nil {
		routerLog.WarnContext(ctx, "close deferral sweep: clear after close failed", "task_id", task.ID, "error", err)
	}
	return true, nil
}

// deferredCloseReason finds the relation that deferred the close of task on
// evt and asks it whether the close still holds: a terminating relation
// while the task's entity is still closed (a pull request or issue reopened
// since is live work again); a typed one when its prepare and keep both admit
// the task. admitted=false with no relation found too — the close it owed is
// not one this build declares.
func (r *Router) deferredCloseReason(ctx context.Context, orgID string, evt domain.Event, task domain.Task) (reason string, admitted bool, err error) {
	if rel := terminatingRelationFor(evt.EventType); rel != nil {
		entity, err := r.entities.GetSystem(ctx, orgID, task.EntityID)
		if err != nil {
			return "", false, fmt.Errorf("read entity %s: %w", task.EntityID, err)
		}
		if entity == nil || entity.State != "closed" {
			return "", false, nil
		}
		return rel.reason(), true, nil
	}
	for ri := range closeRelations {
		rel := &closeRelations[ri]
		if rel.terminatesEntity || rel.causedByRun == nil ||
			!slices.Contains(rel.onEvents, evt.EventType) || !slices.Contains(rel.closes, task.EventType) {
			continue
		}
		var cctx closeContext
		if rel.prepare != nil {
			var ok bool
			if cctx, ok, err = rel.prepare(ctx, r, orgID, evt, task.EntityID); err != nil {
				return "", false, fmt.Errorf("prepare %s close: %w", evt.EventType, err)
			} else if !ok {
				continue
			}
		}
		if rel.keep != nil && !rel.keep(evt, cctx, taskTarget(task)) {
			continue
		}
		return rel.reason(), true, nil
	}
	return "", false, nil
}

// clearCloseDeferral is the compare-and-clear on the event the sweep read: a
// deferral a newer event overwrote since is left for the next pass.
func (r *Router) clearCloseDeferral(ctx context.Context, d domain.TaskCloseDeferral) error {
	if _, err := r.tasks.ClearCloseDeferralSystem(ctx, d.OrgID, d.TaskID, d.EventID); err != nil {
		return fmt.Errorf("clear deferral: %w", err)
	}
	return nil
}
