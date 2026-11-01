package routing

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// An event close that finds one of the task's conversations still working
// stops nothing: the close is held on the task (domain.TaskCloseDeferral),
// each working conversation is told that the event resolved the task, and
// the sweep below applies the close once none of the task's conversations is
// working, if it still holds then. Holding rather than stopping is what lets
// a run finish the work that resolved its own task (the push CI then passed
// on, the merge, the move to done), and lets a run someone else's work
// overtook wrap up rather than die mid-step. The task's own event arriving
// again cancels the hold (TaskStore.Bump): the situation is live again.

// DefaultCloseDeferralSweepInterval is how often the brain looks at the held
// closes for tasks nothing is working on any more.
const DefaultCloseDeferralSweepInterval = 30 * time.Second

// tellTaskClosing tells each of the task's working conversations that evt
// resolved it, delivered where the conversation's runtime reads it without
// starting a turn. Best effort, after the hold committed: a lost note costs
// the conversation the news, never the close, which the sweep applies once
// the conversation finishes whether or not it was told.
func (r *Router) tellTaskClosing(ctx context.Context, orgID string, evt domain.Event, taskID string, conversationIDs []string) {
	for _, id := range conversationIDs {
		if !r.spawner.TellTaskClosing(ctx, orgID, id, evt) {
			routerLog.WarnContext(ctx, "close held, but the closing note was not staged", "task_id", taskID, "conversation", id, "on_event", evt.EventType)
		}
	}
}

// --- the sweep --------------------------------------------------------------

// RunCloseDeferralSweep applies the held closes of tasks nothing is working
// on any more, every interval, across every org, until ctx is cancelled. A
// brain worker: exactly one process runs it.
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

// sweepCloseDeferrals runs one pass over every active task holding a close.
// A failure on one is logged and leaves it for the next pass; its siblings
// still settle.
func (r *Router) sweepCloseDeferrals(ctx context.Context) {
	deferrals, err := r.tasks.ListActiveCloseDeferralsSystem(ctx)
	if err != nil {
		routerLog.ErrorContext(ctx, "close hold sweep: list held closes failed", "error", err)
		return
	}
	closedIn := map[string]bool{}
	for _, d := range deferrals {
		if ctx.Err() != nil {
			return
		}
		closed, err := r.settleCloseDeferral(ctx, d)
		if err != nil {
			routerLog.ErrorContext(ctx, "close hold sweep: settle failed",
				"task_id", d.TaskID, "event_id", d.EventID, "error", err)
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

// settleCloseDeferral decides one held close. While a conversation on the
// task is working it is left for the next pass. Otherwise the close applies
// if the relation still admits it against current state, and is dropped if
// not (CI failing again, a reopened pull request). A typed relation's close
// is dropped too while one of the task's deliverables awaits a person's
// verdict, as it is when nothing held it (closeOrHoldTaskWithAudit): the
// entity is still open, so the person can still act on it, and resolving
// the last one closes the task. closed reports whether the task closed.
//
// The close goes through ApplyHeldCloseSystem, which applies it only while
// the task still holds this event's close: the list was read before this
// task's turn, and since then the task's own event may have cancelled the
// hold, or a newer closing event replaced it. Either way nothing closes, and
// a replaced hold is the next pass's. A conversation that started working
// since keeps the hold as it is, so nobody is told twice. The stop, the
// cancel intent, the teardown and the audit row naming the stored event are
// what an unheld close writes.
func (r *Router) settleCloseDeferral(ctx context.Context, d domain.TaskCloseDeferral) (closed bool, err error) {
	working, err := r.conversations.HasWorkingConversationForTaskSystem(ctx, d.OrgID, d.TaskID)
	if err != nil {
		return false, fmt.Errorf("read working conversations: %w", err)
	}
	if working {
		return false, nil
	}
	evt, err := r.events.GetSystem(ctx, d.OrgID, d.EventID)
	if err != nil {
		return false, fmt.Errorf("read closing event: %w", err)
	}
	if evt == nil {
		return false, fmt.Errorf("closing event %s not found", d.EventID)
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
		routerLog.InfoContext(ctx, "held close no longer holds; dropped",
			"task_id", task.ID, "on_event", evt.EventType)
		return false, r.clearCloseDeferral(ctx, d)
	}
	typed := terminatingRelationFor(evt.EventType) == nil
	out, err := r.tasks.ApplyHeldCloseSystem(ctx, d.OrgID, task.ID, d.EventID, reason, evt.EventType, typed)
	if err != nil {
		return false, fmt.Errorf("close task: %w", err)
	}
	if out.AwaitingPerson {
		routerLog.InfoContext(ctx, "held close dropped: a deliverable on the task awaits a person",
			"task_id", task.ID, "on_event", evt.EventType)
	}
	if !out.Closed {
		return false, nil
	}
	r.finishTaskClose(ctx, d.OrgID, task.ID, out.ActiveConversationIDs)
	routerLog.InfoContext(ctx, "closed task on its held close", "task_id", task.ID, "on_event", evt.EventType, "reason", reason)
	return true, nil
}

// deferredCloseReason finds the relation that held the close of task on evt
// and asks it whether the close still holds: a terminating relation while
// the task's entity is still closed (a pull request or issue reopened since
// is live work again); a typed one when its prepare and keep both admit the
// task. admitted=false with no relation found too — the close it owed is not
// one this build declares.
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
		if rel.terminatesEntity || !slices.Contains(rel.onEvents, evt.EventType) || !slices.Contains(rel.closes, task.EventType) {
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
// hold a newer closing event overwrote since is left for the next pass.
func (r *Router) clearCloseDeferral(ctx context.Context, d domain.TaskCloseDeferral) error {
	if _, err := r.tasks.ClearCloseDeferralSystem(ctx, d.OrgID, d.TaskID, d.EventID); err != nil {
		return fmt.Errorf("clear held close: %w", err)
	}
	return nil
}
