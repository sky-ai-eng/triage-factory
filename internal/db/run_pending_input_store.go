package db

import "context"

// RunPendingInputStore is the durable half of resume-by-enqueue (TFAC-585):
// the message (+ acting user) recorded before a parked run's continuation
// is re-queued as ordinary claimable work, so a crash between "message
// recorded" and "process spawned" is recoverable by the standard boot
// sweep rather than an ad-hoc path. See
// docs/specs/horizontal-scaling/README.md §5.2.
//
// Both dialects (unlike RunSignalStore): local mode's dispatcher claims its
// own resumed runs through the identical queue path — resume-by-enqueue
// applies in every mode, TF_ROLE=all included (decision log #7). Store runs
// claims-scoped, bound to the resume flip's tx (TxStores.RunPendingInput), so
// the input write commits atomically with the status flip under the resuming
// user's claims and a losing concurrent wake rolls it back. Consume runs
// system-side off the top-level (admin-pool in Postgres) store: the
// dispatcher's claim path is a goroutine with no request context.
type RunPendingInputStore interface {
	// Store durably records the message to deliver on the run's next
	// claim, replacing any not-yet-consumed prior input for the same run —
	// an idempotent upsert keyed by run_id (a retried "requeue failed"
	// request just overwrites its own prior write with an equal one).
	Store(ctx context.Context, orgID, runID, userID, message string) error

	// Consume atomically deletes and returns the pending input for a run
	// (DELETE ... RETURNING), or ok=false when none exists. Called once, at
	// the top of the claiming executor's resume path — exactly like
	// staged_agent_injections' flush, delivered exactly once.
	Consume(ctx context.Context, orgID, runID string) (message, userID string, ok bool, err error)
}
