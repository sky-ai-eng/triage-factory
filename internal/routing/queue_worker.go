package routing

import (
	"context"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"go.opentelemetry.io/otel/codes"
)

// maxEventAttempts caps how many times the drain worker re-claims a
// single queue row before parking it as failed (a poison pill). A
// well-behaved event routes on attempt 1; repeated attempts mean a panic
// or a persistent transient error, and parking it stops the worker from
// spinning on one row while the rest of the queue waits behind it.
const maxEventAttempts = 5

// claimErrorEscalateThreshold is how many consecutive ClaimNext failures
// the worker tolerates before escalating from a one-line notice to a loud
// "routing stalled" warning. A transient blip clears in one tick; a
// persistent schema/connection fault would otherwise only ever drip a
// uniform log line, so the threshold turns a sustained outage into an
// obviously-different, periodically-repeated signal (and a recovery line
// when it clears).
const claimErrorEscalateThreshold = 5

// staleProcessingAfter is how long a row may sit 'processing' before the
// floor scan reclaims it regardless of who claimed it. RunEventQueue's boot
// reset is ownership-scoped, so it only ever recovers an owner that comes
// back; a pod that is replaced rather than rebooted — scale-down, a fresh
// instance id after the brain lease moves — leaves its claimed rows
// unrouted forever without this.
//
// Ten minutes is three-plus orders of magnitude past a real unit: a routing
// unit is milliseconds-to-seconds of DB writes and enqueues, never an
// inline LLM call or an agent process. So this cannot plausibly steal live
// work, and the double route it would cause if it did is absorbed by the
// same fences that make a boot replay safe (the tasks dedup index, the
// (event, trigger) replay fence, the one-active index). The asymmetry is
// the whole argument: a lost event is unrecoverable — the tracker's
// snapshot-diff is forward-only and will not re-emit it — while a duplicate
// one is fenced.
//
// FIFO is not preserved across a reclaim (the row re-enters the queue with
// its original id, ahead of rows enqueued since). Acceptable for the same
// reason: ordering is a nicety the fences don't depend on.
const staleProcessingAfter = 10 * time.Minute

// Default cadences for RunEventQueue, exported so main can tune them and
// tests can drive the loop fast. The floor scan is the correctness
// backstop (a dropped wake only delays to the next tick); prune is the
// retention sweep; pruneAge is how long 'done' rows are kept for
// debuggability before deletion.
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
// only delays a drain to the next scan tick, never loses an event. On
// boot it resets any 'processing' rows left by a crash back to 'pending'
// (single-worker recovery) and drains whatever was buffered before the
// restart, so buffered-but-unprocessed events survive a process restart.
//
// Single worker, global FIFO by id — an entity's events process in
// insertion order, preserving the per-entity ordering the old single-
// goroutine bus subscriber gave for free. Returns when ctx is cancelled.
// A nil EventQueueStore (SetEventQueue never called) makes this a logged
// no-op rather than a panicking goroutine.
func (r *Router) RunEventQueue(ctx context.Context, wake <-chan struct{}, scanInterval, pruneInterval, pruneAge time.Duration) {
	if r.eventQueue == nil {
		routerLog.Warn("event-queue worker not started: no EventQueueStore wired")
		return
	}

	// Boot crash-recovery: a 'processing' row at startup means the worker
	// died mid-process (single worker, so no other claimant). Reset it to
	// 'pending' and replay — re-routing is idempotent w.r.t. task creation
	// via the tasks dedup index.
	//
	// A reset row deliberately KEEPS its attempts count — but this is cheap
	// tail-risk insurance, not the main poison-pill guard. The realistic
	// poison case, a deterministic panic in HandleEvent, is caught
	// in-process by processQueuedEvent's recover and parked via attempts
	// with no crash at all. A row only reaches boot recovery still
	// 'processing' after a HARD death that bypasses recover (a deploy
	// SIGKILL, a cgroup OOM driven by something else, VM loss) — and since
	// the work done while a row is 'processing' is bounded (a few queries +
	// small metadata; the heavy delegation runs in a detached goroutine
	// after the row is marked done), that death is almost never caused by
	// this particular event. So retaining attempts here mostly guards an
	// asymmetric tail: IF some event ever did deterministically hard-crash
	// the process, resetting attempts would crash-loop the service on every
	// boot, whereas retaining parks the row after maxEventAttempts and
	// keeps the service healthy. The cost — an innocent event caught by an
	// unrelated crash spends one of its five retries, and would need ~5
	// such crashes on the same row to be wrongly parked — is mild and
	// unlikely. Recovering a parked 'failed' row is an operator affordance
	// (EventQueueStore.ListFailedEvents / RequeueFailedEvents, behind the
	// admin-gated /api/events/failed surface); nothing recovers one
	// automatically, because the tracker's snapshot-diff is forward-only and
	// will not re-emit a parked event.
	// Ownership-scoped: only sweeps rows this router instance
	// itself claimed during an earlier boot, never a live sibling's
	// still-processing row — see EventQueueStore.ResetProcessing. That
	// scoping is also why it is only half the recovery: an owner replaced
	// rather than rebooted never runs it, and the floor scan's
	// sweepStaleProcessing is what reclaims what it leaves behind.
	executorID, bootEpoch := r.executorIdentity()
	if n, err := r.eventQueue.ResetProcessing(ctx, executorID, bootEpoch); err != nil {
		routerLog.Error("event-queue boot recovery: reset processing rows failed", "error", err)
	} else if n > 0 {
		routerLog.Info("event-queue boot recovery: reset in-flight rows to pending", "rows", n)
	}

	scan := time.NewTicker(scanInterval)
	defer scan.Stop()
	prune := time.NewTicker(pruneInterval)
	defer prune.Stop()

	// drain runs one full drain pass and tracks consecutive ClaimNext
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
			// The floor scan is the correctness tick, so the staleness
			// backstop rides it rather than the wake channel (which is
			// burst-driven and can fire many times a second). Sweeping
			// before the drain means a row reclaimed here is claimed by
			// the very next pass instead of waiting a full tick.
			r.sweepStaleProcessing(ctx)
			drain()
		case <-prune.C:
			if n, err := r.eventQueue.PruneDone(ctx, time.Now().UTC().Add(-pruneAge)); err != nil {
				routerLog.Error("event-queue prune failed", "error", err)
			} else if n > 0 {
				routerLog.Info("event-queue prune: removed done rows", "rows", n, "older_than", pruneAge)
			}
		}
	}
}

// sweepStaleProcessing returns every row stuck 'processing' past
// staleProcessingAfter to pending, whoever claimed it. Runs on the floor
// scan of the drain loop, which the background-brain lease already makes
// single-holder — so this is not a second claimant racing the drainer, it
// is the same worker noticing that a row nobody is driving has been left
// behind. Logged only when it finds something: a non-zero count means a
// pod died holding claimed work, which is worth a line every time.
func (r *Router) sweepStaleProcessing(ctx context.Context) {
	n, err := r.eventQueue.RequeueStaleProcessing(ctx, staleProcessingAfter)
	if err != nil {
		routerLog.Error("event-queue: stale-processing sweep failed", "error", err)
		return
	}
	if n > 0 {
		routerLog.Warn("event-queue: requeued rows orphaned in 'processing' (owner replaced, not rebooted?)",
			"rows", n, "older_than", staleProcessingAfter)
	}
}

// drainEventQueue claims and processes pending rows until the queue is
// empty, a row asks to be retried later, or ctx is cancelled. Returns the
// ClaimNext error if one occurs so the caller can track consecutive failures
// and escalate; every other ending — drained, paused for a retry, cancelled —
// returns nil.
//
// Cancellation is checked BETWEEN rows and nowhere else. That is the whole
// shape of shutdown for this worker: the claim gate below runs on the
// cancellable ctx, so a demotion stops the next claim promptly, while the
// row already claimed runs to completion on its own detached context (see
// processQueuedEvent). Claiming is where stopping is free; a claimed row is
// a debt this process has already taken on.
func (r *Router) drainEventQueue(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil // clean stop, not a claim failure
		}
		executorID, bootEpoch := r.executorIdentity()
		qe, err := r.eventQueue.ClaimNext(ctx, executorID, bootEpoch)
		if err != nil {
			return err // caller owns logging + consecutive-failure escalation
		}
		if qe == nil {
			return nil // queue drained
		}
		if requeued := r.processQueuedEvent(ctx, qe); requeued {
			// The row went back to 'pending' with its attempt spent, and
			// ClaimNext takes the oldest pending row — which is this one
			// again. Draining on would hand the same row back within
			// microseconds and burn its whole retry budget on a blip that
			// hasn't had time to clear, parking an event the store would
			// have accepted a second later. The scan tick is this worker's
			// only pacing, so retries are its to space out.
			return nil
		}
	}
}

// processQueuedEvent routes one claimed row, then marks it terminal.
// Terminal has two shapes and HandleEvent's return picks between them: nil
// marks the row done, an error requeues it for another attempt (parked
// 'failed' once the attempt budget is spent). Done therefore means done —
// every routing obligation the event carried was met. A store failure inside
// routing used to reach MarkDone all the same, which permanently consumed the
// event: no task, no retry, nothing left to inspect. The recover guard is the
// poison-pill safety net on the same budget: a panic in routing must not kill
// the single worker goroutine and freeze the whole queue.
//
// A claimed row is ONE UNIT, and the unit runs to completion. Everything
// below — the event load, the routing itself, and the terminal MarkDone /
// parkOrRequeue — runs on eventCtx, ctx with its cancellation dropped and
// its values (deadline-free trace context included) kept. Cancelling
// mid-unit is not a cheaper stop, it is a worse one: the row is already
// 'processing', ResetProcessing is ownership-scoped BOOT recovery, and a
// demoted pod does not reboot — so an aborted unit leaves a half-routed
// row for the staleness backstop to reclaim minutes later, which is a long
// detour to save nothing. Running on is cheap and bounded: this unit is DB
// writes and enqueues, never an inline LLM call or an agent process, and
// the lease contract already tolerates last-writer-wins on in-flight work
// (the one write that genuinely needs fencing, the tracker's snapshot RMW,
// carries its own poll_seq CAS). drainEventQueue's between-rows gate is
// where a demotion actually stops this worker.
//
// Returns true when the row was requeued rather than resolved — the caller's
// signal to end the pass and let the next scan tick own the retry.
func (r *Router) processQueuedEvent(ctx context.Context, qe *domain.QueuedEvent) (requeued bool) {
	eventCtx := context.WithoutCancel(ctx)

	// The trace root for this event's routing, linked back to whoever
	// enqueued it. Opened around the WHOLE unit, terminal mark included, so
	// the span's duration is what the worker actually owed this row.
	eventCtx, span := startRouteSpan(eventCtx, qe)
	defer span.End()

	defer func() {
		if rec := recover(); rec != nil {
			span.SetStatus(codes.Error, "panic")
			routerLog.ErrorContext(eventCtx, "event-queue: panic routing event",
				"event_id", qe.EventID, "queue_id", qe.ID, "attempt", qe.Attempts, "panic", rec)
			requeued = r.parkOrRequeue(eventCtx, qe, fmt.Sprintf("panic: %v", rec))
		}
	}()

	ev, err := r.events.GetSystem(eventCtx, qe.OrgID, qe.EventID)
	if err != nil {
		// Transient read failure — requeue for another attempt.
		span.SetStatus(codes.Error, "load event")
		routerLog.WarnContext(eventCtx, "event-queue: load event failed", "event_id", qe.EventID, "queue_id", qe.ID, "error", err)
		return r.parkOrRequeue(eventCtx, qe, fmt.Sprintf("load event: %v", err))
	}
	if ev == nil {
		// The event row is gone (e.g. its entity was cascade-deleted).
		// Nothing to route — park it failed so the worker doesn't spin.
		// An outcome, not an error status: there is nothing wrong with a
		// queue row outliving the event it pointed at by a cascade.
		span.SetAttributes(telemetry.Outcome("event_gone"))
		routerLog.WarnContext(eventCtx, "event-queue: event not found, marking failed", "event_id", qe.EventID, "queue_id", qe.ID)
		if err := r.eventQueue.MarkFailed(eventCtx, qe.OrgID, qe.ID, "event row not found"); err != nil {
			routerLog.ErrorContext(eventCtx, "event-queue: mark failed (missing event)", "queue_id", qe.ID, "error", err)
		}
		return false
	}

	if err := r.HandleEvent(eventCtx, *ev); err != nil {
		// A routing obligation went unmet — a dependency the pass needs
		// failed, not a legitimate "nothing to do" outcome (those return
		// nil and publish their own taskless disposition). Requeue rather
		// than consume: replaying is safe (the tasks dedup index and the
		// firing fences collapse anything that already landed) and it is
		// the only path back, since the tracker's snapshot-diff will not
		// re-emit this event.
		span.SetStatus(codes.Error, "route event")
		routerLog.WarnContext(eventCtx, "event-queue: routing failed, event not consumed",
			"event_id", qe.EventID, "queue_id", qe.ID, "attempt", qe.Attempts, "error", err)
		return r.parkOrRequeue(eventCtx, qe, fmt.Sprintf("route: %v", err))
	}

	if err := r.eventQueue.MarkDone(eventCtx, qe.OrgID, qe.ID); err != nil {
		// The routing side effects already committed. A failed mark-done
		// leaves the row 'processing'; the floor scan only claims
		// 'pending', so it won't be re-routed until the next restart's
		// boot recovery resets it — at which point re-routing is dedup-
		// safe (tasks dedup index). Rare DB anomaly; log loudly and move
		// on rather than risk a double-route by requeuing.
		span.SetStatus(codes.Error, "mark done")
		routerLog.ErrorContext(eventCtx, "event-queue: mark done failed, row left processing (re-route on restart is dedup-safe)", "queue_id", qe.ID, "error", err)
	}
	return false
}

// parkOrRequeue requeues a row for another attempt, or parks it failed
// once it has burned through maxEventAttempts. Used for transient
// failures and panics so one bad event can't wedge the queue.
//
// A park is a decision to stop, not a decision to discard: the row is
// retained (PruneDone only collects 'done'), and an operator can put it back
// through the admin-gated parked-events surface, which grants a fresh budget.
// That is why the reason recorded here is worth composing carefully — it is
// what a human reads when deciding whether requeueing will do any good.
//
// Returns true only when the row actually reached 'pending' again — the one
// state where it is about to be re-claimed, so the caller must stop draining
// rather than pick it straight back up. A parked row returns false: it is
// terminal and ClaimNext will never see it again. So does a row whose
// Requeue itself failed, which leaves it stranded in 'processing' where
// ClaimNext cannot reach it either — stopping the pass over that row would
// hold up every other pending row for a scan tick and buy nothing.
func (r *Router) parkOrRequeue(ctx context.Context, qe *domain.QueuedEvent, reason string) (requeued bool) {
	if qe.Attempts >= maxEventAttempts {
		if err := r.eventQueue.MarkFailed(ctx, qe.OrgID, qe.ID,
			fmt.Sprintf("%s (after %d attempts)", reason, qe.Attempts)); err != nil {
			routerLog.Error("event-queue: mark failed", "queue_id", qe.ID, "error", err)
		}
		return false
	}
	if err := r.eventQueue.Requeue(ctx, qe.OrgID, qe.ID, reason); err != nil {
		routerLog.Error("event-queue: requeue failed", "queue_id", qe.ID, "error", err)
		return false
	}
	return true
}
