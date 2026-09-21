package routing

import (
	"context"
	"errors"
	"fmt"
	"time"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workkinds"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// The re-derive worker is a SingleTx consumer of the shared work-item
// contract (internal/db/workitem), over the kind workkinds.TaskReDerive
// declares: one row per task whose scores have landed and whose deferred
// triggers have not been evaluated against them. The score write admits the
// row and raises its requested_revision in the transaction that writes the
// scores; the claim freezes that revision into the receipt; the evaluation
// reads the task outside any transaction; and the completion locks the row,
// compares the frozen revision with the row's, and either commits the
// evaluation's firing admissions with the terminal flip or commits nothing
// and defers.
//
// Why the reads sit outside the transaction and the check inside: every
// score writer raises requested_revision in the transaction that writes the
// score. A read that saw a newer score than the receipt froze belongs to a
// write whose raise is already committed, so the check under the lock sees it
// and defers. A read that saw the old score while the newer one commits
// afterwards either commits before the lock — the check sees the raise — or
// waits behind it — the completion finishes on the old score, and the
// writer's admission then finds a done row and inserts a fresh ready one for
// the new revision. The frozen value is the only thing ever compared against
// the row.
//
// The worker never fires inline and never folds an event into a live
// conversation: it admits a firing row, and the firing worker fires it when
// the task is free, after that worker's own validations (task open, the
// bot's claim standing, trigger enabled, breaker under threshold). Nothing
// here resets, sweeps or renews on a timer. A row whose holder died is
// reclaimed by the first claim after its lease expires, and the reclaim is
// logged because it is the one signal that a unit was interrupted. An
// evaluation that keeps failing on a read carries backoff and parks after its
// budget for a person to look at.

// reDeriveClaimBatch is how many rows one claim takes. Ten rather than one
// because the kind's fairness interleaves at batch granularity: a batch from
// one org leaves it with ten leased rows, so the next pick prefers another.
const reDeriveClaimBatch = 10

// DefaultReDeriveScanInterval is the floor scan for RunReDeriveQueue,
// exported so main can tune it and tests can drive the loop fast. The scan
// is the correctness backstop: a dropped wake, a score written on a process
// that has no waker, or a requeued row's backoff ripening all wait at most
// one tick.
const DefaultReDeriveScanInterval = 3 * time.Second

// taskReDeriveKind is the kind the worker's timing comes from. The dialect is
// irrelevant to the policy, which is all the worker reads.
var taskReDeriveKind = workkinds.TaskReDerive(workitem.SQLite)

// WakeReDerive nudges the re-derive worker: a scoring cycle committed, so
// rows were admitted or raised. Non-blocking, and best-effort: the wake
// channel holds one signal, and a dropped wake only delays the drain to the
// next scan tick.
func (r *Router) WakeReDerive() {
	select {
	case r.rederiveWake <- struct{}{}:
	default:
	}
}

// RunReDeriveQueue is the re-derive worker's loop: an initial drain, then a
// drain on every wake and every scan tick. Single worker, claiming in batches
// interleaved across orgs — exactly one process may run this at a time,
// which is what makes it a brain component rather than a replica-safe one.
// Returns when ctx is cancelled. A nil TaskReDeriveStore makes this a logged
// no-op rather than a panicking goroutine.
func (r *Router) RunReDeriveQueue(ctx context.Context, scanInterval time.Duration) {
	if r.rederive == nil {
		routerLog.Warn("rederive-queue worker not started: no TaskReDeriveStore wired")
		return
	}

	scan := time.NewTicker(scanInterval)
	defer scan.Stop()

	fails := claimFailures{name: "rederive-queue"}
	drain := func() { fails.observe(r.drainReDeriveQueue(ctx)) }

	drain() // drain whatever survived the restart

	for {
		select {
		case <-ctx.Done():
			return
		case <-r.rederiveWake:
			drain()
		case <-scan.C:
			drain()
		}
	}
}

// drainReDeriveQueue claims and processes rows in batches until the queue
// is drained or ctx is cancelled. Returns the claim error if one occurs so
// the caller can track consecutive failures and escalate; every other
// ending — drained, deferred rows not yet ripe, cancelled — returns nil.
//
// A batch shorter than reDeriveClaimBatch ends the pass: the queue is
// drained, or what remains is behind a retry time, and the next wake or scan
// tick owns it. Cancellation is checked between batches and nowhere else, as
// the firing worker's is: the claim runs on the cancellable ctx, while the
// rows already claimed run to completion on their own detached contexts.
func (r *Router) drainReDeriveQueue(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil // clean stop, not a claim failure
		}
		executorID, bootEpoch := r.executorIdentity()
		batch, err := r.rederive.Claim(ctx, workitem.Owner{ID: executorID, Epoch: bootEpoch}, reDeriveClaimBatch)
		if batch.Cancelled+batch.Parked > 0 {
			routerLog.Info("rederive-queue claim settled rows", "cancelled", batch.Cancelled, "parked", batch.Parked)
		}
		for _, item := range batch.Items {
			if item.Receipt.Reclaimed {
				// A reclaim means a unit was interrupted on some process:
				// the previous holder's lease ran out with no terminal write.
				// Nothing it did landed, because its admissions were inside
				// its uncommitted completion.
				routerLog.Warn("rederive-queue reclaimed a row whose lease expired without a terminal write",
					"rederive_id", item.Receipt.ItemID, "task_id", item.TaskID, "previous_owner", item.Receipt.PreviousOwner, "attempt", item.Receipt.Attempt)
			}
		}
		// Leases committed before a claim error are real, and their rows
		// are this worker's to dispose of before it reports the failure.
		for _, item := range batch.Items {
			r.processReDerive(ctx, item)
		}
		if err != nil {
			return err // caller owns logging + consecutive-failure escalation
		}
		if len(batch.Items) < reDeriveClaimBatch {
			return nil
		}
	}
}

// landedFiring is one planned firing after the completion committed: whether
// its admission inserted a row or collapsed onto one already queued, and
// whether its claim stamp moved the task.
type landedFiring struct {
	firing   reDeriveFiring
	inserted bool
	claimed  bool
}

// processReDerive runs one claimed row as one unit: renew the lease,
// evaluate the task against the score it carries now, and commit the
// evaluation's firing admissions with the terminal flip under the receipt's
// fence — or commit nothing and defer, when a newer score landed since the
// claim. Done means done: the plan's rows are admitted, or the evaluation
// decided there was nothing to admit. A failure is never consumed: a read
// that reached no verdict returns the row to the queue with a retry time, or
// parks it once the budget is spent.
//
// The unit runs on ctx with its cancellation dropped and its values kept,
// under the kind's UnitDeadline, for the reason the firing worker gives:
// cancelling mid-unit is a worse stop, not a cheaper one. The renewal at the
// top is the fence check and the point where a cancellation request is
// observed; every later write presents the receipt it returns.
//
// Every terminal write's loser contract is the same: ErrLeaseLost logs at
// info and stops — nothing landed, and the row's next holder re-evaluates;
// ErrCancelled logs at info and the row is settled; any other error logs at
// error and stops, the lease expires and the row is reclaimed. Nothing
// retries a terminal write in a loop.
func (r *Router) processReDerive(ctx context.Context, item dbpkg.ClaimedReDerive) {
	unitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), taskReDeriveKind.Policy.UnitDeadline)
	defer cancel()

	unitCtx, span := tracer.Start(unitCtx, "rederive.evaluate", trace.WithAttributes(
		attribute.Int64("rederive.id", item.Receipt.ItemID),
		telemetry.TaskID(item.TaskID),
		attribute.Int64("rederive.requested_revision", item.RequestedRevision),
	))
	defer span.End()

	receipt, err := r.rederive.RenewLease(unitCtx, item.Receipt)
	switch {
	case errors.Is(err, workitem.ErrLeaseLost):
		span.SetAttributes(telemetry.Outcome("lease_lost"))
		routerLog.InfoContext(unitCtx, "rederive-queue: lease lost before evaluating; the row's next holder owns it",
			"rederive_id", item.Receipt.ItemID, "task_id", item.TaskID)
		return
	case errors.Is(err, workitem.ErrCancelled):
		span.SetAttributes(telemetry.Outcome("cancelled"))
		routerLog.InfoContext(unitCtx, "rederive-queue: row settled cancelled at renewal", "rederive_id", item.Receipt.ItemID, "task_id", item.TaskID)
		return
	case err != nil:
		// Skipped this pass: the lease expires on its own and the next
		// claim reclaims the row.
		span.SetStatus(codes.Error, "renew lease")
		routerLog.WarnContext(unitCtx, "rederive-queue: lease renewal failed; the row is reclaimed after its lease expires",
			"rederive_id", item.Receipt.ItemID, "task_id", item.TaskID, "error", err)
		return
	}
	orgID := receipt.OrgID

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
			routerLog.InfoContext(termCtx, "rederive-queue: lease lost at "+verb+"; nothing landed and the successor re-evaluates", "rederive_id", receipt.ItemID, "task_id", item.TaskID)
		case errors.Is(err, workitem.ErrCancelled):
			span.SetAttributes(telemetry.Outcome("cancelled"))
			routerLog.InfoContext(termCtx, "rederive-queue: row settled cancelled at "+verb, "rederive_id", receipt.ItemID, "task_id", item.TaskID)
		default:
			span.SetStatus(codes.Error, verb)
			routerLog.ErrorContext(termCtx, "rederive-queue: "+verb+" failed; the row is reclaimed after its lease expires", "rederive_id", receipt.ItemID, "task_id", item.TaskID, "error", err)
		}
	}
	requeue := func(outcome workitem.Outcome, cause error) {
		terminal(func(termCtx context.Context) {
			parked, err := r.rederive.Requeue(termCtx, receipt, outcome, cause)
			if err == nil && parked {
				routerLog.WarnContext(termCtx, "rederive-queue: parked row; the task's deferred triggers will not be evaluated again without an operator",
					"rederive_id", receipt.ItemID, "task_id", item.TaskID, "outcome", string(outcome), "attempt", receipt.Attempt, "cause", cause)
			}
			loser(termCtx, "requeue", err)
		})
	}

	defer func() {
		if rec := recover(); rec != nil {
			// The poison-pill safety net: a panic in a unit must not kill
			// the single worker goroutine and freeze the whole queue.
			span.SetStatus(codes.Error, "panic")
			routerLog.ErrorContext(unitCtx, "rederive-queue: panic evaluating",
				"rederive_id", receipt.ItemID, "task_id", item.TaskID, "attempt", receipt.Attempt, "panic", rec)
			requeue(workitem.OutcomePoisonSuspected, fmt.Errorf("panic: %v", rec))
		}
	}()

	plan, err := r.evaluateReDerive(unitCtx, orgID, item.TaskID)
	if err != nil {
		outcome := workitem.OutcomeTransient
		if errors.Is(err, context.DeadlineExceeded) {
			outcome = workitem.OutcomeDeadline
		}
		span.SetStatus(codes.Error, "evaluate")
		routerLog.WarnContext(unitCtx, "rederive-queue: evaluation reached no verdict, row not consumed",
			"rederive_id", receipt.ItemID, "task_id", item.TaskID, "attempt", receipt.Attempt, "error", err)
		requeue(outcome, err)
		return
	}
	span.SetAttributes(attribute.Int("rederive.planned_firings", len(plan.Firings)))

	// The completion: every planned firing admitted, in order, inside the
	// transaction that flips the row done. An error from any admission rolls
	// the whole thing back, so nothing lands half-way. An empty plan is a
	// completion with no effects.
	var landed []landedFiring
	terminal(func(termCtx context.Context) {
		err := r.rederive.Complete(termCtx, receipt, func(firings dbpkg.PendingFiringsStore) error {
			landed = landed[:0]
			for _, f := range plan.Firings {
				inserted, claimed, err := firings.Enqueue(termCtx, orgID, plan.Task.EntityID, plan.Task.ID, f.Trigger.ID, plan.Task.PrimaryEventID, claimStamp(f.AgentID, f.FiringTeam))
				if err != nil {
					return fmt.Errorf("admit firing for trigger %s: %w", f.Trigger.ID, err)
				}
				landed = append(landed, landedFiring{firing: f, inserted: inserted, claimed: claimed})
			}
			return nil
		})
		switch {
		case err == nil:
			anyInserted := false
			for _, l := range landed {
				if !l.inserted {
					// The intent is already queued: a firing for this (task,
					// trigger) sits unsettled on pending_firings, and its own
					// admission made the commitment.
					routerLog.DebugContext(termCtx, "rederive-queue: firing collapsed onto an already-queued row",
						"task_id", plan.Task.ID, "trigger", l.firing.Trigger.ID)
					continue
				}
				anyInserted = true
				routerLog.InfoContext(termCtx, "rederive-queue: admitted firing", "task_id", plan.Task.ID, "trigger", l.firing.Trigger.ID, "team", l.firing.FiringTeam)
				r.claimCommitted(orgID, plan.Task, l.firing.FiringTeam, l.firing.AgentID, l.claimed)
			}
			if anyInserted {
				r.WakeFirings()
			}
			span.SetAttributes(telemetry.Outcome("completed"))
		case errors.Is(err, dbpkg.ErrScoreMoved):
			// A newer score landed after the claim. The evaluation was
			// against the older one, so nothing of it lands; the deferral
			// refunds the attempt and the next claim freezes the newer
			// revision. A refused deferral means the revision is no longer
			// above the frozen value, which cannot happen because it never
			// decreases; the backoff retries it rather than guessing.
			span.SetAttributes(telemetry.Outcome("score_moved"))
			derr := r.rederive.DeferScoreMoved(termCtx, receipt)
			if errors.Is(derr, workitem.ErrDeferRefused) {
				routerLog.WarnContext(termCtx, "rederive-queue: completion refused on a moved score but the deferral found none; requeued with backoff",
					"rederive_id", receipt.ItemID, "task_id", item.TaskID)
				parked, rerr := r.rederive.Requeue(termCtx, receipt, workitem.OutcomeTransient, err)
				if rerr == nil && parked {
					routerLog.WarnContext(termCtx, "rederive-queue: parked row; the task's deferred triggers will not be evaluated again without an operator",
						"rederive_id", receipt.ItemID, "task_id", item.TaskID, "outcome", string(workitem.OutcomeTransient), "attempt", receipt.Attempt, "cause", err)
				}
				loser(termCtx, "requeue", rerr)
				return
			}
			if derr == nil {
				routerLog.InfoContext(termCtx, "rederive-queue: deferred, a newer score landed since the claim",
					"rederive_id", receipt.ItemID, "task_id", item.TaskID, "frozen_revision", item.RequestedRevision)
			}
			loser(termCtx, "defer", derr)
		case errors.Is(err, workitem.ErrLeaseLost), errors.Is(err, workitem.ErrCancelled):
			loser(termCtx, "complete", err)
		default:
			span.SetStatus(codes.Error, "complete")
			routerLog.WarnContext(termCtx, "rederive-queue: completion failed, row not consumed",
				"rederive_id", receipt.ItemID, "task_id", item.TaskID, "attempt", receipt.Attempt, "error", err)
			parked, rerr := r.rederive.Requeue(termCtx, receipt, workitem.OutcomeTransient, err)
			if rerr == nil && parked {
				routerLog.WarnContext(termCtx, "rederive-queue: parked row; the task's deferred triggers will not be evaluated again without an operator",
					"rederive_id", receipt.ItemID, "task_id", item.TaskID, "outcome", string(workitem.OutcomeTransient), "attempt", receipt.Attempt, "cause", err)
			}
			loser(termCtx, "requeue", rerr)
		}
	})
}
