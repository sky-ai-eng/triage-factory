package routing

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// One thing holds an event's close: a deliverable on the task awaiting a
// person's verdict, on a pull request still open. The task stays open while
// the person has that to act on: the close is held on the task
// (domain.TaskCloseDeferral), and the sweep below applies it once the person
// has resolved the deliverable, if the close still holds then. The task's own
// event arriving again cancels the hold (TaskStore.Bump): the situation is
// live again.
//
// Work on the task holds nothing. A close applies under a working
// conversation, which goes on, and so does its blueprint. A reassignment
// never holds either (closeRelation.reassigns), and neither does a person's
// own close.
//
// The conversations working on the task are told twice, of two things: when
// the hold begins, that the event resolved the situation and the task stays
// open for the person (Delegator.TellTaskCloseHeld), and when the close
// applies, that the task closed, as any close tells them.

// DefaultCloseDeferralSweepInterval is how often the brain looks at the held
// closes of tasks nothing holds any more.
const DefaultCloseDeferralSweepInterval = 30 * time.Second

// --- the sweep --------------------------------------------------------------

// RunCloseDeferralSweep applies the held closes of tasks nothing holds any
// more, no deliverable awaiting a person, every interval, across every org,
// until ctx is cancelled. A brain worker: exactly one process runs it.
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

// sweepCloseDeferrals runs one pass over every active task holding a close
// that nothing holds any more (TaskStore.ListActiveCloseDeferralsSystem). A
// failure on one is logged and leaves it for the next pass; its siblings
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

// settleCloseDeferral decides one held close the sweep listed, which nothing
// held when it was read. The close applies if the relation still admits it
// against current state, and is dropped if not (CI failing again, a reopened
// pull request). closed reports whether the task closed.
//
// The close goes through ApplyHeldCloseSystem, which applies it only while
// the task still holds this event's close: the list was read before this
// task's turn, and since then the task's own event may have cancelled the
// hold, or a newer closing event replaced it. Either way nothing closes, and
// a replaced hold is the next pass's. A deliverable recorded since keeps the
// hold as it is. What it writes is what an unheld close writes, the audit
// row naming the stored event, and the conversations working on the task
// are told as an unheld close tells them.
func (r *Router) settleCloseDeferral(ctx context.Context, d domain.TaskCloseDeferral) (closed bool, err error) {
	evt, reason, admitted, err := r.readHeldClose(ctx, d)
	if err != nil || evt == nil {
		return false, err
	}
	if !admitted {
		routerLog.InfoContext(ctx, "held close no longer holds; dropped",
			"task_id", d.TaskID, "on_event", evt.EventType)
		return false, r.clearCloseDeferral(ctx, d)
	}
	out, err := r.tasks.ApplyHeldCloseSystem(ctx, d.OrgID, d.TaskID, d.EventID, reason, evt.EventType)
	if err != nil {
		return false, fmt.Errorf("close task: %w", err)
	}
	if !out.Closed {
		return false, nil
	}
	r.spawner.TellTaskClosed(ctx, d.OrgID, d.TaskID, out.WorkingConversationIDs)
	routerLog.InfoContext(ctx, "closed task on its held close", "task_id", d.TaskID, "on_event", evt.EventType, "reason", reason)
	return true, nil
}

// readHeldClose reads the closing event of the held close d and asks its
// relation whether the close still holds against current state
// (deferredCloseReason): admitted, with its close reason. evt is nil for a
// task no longer active, which can close between the sweep's list read and
// this one.
func (r *Router) readHeldClose(ctx context.Context, d domain.TaskCloseDeferral) (evt *domain.Event, reason string, admitted bool, err error) {
	evt, err = r.events.GetSystem(ctx, d.OrgID, d.EventID)
	if err != nil {
		return nil, "", false, fmt.Errorf("read closing event: %w", err)
	}
	if evt == nil {
		return nil, "", false, fmt.Errorf("closing event %s not found", d.EventID)
	}
	task, err := r.tasks.GetSystem(ctx, d.OrgID, d.TaskID)
	if err != nil {
		return nil, "", false, fmt.Errorf("read task: %w", err)
	}
	if task == nil || task.Status == "done" || task.Status == "dismissed" {
		return nil, "", false, nil
	}
	reason, admitted, err = r.deferredCloseReason(ctx, d.OrgID, *evt, *task)
	if err != nil {
		return nil, "", false, err
	}
	return evt, reason, admitted, nil
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
		return rel.reasonFor(evt.EventType, task.EventType), true, nil
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
