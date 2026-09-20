package routing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workkinds"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"go.opentelemetry.io/otel/codes"
)

// The event-queue worker is a FencedReplay consumer of the shared work-item
// contract (internal/db/workitem), over the kind workkinds.EventQueue
// declares. Every unit it runs is a replay by construction — an expired lease
// hands the row to the next claim — and three domain fences make the replay a
// no-op: (1) task mint and bump are idempotent under the tasks partial unique
// index on (entity_id, event_type, dedup_key) for active rows; (2) trigger
// firing is fenced on blueprint_runs (triggering_event_id, trigger_id) plus
// the one-active-run index, with ErrTaskBusy deferring onto pending_firings;
// (3) the close phase is fenced on the entity's poll_seq (entity_poll_seq on
// the queue row, CloseTerminalSystem's guard) for terminating events, and on
// Tasks.Close being a guarded transition that no-ops on an already-closed
// row for typed closes. The events audit row is written at admission, not by
// the unit, so a replay never re-records it.
//
// Nothing here resets, sweeps or renews on a timer. A row whose holder died
// is reclaimed by the first claim after its lease expires, and the reclaim
// is logged because it is the one signal that a unit was interrupted.

// eventClaimBatch is how many rows one claim takes. Ten rather than one
// because the kind's fairness interleaves at batch granularity: a batch from
// one org leaves it with ten leased rows, so the next pick prefers another.
const eventClaimBatch = 10

// claimErrorEscalateThreshold is how many consecutive claim failures the
// worker tolerates before escalating from a one-line notice to a loud
// "routing stalled" warning. A transient blip clears in one tick; a
// persistent schema/connection fault would otherwise only ever drip a
// uniform log line, so the threshold turns a sustained outage into an
// obviously-different, periodically-repeated signal (and a recovery line
// when it clears).
const claimErrorEscalateThreshold = 5

// terminalWriteTimeout bounds a unit's terminal write on its own context: a
// unit that hit its deadline must still be able to record that fact, and the
// guard on the write is fresh database time against a lease that outlives
// the deadline by half a minute.
const terminalWriteTimeout = 10 * time.Second

// Default cadences for RunEventQueue, exported so main can tune them and
// tests can drive the loop fast. The floor scan is the correctness
// backstop (a dropped wake only delays to the next tick) and is what
// picks a requeued row up once its backoff ripens; prune is the retention
// sweep; pruneAge is how long settled rows are kept for debuggability
// before deletion.
const (
	DefaultEventScanInterval  = 3 * time.Second
	DefaultEventPruneInterval = 1 * time.Hour
	DefaultEventPruneAge      = 7 * 24 * time.Hour
)

// RunEventQueue is the durable router drain loop. It replaces
// the router's old eventbus subscription: rather than ride the lossy
// in-memory bus (which drops events for slow subscribers under burst),
// the router drains the event_queue table that the ingestor populates
// durably at emit time.
//
// Correctness comes from the table plus the periodic floor scan; the
// wake channel is a best-effort latency optimization — a dropped wake
// only delays a drain to the next scan tick, never loses an event.
// Recovery is the claim's: a row left leased by a crash is claimable
// again once its lease expires, so a restart drains whatever was buffered
// with no reset of its own.
//
// Single worker, claiming in batches interleaved across orgs — exactly
// one process may run this at a time, which is what makes it a brain
// component rather than a replica-safe one. Returns when ctx is
// cancelled. A nil EventQueueStore (SetEventQueue never called) makes this
// a logged no-op rather than a panicking goroutine.
func (r *Router) RunEventQueue(ctx context.Context, wake <-chan struct{}, scanInterval, pruneInterval, pruneAge time.Duration) {
	if r.eventQueue == nil {
		routerLog.Warn("event-queue worker not started: no EventQueueStore wired")
		return
	}

	scan := time.NewTicker(scanInterval)
	defer scan.Stop()
	prune := time.NewTicker(pruneInterval)
	defer prune.Stop()

	// drain runs one full drain pass and tracks consecutive claim
	// failures so a sustained DB fault escalates loudly instead of only
	// dripping a per-tick line (see claimErrorEscalateThreshold). The
	// floor scan paces retries at scanInterval, so no extra backoff is
	// needed — and adding one would only slow recovery once the DB heals.
	claimFails := 0
	drain := func() {
		if err := r.drainEventQueue(ctx); err != nil {
			claimFails++
			switch {
			case claimFails == 1:
				routerLog.Warn("event-queue claim failed, retrying on the next scan", "error", err)
			case claimFails == claimErrorEscalateThreshold || claimFails%claimErrorEscalateThreshold == 0:
				routerLog.Error("event-queue routing stalled, not progressing", "claim_failures", claimFails, "error", err)
			}
			return
		}
		if claimFails >= claimErrorEscalateThreshold {
			routerLog.Info("event-queue routing recovered", "claim_failures", claimFails)
		}
		claimFails = 0
	}

	drain() // drain whatever survived the restart

	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
			drain()
		case <-scan.C:
			drain()
		case <-prune.C:
			if n, err := r.eventQueue.PruneSettled(ctx, time.Now().UTC().Add(-pruneAge)); err != nil {
				routerLog.Error("event-queue prune failed", "error", err)
			} else if n > 0 {
				routerLog.Info("event-queue prune: removed settled rows", "rows", n, "older_than", pruneAge)
			}
		}
	}
}

// eventQueueKind is the kind the worker's timing comes from. The dialect is
// irrelevant to the policy, which is all the worker reads.
var eventQueueKind = workkinds.EventQueue(workitem.SQLite)

// drainEventQueue claims and processes rows in batches until the queue is
// drained or ctx is cancelled. Returns the claim error if one occurs so the
// caller can track consecutive failures and escalate; every other ending —
// drained, deferred rows not yet ripe, cancelled — returns nil.
//
// A batch shorter than eventClaimBatch ends the pass: the queue is drained,
// or what remains is deferred and not yet ripe, and the next scan tick owns
// it. A failing row no longer holds the pass up — its requeue carries a
// retry time, so the next claim skips it and takes the rows behind it.
//
// Cancellation is checked BETWEEN batches and nowhere else. That is the
// whole shape of shutdown for this worker: the claim runs on the
// cancellable ctx, so a demotion stops the next claim promptly, while the
// rows already claimed run to completion on their own detached contexts
// (see processQueuedEvent). Claiming is where stopping is free; a claimed
// row is a debt this process has already taken on.
func (r *Router) drainEventQueue(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil // clean stop, not a claim failure
		}
		executorID, bootEpoch := r.executorIdentity()
		batch, err := r.eventQueue.Claim(ctx, workitem.Owner{ID: executorID, Epoch: bootEpoch}, eventClaimBatch)
		if batch.Cancelled+batch.Parked > 0 {
			routerLog.Info("event-queue claim settled rows", "cancelled", batch.Cancelled, "parked", batch.Parked)
		}
		for _, ce := range batch.Events {
			if ce.Receipt.Reclaimed {
				// A reclaim means a unit was interrupted on some process:
				// the previous holder's lease ran out with no terminal write.
				routerLog.Warn("event-queue reclaimed a row whose lease expired without a terminal write",
					"queue_id", ce.Event.ID, "event_id", ce.Event.EventID, "previous_owner", ce.Receipt.PreviousOwner, "attempt", ce.Receipt.Attempt)
			}
		}
		// Leases committed before a claim error are real, and their rows
		// are this worker's to dispose of before it reports the failure.
		for _, ce := range batch.Events {
			r.processQueuedEvent(ctx, ce)
		}
		if err != nil {
			return err // caller owns logging + consecutive-failure escalation
		}
		if len(batch.Events) < eventClaimBatch {
			return nil
		}
	}
}

// processQueuedEvent routes one claimed row as one unit and writes its
// terminal under the receipt's fence: MarkDone on success, a typed Requeue
// on failure. Done therefore means done — every routing obligation the
// event carried was met — and a failure is never consumed: the row returns
// to the queue with a retry time, or parks with a typed reason for an
// operator once the budget is spent or the outcome is permanent.
//
// The unit runs on ctx with its cancellation dropped and its values (trace
// context) kept, under the kind's UnitDeadline. Cancelling mid-unit is not a
// cheaper stop, it is a worse one: the routing writes may have committed,
// and an aborted unit leaves the row leased for the next claim to replay a
// minute later, which is a long detour to save nothing. drainEventQueue's
// between-batches gate is where a demotion actually stops this worker.
//
// The renewal at the top is the fence check and the point where a
// cancellation request is observed; every later write presents the receipt
// it returns. A lost lease means the row's next holder owns it, and this
// unit writes nothing further.
func (r *Router) processQueuedEvent(ctx context.Context, ce db.ClaimedEvent) {
	qe := ce.Event
	unitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), eventQueueKind.Policy.UnitDeadline)
	defer cancel()

	// The trace root for this event's routing, linked back to whoever
	// enqueued it. Opened around the WHOLE unit, terminal mark included, so
	// the span's duration is what the worker actually owed this row.
	unitCtx, span := startRouteSpan(unitCtx, &qe)
	defer span.End()

	receipt, err := r.eventQueue.RenewLease(unitCtx, ce.Receipt)
	switch {
	case errors.Is(err, workitem.ErrLeaseLost):
		span.SetAttributes(telemetry.Outcome("lease_lost"))
		routerLog.InfoContext(unitCtx, "event-queue: lease lost before routing; the row's next holder owns it",
			"queue_id", qe.ID, "event_id", qe.EventID)
		return
	case errors.Is(err, workitem.ErrCancelled):
		span.SetAttributes(telemetry.Outcome("cancelled"))
		routerLog.InfoContext(unitCtx, "event-queue: row settled cancelled at renewal", "queue_id", qe.ID, "event_id", qe.EventID)
		return
	case err != nil:
		// Skipped this pass: the lease expires on its own and the next
		// claim reclaims the row.
		span.SetStatus(codes.Error, "renew lease")
		routerLog.WarnContext(unitCtx, "event-queue: lease renewal failed; the row is reclaimed after its lease expires",
			"queue_id", qe.ID, "event_id", qe.EventID, "error", err)
		return
	}

	// The terminal write gets a context of its own, so a unit that ran out
	// its deadline can still record that it did.
	terminal := func(fn func(termCtx context.Context)) {
		termCtx, cancelTerm := context.WithTimeout(context.WithoutCancel(ctx), terminalWriteTimeout)
		defer cancelTerm()
		fn(termCtx)
	}
	requeue := func(outcome workitem.Outcome, cause error) {
		terminal(func(termCtx context.Context) {
			r.requeueQueuedEvent(termCtx, qe, receipt, outcome, cause)
		})
	}

	defer func() {
		if rec := recover(); rec != nil {
			// The poison-pill safety net: a panic in routing must not kill
			// the single worker goroutine and freeze the whole queue.
			span.SetStatus(codes.Error, "panic")
			routerLog.ErrorContext(unitCtx, "event-queue: panic routing event",
				"event_id", qe.EventID, "queue_id", qe.ID, "attempt", receipt.Attempt, "panic", rec)
			requeue(workitem.OutcomePoisonSuspected, fmt.Errorf("panic: %v", rec))
		}
	}()

	ev, err := r.events.GetSystem(unitCtx, qe.OrgID, qe.EventID)
	if err != nil {
		span.SetStatus(codes.Error, "load event")
		routerLog.WarnContext(unitCtx, "event-queue: load event failed", "event_id", qe.EventID, "queue_id", qe.ID, "error", err)
		requeue(workitem.OutcomeTransient, fmt.Errorf("load event: %w", err))
		return
	}
	if ev == nil {
		// A queue row that outlived its events row by a cascade cannot
		// route: it parks at once with a typed reason instead of burning
		// four retries on the same answer.
		span.SetAttributes(telemetry.Outcome("event_gone"))
		routerLog.WarnContext(unitCtx, "event-queue: event not found, parking", "event_id", qe.EventID, "queue_id", qe.ID)
		requeue(workitem.OutcomePermanent, errors.New("event row not found"))
		return
	}

	// The queue row is the envelope: the version the event was judged at
	// rides here, not on the events row, and reaches the terminating close.
	if err := r.routeEvent(unitCtx, *ev, qe.EntityPollSeq); err != nil {
		// A routing obligation went unmet — a dependency the pass needs
		// failed, not a legitimate "nothing to do" outcome (those return
		// nil and publish their own taskless disposition). Requeue rather
		// than consume: replaying is safe behind the fences, and it is the
		// only path back, since the tracker's snapshot-diff will not
		// re-emit this event.
		span.SetStatus(codes.Error, "route event")
		routerLog.WarnContext(unitCtx, "event-queue: routing failed, event not consumed",
			"event_id", qe.EventID, "queue_id", qe.ID, "attempt", receipt.Attempt, "error", err)
		outcome := workitem.OutcomeTransient
		if errors.Is(err, context.DeadlineExceeded) {
			outcome = workitem.OutcomeDeadline
		}
		requeue(outcome, fmt.Errorf("route: %w", err))
		return
	}

	terminal(func(termCtx context.Context) {
		err := r.eventQueue.MarkDone(termCtx, receipt)
		switch {
		case err == nil:
		case errors.Is(err, workitem.ErrLeaseLost):
			// The routing side effects committed; the row's next claimer
			// replays a unit the three fences absorb.
			span.SetAttributes(telemetry.Outcome("lease_lost"))
			routerLog.InfoContext(termCtx, "event-queue: lease lost at mark done; the successor's replay is fenced", "queue_id", qe.ID)
		case errors.Is(err, workitem.ErrCancelled):
			span.SetAttributes(telemetry.Outcome("cancelled"))
			routerLog.InfoContext(termCtx, "event-queue: row settled cancelled at mark done", "queue_id", qe.ID)
		default:
			// The lease expires and the row is reclaimed; nothing retries a
			// terminal write in a loop.
			span.SetStatus(codes.Error, "mark done")
			routerLog.ErrorContext(termCtx, "event-queue: mark done failed; the row is reclaimed after its lease expires", "queue_id", qe.ID, "error", err)
		}
	})
}

// requeueQueuedEvent records a failed attempt under its typed outcome and
// logs where the row landed. A park is a decision to stop, not a decision
// to discard: the row is retained (PruneSettled collects only done and
// cancelled rows), and an operator can put it back through the parked
// surface, which grants a fresh budget. That is why the cause recorded here
// is worth composing carefully — it is what a human reads when deciding
// whether redriving will do any good.
func (r *Router) requeueQueuedEvent(ctx context.Context, qe domain.QueuedEvent, receipt workitem.Receipt, outcome workitem.Outcome, cause error) {
	parked, err := r.eventQueue.Requeue(ctx, receipt, outcome, cause)
	switch {
	case err == nil:
		if parked {
			routerLog.WarnContext(ctx, "event-queue: parked row; it will not run again without an operator",
				"queue_id", qe.ID, "event_id", qe.EventID, "outcome", string(outcome), "attempt", receipt.Attempt, "cause", cause)
		}
	case errors.Is(err, workitem.ErrLeaseLost):
		routerLog.InfoContext(ctx, "event-queue: lease lost at requeue; the row's next holder owns it", "queue_id", qe.ID)
	case errors.Is(err, workitem.ErrCancelled):
		routerLog.InfoContext(ctx, "event-queue: row settled cancelled at requeue", "queue_id", qe.ID)
	default:
		routerLog.ErrorContext(ctx, "event-queue: requeue failed; the row is reclaimed after its lease expires", "queue_id", qe.ID, "error", err)
	}
}
