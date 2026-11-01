package routing

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// An event close that finds one of the task's conversations still working
// stops nothing: the close is held on the task (domain.TaskCloseDeferral),
// each working conversation is told that the event resolved the task, and
// the sweep below applies the close once none of the task's conversations is
// working, if it still holds then. Holding rather than stopping is what lets
// a run finish the work that resolved its own task (the push CI then passed
// on, the merge, the move to done), and lets a run someone else's work
// overtook wrap up rather than die mid-step. A deliverable awaiting a
// person's verdict, on a pull request still open, holds the close the same
// way, so closing the task never retires it before the person acts on it.
// The task's own event arriving again cancels the hold (TaskStore.Bump): the
// situation is live again.
//
// The hold does not wait forever on work an event started. That work is told
// to wrap up, and if it is still going once the close has held for the
// sweep's limit, the sweep stops it, then closes the task unless a person's
// work or a deliverable awaiting a person still holds it. Work a person
// started is told what happened without being told to stop, so it is never
// stopped on the limit.

// DefaultCloseDeferralSweepInterval is how often the brain looks at the held
// closes of tasks nothing holds any more, and at the holds that have run out.
const DefaultCloseDeferralSweepInterval = 30 * time.Second

// DefaultCloseHoldTimeout is how long a held close waits on work an event
// started before the sweep stops it: long enough for a slow tool call to
// return, the agent to read the close note, and a turn or two to wrap up.
const DefaultCloseHoldTimeout = 20 * time.Minute

// ParseCloseHoldTimeout interprets the TF_CLOSE_HOLD_TIMEOUT env value.
// Empty → the default. Non-positive or unparseable → the default plus an
// error the caller logs.
func ParseCloseHoldTimeout(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultCloseHoldTimeout, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return DefaultCloseHoldTimeout, fmt.Errorf(
			"invalid TF_CLOSE_HOLD_TIMEOUT %q (want a positive duration like \"20m\"); using default %s",
			raw, DefaultCloseHoldTimeout)
	}
	return d, nil
}

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

// RunCloseDeferralSweep applies the held closes of tasks nothing holds any
// more, no conversation working and no deliverable awaiting a person, and
// expires the holds older than holdTimeout that work an event started still
// holds, every interval, across every org, until ctx is cancelled. A brain
// worker: exactly one process runs it.
func (r *Router) RunCloseDeferralSweep(ctx context.Context, interval, holdTimeout time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		r.sweepCloseDeferrals(ctx, holdTimeout)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// sweepCloseDeferrals runs one pass over every active task holding a close
// that nothing holds any more (TaskStore.ListActiveCloseDeferralsSystem),
// then over every hold older than holdTimeout that work an event started
// still holds (TaskStore.ListExpiredCloseDeferralsSystem). A failure on one
// is logged and leaves it for the next pass; its siblings still settle.
func (r *Router) sweepCloseDeferrals(ctx context.Context, holdTimeout time.Duration) {
	closedIn := map[string]bool{}
	defer func() {
		for orgID := range closedIn {
			r.broadcastTasksUpdated(orgID)
		}
	}()

	deferrals, err := r.tasks.ListActiveCloseDeferralsSystem(ctx)
	if err != nil {
		routerLog.ErrorContext(ctx, "close hold sweep: list held closes failed", "error", err)
	}
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

	expired, err := r.tasks.ListExpiredCloseDeferralsSystem(ctx, holdTimeout)
	if err != nil {
		routerLog.ErrorContext(ctx, "close hold sweep: list expired holds failed", "error", err)
		return
	}
	for _, d := range expired {
		if ctx.Err() != nil {
			return
		}
		closed, err := r.expireCloseDeferral(ctx, d, holdTimeout)
		if err != nil {
			routerLog.ErrorContext(ctx, "close hold sweep: expire failed",
				"task_id", d.TaskID, "event_id", d.EventID, "error", err)
			continue
		}
		if closed {
			closedIn[d.OrgID] = true
		}
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
// a replaced hold is the next pass's. A conversation that started working
// since keeps the hold as it is, so nobody is told twice. The stop, the
// cancel intent, the teardown and the audit row naming the stored event are
// what an unheld close writes.
func (r *Router) settleCloseDeferral(ctx context.Context, d domain.TaskCloseDeferral) (closed bool, err error) {
	evt, reason, holds, err := r.heldCloseStillHolds(ctx, d)
	if err != nil || !holds {
		return false, err
	}
	out, err := r.tasks.ApplyHeldCloseSystem(ctx, d.OrgID, d.TaskID, d.EventID, reason, evt.EventType)
	if err != nil {
		return false, fmt.Errorf("close task: %w", err)
	}
	if !out.Closed {
		return false, nil
	}
	r.finishTaskClose(ctx, d.OrgID, d.TaskID, out.ActiveConversationIDs)
	routerLog.InfoContext(ctx, "closed task on its held close", "task_id", d.TaskID, "on_event", evt.EventType, "reason", reason)
	return true, nil
}

// expireCloseDeferral ends the wait of one held close the sweep listed as
// older than holdTimeout. A close that no longer holds is dropped, as the
// settle drops one, and the work goes on. Otherwise ExpireHeldCloseSystem
// decides again under the task's lock: it calls off the work an event
// started, then closes the task if nothing a person does still holds it,
// through the same post-commit sequence as any close, or leaves the close
// held and has the expired conversations stopped. closed reports whether
// the task closed.
func (r *Router) expireCloseDeferral(ctx context.Context, d domain.TaskCloseDeferral, holdTimeout time.Duration) (closed bool, err error) {
	evt, reason, holds, err := r.heldCloseStillHolds(ctx, d)
	if err != nil || !holds {
		return false, err
	}
	out, err := r.tasks.ExpireHeldCloseSystem(ctx, d.OrgID, d.TaskID, d.EventID, holdTimeout, reason, evt.EventType)
	if err != nil {
		return false, fmt.Errorf("expire held close: %w", err)
	}
	if out.Closed {
		r.finishTaskClose(ctx, d.OrgID, d.TaskID, out.ActiveConversationIDs)
		routerLog.InfoContext(ctx, "held close ran out; closed the task", "task_id", d.TaskID,
			"on_event", evt.EventType, "reason", reason, "hold_timeout", holdTimeout)
		return true, nil
	}
	if len(out.ExpiredConversationIDs) == 0 {
		return false, nil
	}
	for _, id := range out.ExpiredConversationIDs {
		// A blueprint between two steps has a settled step no stop reaches;
		// the expiry already called its blueprint off.
		if err := r.spawner.StopConversationAndCancelBlueprint(d.OrgID, id, "", delegate.StopCauseCloseHoldExpired); err != nil && !errors.Is(err, delegate.ErrNoActiveConversation) {
			routerLog.ErrorContext(ctx, "stop conversation on an expired held close failed", "conversation", id, "task_id", d.TaskID, "error", err)
		}
	}
	routerLog.InfoContext(ctx, "held close ran out; stopped the work an event started, close still held",
		"task_id", d.TaskID, "on_event", evt.EventType, "stopped", len(out.ExpiredConversationIDs), "hold_timeout", holdTimeout)
	return false, nil
}

// heldCloseStillHolds reads the closing event of the held close d and asks
// its relation whether the close still holds against current state
// (deferredCloseReason). holds is false for a task no longer active, which
// can close between the sweep's list read and this one (a close attempt on
// it would still write an audit row naming the event), and for a close that
// no longer holds, which is dropped here with a compare-and-clear on d's
// event.
func (r *Router) heldCloseStillHolds(ctx context.Context, d domain.TaskCloseDeferral) (evt domain.Event, reason string, holds bool, err error) {
	e, err := r.events.GetSystem(ctx, d.OrgID, d.EventID)
	if err != nil {
		return domain.Event{}, "", false, fmt.Errorf("read closing event: %w", err)
	}
	if e == nil {
		return domain.Event{}, "", false, fmt.Errorf("closing event %s not found", d.EventID)
	}
	task, err := r.tasks.GetSystem(ctx, d.OrgID, d.TaskID)
	if err != nil {
		return domain.Event{}, "", false, fmt.Errorf("read task: %w", err)
	}
	if task == nil || task.Status == "done" || task.Status == "dismissed" {
		return domain.Event{}, "", false, nil
	}
	reason, admitted, err := r.deferredCloseReason(ctx, d.OrgID, *e, *task)
	if err != nil {
		return domain.Event{}, "", false, err
	}
	if !admitted {
		routerLog.InfoContext(ctx, "held close no longer holds; dropped",
			"task_id", task.ID, "on_event", e.EventType)
		return domain.Event{}, "", false, r.clearCloseDeferral(ctx, d)
	}
	return *e, reason, true, nil
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
