package routing

import (
	"context"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
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
	// unlikely. Recovering a parked 'failed' row is a manual/admin
	// affordance (a separate ticket); it matters because the tracker's
	// snapshot-diff is forward-only and may not re-emit a parked event.
	// Ownership-scoped (TFAC-578): only sweeps rows this router instance
	// itself claimed during an earlier boot, never a live sibling's
	// still-processing row — see EventQueueStore.ResetProcessing.
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
			drain()
		case <-prune.C:
			if n, err := r.eventQueue.PruneDone(ctx, time.Now().Add(-pruneAge)); err != nil {
				routerLog.Error("event-queue prune failed", "error", err)
			} else if n > 0 {
				routerLog.Info("event-queue prune: removed done rows", "rows", n, "older_than", pruneAge)
			}
		}
	}
}

// drainEventQueue claims and processes pending rows until the queue is
// empty (or ctx is cancelled). Returns the ClaimNext error if one occurs
// so the caller can track consecutive failures and escalate; a clean
// drain or ctx cancellation returns nil.
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
		r.processQueuedEvent(ctx, qe)
	}
}

// processQueuedEvent routes one claimed row, then marks it terminal.
// HandleEvent is best-effort (logs and continues on per-step errors), so
// a successful run marks the row done. The recover guard is the poison-
// pill safety net: a panic in routing must not kill the single worker
// goroutine and freeze the whole queue.
//
// A claimed row is ONE UNIT, and the unit runs to completion. Everything
// below — the event load, the routing itself, and the terminal MarkDone /
// parkOrRequeue — runs on eventCtx, ctx with its cancellation dropped and
// its values (deadline-free trace context included) kept. Cancelling
// mid-unit is not a cheaper stop, it is a worse one: the row is already
// 'processing', ResetProcessing is ownership-scoped BOOT recovery, and a
// demoted pod does not reboot — so an aborted unit leaves a half-routed
// row stranded until that process happens to restart, which nothing else
// sweeps. Running on is cheap and bounded: this unit is DB writes and
// enqueues, never an inline LLM call or an agent process, and the lease
// contract already tolerates last-writer-wins on in-flight work (the one
// write that genuinely needs fencing, the tracker's snapshot RMW, carries
// its own poll_seq CAS). drainEventQueue's between-rows gate is where a
// demotion actually stops this worker.
func (r *Router) processQueuedEvent(ctx context.Context, qe *domain.QueuedEvent) {
	eventCtx := context.WithoutCancel(ctx)

	defer func() {
		if rec := recover(); rec != nil {
			routerLog.Error("event-queue: panic routing event",
				"event_id", qe.EventID, "queue_id", qe.ID, "attempt", qe.Attempts, "panic", rec)
			r.parkOrRequeue(eventCtx, qe, fmt.Sprintf("panic: %v", rec))
		}
	}()

	ev, err := r.events.GetSystem(eventCtx, qe.OrgID, qe.EventID)
	if err != nil {
		// Transient read failure — requeue for another attempt.
		routerLog.Warn("event-queue: load event failed", "event_id", qe.EventID, "queue_id", qe.ID, "error", err)
		r.parkOrRequeue(eventCtx, qe, fmt.Sprintf("load event: %v", err))
		return
	}
	if ev == nil {
		// The event row is gone (e.g. its entity was cascade-deleted).
		// Nothing to route — park it failed so the worker doesn't spin.
		routerLog.Warn("event-queue: event not found, marking failed", "event_id", qe.EventID, "queue_id", qe.ID)
		if err := r.eventQueue.MarkFailed(eventCtx, qe.OrgID, qe.ID, "event row not found"); err != nil {
			routerLog.Error("event-queue: mark failed (missing event)", "queue_id", qe.ID, "error", err)
		}
		return
	}

	r.HandleEvent(eventCtx, *ev)

	if err := r.eventQueue.MarkDone(eventCtx, qe.OrgID, qe.ID); err != nil {
		// The routing side effects already committed. A failed mark-done
		// leaves the row 'processing'; the floor scan only claims
		// 'pending', so it won't be re-routed until the next restart's
		// boot recovery resets it — at which point re-routing is dedup-
		// safe (tasks dedup index). Rare DB anomaly; log loudly and move
		// on rather than risk a double-route by requeuing.
		routerLog.Error("event-queue: mark done failed, row left processing (re-route on restart is dedup-safe)", "queue_id", qe.ID, "error", err)
	}
}

// parkOrRequeue requeues a row for another attempt, or parks it failed
// once it has burned through maxEventAttempts. Used for transient
// failures and panics so one bad event can't wedge the queue.
func (r *Router) parkOrRequeue(ctx context.Context, qe *domain.QueuedEvent, reason string) {
	if qe.Attempts >= maxEventAttempts {
		if err := r.eventQueue.MarkFailed(ctx, qe.OrgID, qe.ID,
			fmt.Sprintf("%s (after %d attempts)", reason, qe.Attempts)); err != nil {
			routerLog.Error("event-queue: mark failed", "queue_id", qe.ID, "error", err)
		}
		return
	}
	if err := r.eventQueue.Requeue(ctx, qe.OrgID, qe.ID, reason); err != nil {
		routerLog.Error("event-queue: requeue failed", "queue_id", qe.ID, "error", err)
	}
}
