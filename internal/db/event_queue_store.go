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

	// ClaimNext claims the globally-oldest pending row (FIFO by id),
	// flips it pending -> processing, stamps claimed_at + executor_id +
	// boot_epoch, increments attempts, and returns it — traceparent
	// included, since the claim is where a consumer picks the producer's
	// trace context back up. Returns (nil, nil) when the queue is empty.
	//
	// executorID/bootEpoch (the caller's persistent instance-registry
	// identity, TFAC-577) are stamped atomically in the same claim
	// statement, mirroring RunQueueStore.ClaimNextRun — so ResetProcessing
	// (TFAC-578) can later self-sweep only this instance's own
	// orphaned rows.
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
	// Ownership-scoped (TFAC-578), mirroring RunQueueStore.ResetProcessingRuns:
	// only rows stamped executor_id = executorID AND boot_epoch < bootEpoch
	// are reset — this instance's own orphans from a strictly earlier boot
	// of itself. A live sibling's still-processing row (a different
	// executor_id) is never touched, which is what makes a rolling deploy /
	// two-replica boot safe.
	ResetProcessing(ctx context.Context, executorID string, bootEpoch int64) (int, error)

	// PruneDone deletes 'done' rows whose processed_at < before across all
	// orgs, returning the count removed. The retention sweep — done rows
	// are kept for debuggability but bounded by age; failed rows are never
	// pruned here. Cross-org system sweep.
	PruneDone(ctx context.Context, before time.Time) (int, error)

	// ListForEntity returns every queue row for an entity in id order
	// regardless of status. Debug/audit views and conformance assertions.
	ListForEntity(ctx context.Context, orgID, entityID string) ([]domain.QueuedEvent, error)
}
