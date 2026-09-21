package routing

import (
	"context"
	"errors"
	"fmt"
	"time"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workkinds"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// The firing worker is a FencedReplay consumer of the shared work-item
// contract (internal/db/workitem), over the kind workkinds.PendingFirings
// declares: the per-task queue of auto-delegation intents the router admits
// when a matched trigger cannot fire because its task is busy. Every unit it
// runs is a replay by construction — an expired lease hands the row to the
// next claim — and two domain fences make the replay a no-op: (1) the
// blueprint-run insert is fenced on blueprint_runs (triggering_event_id,
// trigger_id), so a replay of a firing whose run committed returns
// delegate.ErrAlreadyFired and the row is skipped with that reason; (2) the
// one-active-run-per-task index refuses a second live run, returning
// delegate.ErrTaskBusy, which becomes a deferral with the attempt refunded.
// A firing's only domain write is that insert, one transaction, so a replay
// either finds it committed or repeats it whole.
//
// The per-task gate is in the claim query: the kind's claim filter admits a
// ready row only while its task holds no live top-level conversation and no
// blueprint run still marked running. The second half is fence (2) stated as
// a filter: a run is marked terminal only after its last conversation is, and
// a row claimed between those two writes would only be refused. So a fire
// closes the gate for the task's remaining rows at the next claim with no
// write of the worker's, a row for the same task already in the batch meets
// ErrTaskBusy and defers at no cost, and FIFO within a task is the claim's
// own ORDER BY id.
//
// Nothing here resets, sweeps or renews on a timer. A row whose holder died
// is reclaimed by the first claim after its lease expires, and the reclaim
// is logged because it is the one signal that a unit was interrupted. A
// firing that keeps failing carries backoff and parks after its budget for a
// person to look at.

// firingClaimBatch is how many rows one claim takes. Ten rather than one
// because the kind's fairness interleaves at batch granularity: a batch from
// one org leaves it with ten leased rows, so the next pick prefers another.
const firingClaimBatch = 10

// DefaultFiringScanInterval is the floor scan for RunFiringQueue, exported so
// main can tune it and tests can drive the loop fast. The scan is the
// correctness backstop: a dropped wake, a conversation ending on a process
// that has no waker (a multi-mode executor), or a requeued row's backoff
// ripening all wait at most one tick.
const DefaultFiringScanInterval = 3 * time.Second

// pendingFiringsKind is the kind the worker's timing comes from. The dialect
// is irrelevant to the policy, which is all the worker reads.
var pendingFiringsKind = workkinds.PendingFirings(workitem.SQLite)

// WakeFirings nudges the firing worker: a conversation reached a terminal,
// so a task's gate may have opened. Non-blocking, and best-effort: the wake
// channel holds one signal, and a dropped wake only delays the drain to the
// next scan tick.
func (r *Router) WakeFirings() {
	select {
	case r.firingWake <- struct{}{}:
	default:
	}
}

// RunFiringQueue is the firing worker's loop: an initial drain, then a drain
// on every wake and every scan tick. Single worker, claiming in batches
// interleaved across orgs — exactly one process may run this at a time,
// which is what makes it a brain component rather than a replica-safe one.
// Returns when ctx is cancelled. A nil PendingFiringsStore makes this a
// logged no-op rather than a panicking goroutine.
func (r *Router) RunFiringQueue(ctx context.Context, scanInterval time.Duration) {
	if r.firings == nil {
		routerLog.Warn("firing-queue worker not started: no PendingFiringsStore wired")
		return
	}

	scan := time.NewTicker(scanInterval)
	defer scan.Stop()

	fails := claimFailures{name: "firing-queue"}
	drain := func() { fails.observe(r.drainFiringQueue(ctx)) }

	drain() // drain whatever survived the restart

	for {
		select {
		case <-ctx.Done():
			return
		case <-r.firingWake:
			drain()
		case <-scan.C:
			drain()
		}
	}
}

// drainFiringQueue claims and processes rows in batches until the queue is
// drained or ctx is cancelled. Returns the claim error if one occurs so the
// caller can track consecutive failures and escalate; every other ending —
// drained, deferred rows not yet ripe, cancelled — returns nil.
//
// A batch shorter than firingClaimBatch ends the pass: the queue is drained,
// or what remains is deferred behind a busy task or a retry time, and the
// next wake or scan tick owns it. Cancellation is checked between batches
// and nowhere else, as the event worker's is: the claim runs on the
// cancellable ctx, while the rows already claimed run to completion on their
// own detached contexts.
func (r *Router) drainFiringQueue(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil // clean stop, not a claim failure
		}
		executorID, bootEpoch := r.executorIdentity()
		batch, err := r.firings.Claim(ctx, workitem.Owner{ID: executorID, Epoch: bootEpoch}, firingClaimBatch)
		if batch.Cancelled+batch.Parked > 0 {
			routerLog.Info("firing-queue claim settled rows", "cancelled", batch.Cancelled, "parked", batch.Parked)
		}
		for _, cf := range batch.Firings {
			if cf.Receipt.Reclaimed {
				// A reclaim means a unit was interrupted on some process:
				// the previous holder's lease ran out with no terminal write.
				routerLog.Warn("firing-queue reclaimed a row whose lease expired without a terminal write",
					"firing_id", cf.Firing.ID, "task_id", cf.Firing.TaskID, "previous_owner", cf.Receipt.PreviousOwner, "attempt", cf.Receipt.Attempt)
			}
		}
		// Leases committed before a claim error are real, and their rows
		// are this worker's to dispose of before it reports the failure.
		for _, cf := range batch.Firings {
			r.processFiring(ctx, cf)
		}
		if err != nil {
			return err // caller owns logging + consecutive-failure escalation
		}
		if len(batch.Firings) < firingClaimBatch {
			return nil
		}
	}
}

// processFiring runs one claimed firing as one unit: renew the lease,
// validate the firing against the world now, fire it, and write its terminal
// under the receipt's fence. Done means done — the run committed and the row
// records it, or the firing was declined for a definitive reason — and a
// failure is never consumed: the row returns to the queue with a retry time,
// defers at no cost when the task is busy, or parks with a typed reason once
// the budget is spent.
//
// The unit runs on ctx with its cancellation dropped and its values kept,
// under the kind's UnitDeadline, for the reason the event worker gives:
// cancelling mid-unit is a worse stop, not a cheaper one. The renewal at the
// top is the fence check and the point where a cancellation request is
// observed; every later write presents the receipt it returns.
//
// Every terminal write's loser contract is the same: ErrLeaseLost logs at
// info and stops, the blueprint run, if one committed, stands, and the row's
// next holder replays a unit the fence answers with ErrAlreadyFired, so the
// row ends done with skip_reason already_fired and the run still findable
// from blueprint_runs (triggering_event_id, trigger_id). Nothing tears a run
// down to fix a status column. ErrCancelled logs at info; the row is
// settled. Any other error logs at error and stops; the lease expires and
// the row is reclaimed. Nothing retries a terminal write in a loop.
func (r *Router) processFiring(ctx context.Context, cf dbpkg.ClaimedFiring) {
	f := cf.Firing
	unitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pendingFiringsKind.Policy.UnitDeadline)
	defer cancel()

	unitCtx, span := tracer.Start(unitCtx, "firing.fire", trace.WithAttributes(
		attribute.Int64("firing.id", f.ID),
		telemetry.TaskID(f.TaskID),
		attribute.String("trigger.id", f.TriggerID),
	))
	defer span.End()

	receipt, err := r.firings.RenewLease(unitCtx, cf.Receipt)
	switch {
	case errors.Is(err, workitem.ErrLeaseLost):
		span.SetAttributes(telemetry.Outcome("lease_lost"))
		routerLog.InfoContext(unitCtx, "firing-queue: lease lost before firing; the row's next holder owns it",
			"firing_id", f.ID, "task_id", f.TaskID)
		return
	case errors.Is(err, workitem.ErrCancelled):
		span.SetAttributes(telemetry.Outcome("cancelled"))
		routerLog.InfoContext(unitCtx, "firing-queue: row settled cancelled at renewal", "firing_id", f.ID, "task_id", f.TaskID)
		return
	case err != nil:
		// Skipped this pass: the lease expires on its own and the next
		// claim reclaims the row.
		span.SetStatus(codes.Error, "renew lease")
		routerLog.WarnContext(unitCtx, "firing-queue: lease renewal failed; the row is reclaimed after its lease expires",
			"firing_id", f.ID, "task_id", f.TaskID, "error", err)
		return
	}

	// The terminal write gets a context of its own, so a unit that ran out
	// its deadline can still record that it did.
	terminal := func(fn func(termCtx context.Context)) {
		termCtx, cancelTerm := context.WithTimeout(context.WithoutCancel(ctx), terminalWriteTimeout)
		defer cancelTerm()
		fn(termCtx)
	}
	loser := func(termCtx context.Context, verb string, err error) {
		switch {
		case err == nil:
		case errors.Is(err, workitem.ErrLeaseLost):
			span.SetAttributes(telemetry.Outcome("lease_lost"))
			routerLog.InfoContext(termCtx, "firing-queue: lease lost at "+verb+"; the successor's replay is fenced", "firing_id", f.ID, "task_id", f.TaskID)
		case errors.Is(err, workitem.ErrCancelled):
			span.SetAttributes(telemetry.Outcome("cancelled"))
			routerLog.InfoContext(termCtx, "firing-queue: row settled cancelled at "+verb, "firing_id", f.ID, "task_id", f.TaskID)
		default:
			span.SetStatus(codes.Error, verb)
			routerLog.ErrorContext(termCtx, "firing-queue: "+verb+" failed; the row is reclaimed after its lease expires", "firing_id", f.ID, "task_id", f.TaskID, "error", err)
		}
	}
	requeue := func(outcome workitem.Outcome, cause error) {
		terminal(func(termCtx context.Context) {
			parked, err := r.firings.Requeue(termCtx, receipt, outcome, cause)
			if err == nil && parked {
				routerLog.WarnContext(termCtx, "firing-queue: parked row; it will not fire again without an operator",
					"firing_id", f.ID, "task_id", f.TaskID, "trigger", f.TriggerID, "outcome", string(outcome), "attempt", receipt.Attempt, "cause", cause)
			}
			loser(termCtx, "requeue", err)
		})
	}
	skip := func(reason string) {
		span.SetAttributes(telemetry.Outcome("skipped_" + reason))
		routerLog.InfoContext(unitCtx, "firing-queue: skipped firing", "firing_id", f.ID, "task_id", f.TaskID, "trigger", f.TriggerID, "skip_reason", reason)
		terminal(func(termCtx context.Context) {
			loser(termCtx, "mark skipped", r.firings.MarkSkipped(termCtx, receipt, reason))
		})
	}

	defer func() {
		if rec := recover(); rec != nil {
			// The poison-pill safety net: a panic in a unit must not kill
			// the single worker goroutine and freeze the whole queue.
			span.SetStatus(codes.Error, "panic")
			routerLog.ErrorContext(unitCtx, "firing-queue: panic firing",
				"firing_id", f.ID, "task_id", f.TaskID, "attempt", receipt.Attempt, "panic", rec)
			requeue(workitem.OutcomePoisonSuspected, fmt.Errorf("panic: %v", rec))
		}
	}()

	// Validate against live tables, not the firing row, so the worker
	// reflects the world now: a close, a takeover, a disabled trigger or a
	// tripped breaker since the row was admitted is a definitive skip, and a
	// read that failed is a transient requeue.
	task, err := r.tasks.GetSystem(unitCtx, f.OrgID, f.TaskID)
	if err != nil {
		requeue(workitem.OutcomeTransient, fmt.Errorf("task lookup: %w", err))
		return
	}
	if task == nil || task.Status == "done" || task.Status == "dismissed" || task.Status == "snoozed" {
		skip(domain.PendingFiringSkipTaskClosed)
		return
	}
	// The firing fires only if the bot's claim still holds. A user claim or
	// a requeue invalidates the commitment the queued row carried.
	if task.ClaimedByAgentID == "" {
		skip(domain.PendingFiringSkipClaimChanged)
		return
	}
	trigger, err := r.handlers.GetSystem(unitCtx, f.OrgID, f.TriggerID)
	if err != nil {
		requeue(workitem.OutcomeTransient, fmt.Errorf("trigger lookup: %w", err))
		return
	}
	if trigger == nil || trigger.Kind != domain.EventHandlerKindTrigger || !trigger.Enabled {
		skip(domain.PendingFiringSkipTriggerDisabled)
		return
	}
	breakerThreshold := derefIntDefault(trigger.BreakerThreshold, 0)
	failures, err := r.tasks.CountConsecutiveFailedConversationsSystem(unitCtx, f.OrgID, f.EntityID, r.breakerPromptID(unitCtx, f.OrgID, trigger.BlueprintID))
	if err != nil {
		requeue(workitem.OutcomeTransient, fmt.Errorf("breaker query: %w", err))
		return
	}
	if failures >= breakerThreshold {
		skip(domain.PendingFiringSkipBreakerTripped)
		return
	}

	// The actor is the agent that already claimed this task (non-empty by
	// the guard above): the worker re-fires the same bot's commitment, so
	// the new run's frozen actor matches the standing claim. No claim stamp
	// and no owner consolidation ride this insert: both were committed by
	// the admission that queued the firing, and re-imposing either here
	// would fight a user's requeue.
	blueprintRunID, err := r.fireDelegate(unitCtx, f.OrgID, task, *trigger, f.TriggeringEventID, task.ClaimedByAgentID, dbpkg.AgentClaimStamp{}, "")
	switch {
	case err == nil:
	case errors.Is(err, delegate.ErrAlreadyFired):
		// A run for this (event, trigger) already committed: a previous
		// holder fired it and lost its lease before the terminal write, or
		// the immediate path fired it first. The existing run is the
		// materialization.
		skip(domain.PendingFiringSkipAlreadyFired)
		return
	case errors.Is(err, delegate.ErrTaskBusy):
		// Another run went live on the task between the claim and the
		// fenced insert. Routine waiting, not a failure: the deferral refunds
		// the attempt, and the claim filter holds the row until the task
		// frees. The deferral reads the condition the filter negates, so a
		// refusal means the run that refused the insert ended before that
		// read: the task is free, and the backoff's retry is the one that
		// fires.
		span.SetAttributes(telemetry.Outcome("task_busy"))
		terminal(func(termCtx context.Context) {
			derr := r.firings.DeferWhileTaskBusy(termCtx, receipt)
			if errors.Is(derr, workitem.ErrDeferRefused) {
				routerLog.InfoContext(termCtx, "firing-queue: task busy at the fenced insert and free by the deferral; requeued with backoff",
					"firing_id", f.ID, "task_id", f.TaskID)
				parked, rerr := r.firings.Requeue(termCtx, receipt, workitem.OutcomeTransient, err)
				if rerr == nil && parked {
					routerLog.WarnContext(termCtx, "firing-queue: parked row; it will not fire again without an operator",
						"firing_id", f.ID, "task_id", f.TaskID, "trigger", f.TriggerID, "outcome", string(workitem.OutcomeTransient), "attempt", receipt.Attempt, "cause", err)
				}
				loser(termCtx, "requeue", rerr)
				return
			}
			if derr == nil {
				routerLog.InfoContext(termCtx, "firing-queue: deferred, task busy", "firing_id", f.ID, "task_id", f.TaskID)
			}
			loser(termCtx, "defer", derr)
		})
		return
	case errors.Is(err, context.DeadlineExceeded):
		span.SetStatus(codes.Error, "fire delegate")
		requeue(workitem.OutcomeDeadline, fmt.Errorf("fire delegate: %w", err))
		return
	default:
		span.SetStatus(codes.Error, "fire delegate")
		routerLog.WarnContext(unitCtx, "firing-queue: fire failed, firing not consumed",
			"firing_id", f.ID, "task_id", f.TaskID, "trigger", f.TriggerID, "attempt", receipt.Attempt, "error", err)
		requeue(workitem.OutcomeTransient, fmt.Errorf("fire delegate: %w", err))
		return
	}

	span.SetAttributes(telemetry.BlueprintRunID(blueprintRunID))
	terminal(func(termCtx context.Context) {
		loser(termCtx, "mark fired", r.firings.MarkFired(termCtx, receipt, blueprintRunID))
	})
}
