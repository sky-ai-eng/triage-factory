package routing

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// maxEventAttempts caps how many times the drain worker re-claims a
// single queue row before parking it as failed (a poison pill). A
// well-behaved event routes on attempt 1; repeated attempts mean a panic
// or a persistent transient error, and parking it stops the worker from
// spinning on one row while the rest of the queue waits behind it.
const maxEventAttempts = 5

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
		log.Printf("[router] event-queue worker not started: no EventQueueStore wired (call SetEventQueue)")
		return
	}

	// Boot crash-recovery: a 'processing' row at startup means the worker
	// died mid-process (single worker, so no other claimant). Reset it to
	// 'pending' and replay — re-routing is idempotent w.r.t. task creation
	// via the tasks dedup index.
	if n, err := r.eventQueue.ResetProcessing(ctx); err != nil {
		log.Printf("[router] event-queue boot recovery: reset processing rows: %v", err)
	} else if n > 0 {
		log.Printf("[router] event-queue boot recovery: reset %d in-flight row(s) to pending", n)
	}

	scan := time.NewTicker(scanInterval)
	defer scan.Stop()
	prune := time.NewTicker(pruneInterval)
	defer prune.Stop()

	r.drainEventQueue(ctx) // drain whatever survived the restart

	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
			r.drainEventQueue(ctx)
		case <-scan.C:
			r.drainEventQueue(ctx)
		case <-prune.C:
			if n, err := r.eventQueue.PruneDone(ctx, time.Now().Add(-pruneAge)); err != nil {
				log.Printf("[router] event-queue prune: %v", err)
			} else if n > 0 {
				log.Printf("[router] event-queue prune: removed %d done row(s) older than %s", n, pruneAge)
			}
		}
	}
}

// drainEventQueue claims and processes pending rows until the queue is
// empty (or ctx is cancelled).
func (r *Router) drainEventQueue(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		qe, err := r.eventQueue.ClaimNext(ctx)
		if err != nil {
			log.Printf("[router] event-queue claim: %v", err)
			return // transient; the next scan tick retries
		}
		if qe == nil {
			return // queue drained
		}
		r.processQueuedEvent(ctx, qe)
	}
}

// processQueuedEvent routes one claimed row, then marks it terminal.
// HandleEvent is best-effort (logs and continues on per-step errors), so
// a successful run marks the row done. The recover guard is the poison-
// pill safety net: a panic in routing must not kill the single worker
// goroutine and freeze the whole queue.
func (r *Router) processQueuedEvent(ctx context.Context, qe *domain.QueuedEvent) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[router] event-queue: panic routing event %s (queue id %d, attempt %d): %v",
				qe.EventID, qe.ID, qe.Attempts, rec)
			r.parkOrRequeue(ctx, qe, fmt.Sprintf("panic: %v", rec))
		}
	}()

	ev, err := r.events.GetSystem(ctx, qe.OrgID, qe.EventID)
	if err != nil {
		// Transient read failure — requeue for another attempt.
		log.Printf("[router] event-queue: load event %s (queue id %d): %v", qe.EventID, qe.ID, err)
		r.parkOrRequeue(ctx, qe, fmt.Sprintf("load event: %v", err))
		return
	}
	if ev == nil {
		// The event row is gone (e.g. its entity was cascade-deleted).
		// Nothing to route — park it failed so the worker doesn't spin.
		log.Printf("[router] event-queue: event %s not found for queue id %d — marking failed", qe.EventID, qe.ID)
		if err := r.eventQueue.MarkFailed(ctx, qe.OrgID, qe.ID, "event row not found"); err != nil {
			log.Printf("[router] event-queue: mark failed (missing event, queue id %d): %v", qe.ID, err)
		}
		return
	}

	r.HandleEvent(*ev)

	if err := r.eventQueue.MarkDone(ctx, qe.OrgID, qe.ID); err != nil {
		// The routing side effects already committed. A failed mark-done
		// leaves the row 'processing'; the floor scan only claims
		// 'pending', so it won't be re-routed until the next restart's
		// boot recovery resets it — at which point re-routing is dedup-
		// safe (tasks dedup index). Rare DB anomaly; log loudly and move
		// on rather than risk a double-route by requeuing.
		log.Printf("[router] event-queue: mark done (queue id %d): %v — row left processing, re-route on restart is dedup-safe", qe.ID, err)
	}
}

// parkOrRequeue requeues a row for another attempt, or parks it failed
// once it has burned through maxEventAttempts. Used for transient
// failures and panics so one bad event can't wedge the queue.
func (r *Router) parkOrRequeue(ctx context.Context, qe *domain.QueuedEvent, reason string) {
	if qe.Attempts >= maxEventAttempts {
		if err := r.eventQueue.MarkFailed(ctx, qe.OrgID, qe.ID,
			fmt.Sprintf("%s (after %d attempts)", reason, qe.Attempts)); err != nil {
			log.Printf("[router] event-queue: mark failed (queue id %d): %v", qe.ID, err)
		}
		return
	}
	if err := r.eventQueue.Requeue(ctx, qe.OrgID, qe.ID, reason); err != nil {
		log.Printf("[router] event-queue: requeue (queue id %d): %v", qe.ID, err)
	}
}
