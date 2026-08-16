package db

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// EventQueueStore owns the event_queue table — the durable, DB-backed
// queue the router drains instead of riding the lossy in-memory bus.
// It is a transactional-outbox: Enqueue writes the events
// audit row and the queue row in one transaction, so a recorded event is
// always routable and a queued event always has its audit row.
//
// This is a system-service store: the ingestor (poller/tracker) and the
// drain worker run as background goroutines with no per-user identity, so
// the Postgres impl wires against the admin pool (BYPASSRLS) and keeps
// org_id bound in every statement as defense in depth. SQLite collapses
// onto its single connection and asserts the local sentinel org on the
// org-scoped methods.
//
// Method shapes mirror PendingFiringsStore (the sibling DB-backed queue):
// a claim/drain loop with a periodic sweeper. The key difference is the
// claim — ClaimNext reserves a row (pending -> processing) rather than a
// non-mutating pop, so a future multi-worker drainer is safe via
// FOR UPDATE SKIP LOCKED in Postgres.
// StaleProcessingReclaimReason is the last_error every backend stamps on a
// row RequeueStaleProcessing reclaims. Shared so the two impls can't drift
// and an operator reading the column sees one string for one cause.
const StaleProcessingReclaimReason = "stale processing reclaim"

// MaxFailedEventsRequeueIDs bounds how many queue ids one requeue call may
// name. A selection is made from a page, and a page is bounded by the list
// contract, so this only rejects a hand-rolled request — and rejecting it
// keeps the statement's bind list bounded.
const MaxFailedEventsRequeueIDs = 500

// TraceparentAt reads the i-th entry of a batch's parallel traceparent
// slice, returning "" (stored as NULL) when the slice doesn't cover i.
// Shared by both dialects' EnqueueBatchWithSnapshotCAS so a nil or short
// slice means the same thing in each: those events enqueue untraced, the
// way every untraced producer's row does.
func TraceparentAt(traceparents []string, i int) string {
	if i < len(traceparents) {
		return traceparents[i]
	}
	return ""
}

type EventQueueStore interface {
	// Enqueue atomically records the event (the durable audit row) AND
	// its queue row in a single transaction, returning the generated
	// event id. Empty evt.ID is generated as a v4. The caller (ingestor)
	// stamps the returned id onto the event before publishing it to the
	// ephemeral bus for cosmetic subscribers, so the WS feed and the
	// queue agree on the event id.
	//
	// Only entity-bearing github:/jira: events are enqueued — the
	// router's domain. System events stay bus-only and are never queued.
	//
	// traceparent is the producer's W3C trace context, stamped onto the
	// queue row so the drain worker can link an event's routing back to
	// the cycle that emitted it. It is a parameter rather than something
	// read off ctx here because a store must not depend on an ambient
	// span: the caller (the ingestor) owns the propagator, and a test can
	// hand over a literal. Empty — the normal case, with tracing off or an
	// untraced producer — is stored as NULL.
	Enqueue(ctx context.Context, orgID string, evt domain.Event, traceparent string) (eventID string, err error)

	// EnqueueBatchWithSnapshotCAS is the tracker's emit: the entity
	// snapshot advance and the durable enqueue of the transitions diffed
	// against it, in ONE transaction.
	//
	// It exists because those two writes are the same fact. The
	// snapshot-diff is the sole re-emit prevention, so the snapshot must
	// not advance ahead of the transitions it retires: a process that dies
	// between a won CAS and the enqueue loses them permanently (the next
	// cycle diffs new-against-new and produces nothing). One transaction
	// deletes that window — either both land or neither does.
	//
	// The CAS itself is UpdateSnapshotCASSystem's, semantics unchanged: the
	// UPDATE pins expectedPollSeq alongside org_id/id and bumps poll_seq by
	// 1 on success. A miss (a straggler ex-leader, stale by the time it
	// lands) matches zero rows, and then the whole transaction commits
	// NOTHING — ok=false, no events rows, no queue rows — so a losing
	// writer is invisible in the log rather than half-applied.
	//
	// events is the batch to enqueue on a won CAS: same writes Enqueue
	// makes, one events audit row + one queue row per event, and the
	// returned eventIDs are parallel to it (ids the caller stamps onto the
	// copies it forwards to the bus). traceparents is parallel too; a nil
	// or short slice stores NULL for the entries it doesn't cover, matching
	// Enqueue's empty-traceparent case. An EMPTY batch is a pure CAS, so
	// the tracker keeps one call path whether or not its diff produced
	// events.
	//
	// System-scoped like the CAS it subsumes: the tracker is a background
	// job with no JWT claims, and org_id is bound by argument.
	EnqueueBatchWithSnapshotCAS(ctx context.Context, orgID, entityID, snapshotJSON string, expectedPollSeq int64, events []domain.Event, traceparents []string) (ok bool, eventIDs []string, err error)

	// ClaimNext claims the globally-oldest pending row (FIFO by id),
	// flips it pending -> processing, stamps claimed_at + executor_id +
	// boot_epoch, increments attempts, and returns it — traceparent
	// included, since the claim is where a consumer picks the producer's
	// trace context back up. Returns (nil, nil) when the queue is empty.
	//
	// executorID/bootEpoch (the caller's persistent instance-registry
	// identity) are stamped atomically in the same claim statement,
	// mirroring RunQueueStore.ClaimNextRun — so ResetProcessing can later
	// self-sweep only this instance's own orphaned rows.
	//
	// Cross-org by design: the drain worker is a single system service
	// draining every tenant in insertion order, so this is one of the
	// explicitly org-wide system reads (the claimed row carries its
	// org_id, which scopes all downstream processing). Postgres uses
	// FOR UPDATE SKIP LOCKED so a future multi-worker drainer never
	// double-claims; SQLite is single-worker.
	ClaimNext(ctx context.Context, executorID string, bootEpoch int64) (*domain.QueuedEvent, error)

	// MarkDone marks a claimed row done (processed_at = now). Guarded by
	// status = 'processing' so a stale call can't flip an already-terminal
	// row. org_id is bound as defense in depth.
	MarkDone(ctx context.Context, orgID string, id int64) error

	// MarkFailed marks a claimed row failed with lastErr — the poison-pill
	// terminal, reserved for a row that has exhausted its retry budget.
	// Failed rows are retained (not pruned) for debugging. Guarded by
	// status = 'processing'.
	MarkFailed(ctx context.Context, orgID string, id int64, lastErr string) error

	// Requeue returns a claimed row to pending after a transient failure,
	// recording lastErr for visibility. attempts is left as-is (the claim
	// already counted it), so the worker can fail the row out once
	// attempts crosses its retry budget. Guarded by status = 'processing'.
	Requeue(ctx context.Context, orgID string, id int64, lastErr string) error

	// ResetProcessing flips 'processing' rows back to pending and returns
	// the count reset. Called once at boot: under the single worker a
	// 'processing' row at startup means a crash mid-process, so it must be
	// replayed.
	//
	// Ownership-scoped, mirroring RunQueueStore.ResetProcessingRuns:
	// only rows stamped executor_id = executorID AND boot_epoch < bootEpoch
	// are reset — this instance's own orphans from a strictly earlier boot
	// of itself. A live sibling's still-processing row (a different
	// executor_id) is never touched, which is what makes a rolling deploy /
	// two-replica boot safe.
	//
	// It is the fast path, not the whole recovery story: an owner that never
	// comes back (scale-down, replacement under a fresh instance id) never
	// runs it, which is what RequeueStaleProcessing backstops.
	ResetProcessing(ctx context.Context, executorID string, bootEpoch int64) (int, error)

	// RequeueStaleProcessing is the staleness backstop under ResetProcessing:
	// every 'processing' row claimed longer than olderThan ago returns to
	// pending with last_error = StaleProcessingReclaimReason, whoever owned
	// it. Ownership-scoping alone strands a row forever when the owner is
	// replaced rather than rebooted — the brain lease moves to another pod
	// and the original instance id never returns — and an unrouted event is
	// unrecoverable, since the tracker's snapshot-diff is forward-only and
	// will not re-emit it.
	//
	// attempts is deliberately untouched (the claim already counted this
	// try), so a row that keeps reaching this sweep still parks 'failed'
	// once its budget runs out rather than looping forever. claimed_at and
	// the ownership stamp are cleared: a pending row has no owner.
	//
	// olderThan is a duration rather than an absolute cutoff so the Postgres
	// impl can subtract it from the DB's own clock — claimed_at is stamped
	// with now() server-side, and a caller's wall clock in a multi-pod
	// deployment is not the same clock.
	//
	// Reclaiming a genuinely live row is safe by construction: a routing
	// unit is milliseconds-to-seconds of DB writes with no inline LLM call,
	// so any sane threshold is orders of magnitude past one, and the double
	// route a reclaim could produce is absorbed by the same fences that make
	// a replay safe (the tasks dedup index, the (event, trigger) replay
	// fence, the one-active index). Loss is unrecoverable, duplication is
	// fenced — so this errs toward reclaiming.
	RequeueStaleProcessing(ctx context.Context, olderThan time.Duration) (int, error)

	// PruneDone deletes 'done' rows whose processed_at < before across all
	// orgs, returning the count removed. The retention sweep — done rows
	// are kept for debuggability but bounded by age; failed rows are never
	// pruned here. Cross-org system sweep.
	//
	// The 'done'-only predicate is load-bearing, not incidental: a parked
	// row is the sole record of routing work that was dropped and will not
	// re-emit, so it has to survive until an operator has seen it and
	// decided (ListFailedEvents / RequeueFailedEvents below). Age-pruning
	// failed rows would delete the evidence and the recovery path together.
	PruneDone(ctx context.Context, before time.Time) (int, error)

	// ListFailedEvents returns one page of the org's parked 'failed' rows —
	// the operator surface over routing work the queue dropped — newest
	// first, plus the unpaged total of parked rows. id DESC is a total order
	// (id is monotonic per insert), so the pages partition the result set.
	//
	// The entity fields are outer-joined for display and read empty when the
	// row carries no entity or the entity row is gone. That is deliberate: a
	// queue row can outlive its entity by a cascade, and omitting it would
	// hide a dropped event precisely because something unusual happened to it.
	//
	// Org-scoped by argument on the admin pool, like every other method here
	// — the store is system-service wired, so the org-admin predicate in the
	// handler is the authorization, not RLS.
	ListFailedEvents(ctx context.Context, orgID string, opts ListOpts) ([]domain.FailedEvent, int, error)

	// GetFailedEvent returns one parked row by queue id, or (nil, nil) when
	// the org has no 'failed' row with that id. Same projection as
	// ListFailedEvents — a single read answers with the list's row shape, not
	// a bespoke one.
	GetFailedEvent(ctx context.Context, orgID string, id int64) (*domain.FailedEvent, error)

	// RequeueFailedEvents returns the named parked rows to 'pending' and
	// grants each a fresh attempt budget (attempts = 0). The fresh budget is
	// the point of an operator requeue: the rows parked because their budget
	// ran out, so retaining the spent count would re-park every one of them
	// on its first attempt. last_error is deliberately left in place until
	// the next attempt overwrites it, so the reason a row parked survives
	// long enough to correlate with what happens next; a row that parks
	// again carries its new reason, not the stale one. claimed_at, the
	// ownership stamp and processed_at are cleared — a pending row has no
	// owner and has not been processed.
	//
	// Only rows currently 'failed' flip. An id that is pending, processing,
	// done, belongs to another org, or does not exist at all is a silent
	// no-op counted out of the return, so the count is "rows this call
	// actually moved" and a second requeue of the same ids reports 0. That
	// count is the operator's answer, so a backend that cannot report it
	// errors rather than guessing. On error the count is what was confirmed
	// moved before the failure — a backend that splits a large id set into
	// several statements commits as it goes — so it is not necessarily 0.
	//
	// Replay safety is the same argument every other retry in this queue
	// rests on: a requeued row re-enters the identical at-least-once path,
	// where the tasks dedup index, the (triggering_event_id, trigger_id)
	// replay fence and the one-active-run index collapse anything that
	// already landed. Double-requeue, or requeue of an event whose task
	// arrived by other means, converges to no-ops.
	RequeueFailedEvents(ctx context.Context, orgID string, ids []int64) (int, error)

	// ListForEntity returns every queue row for an entity in id order
	// regardless of status. Debug/audit views and conformance assertions.
	ListForEntity(ctx context.Context, orgID, entityID string) ([]domain.QueuedEvent, error)
}
