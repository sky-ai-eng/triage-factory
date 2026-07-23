package db

import "context"

// RunPendingInputStore is the durable half of resume-by-enqueue (TFAC-585):
// the message (+ acting user) recorded before a parked run's continuation
// is re-queued as ordinary claimable work, so a crash between "message
// recorded" and "process spawned" is recoverable by the standard boot
// sweep rather than an ad-hoc path. Stored as an undelivered messages row
// (role='user', subtype='text', delivered=false) — the pending input IS the
// user's next transcript message, awaiting delivery. See
// docs/for-agents/specs/horizontal-scaling/README.md §5.2.
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
	// claim, replacing any not-yet-delivered prior input for the same run —
	// an idempotent replace keyed by the conversation (a retried "requeue
	// failed" request just overwrites its own prior write with an equal
	// one).
	//
	// The at-most-one-undelivered-row-per-conversation contract is enforced
	// here (delete-then-insert) and by the single-flighted resume path, not
	// by a DB constraint: a partial unique index on undelivered user rows
	// cannot exist on the shared messages table, because curator
	// conversations legitimately queue several undelivered user turns.
	Store(ctx context.Context, orgID, runID, userID, message string) error

	// Peek reads the pending input for a run WITHOUT flipping it, or
	// ok=false when none exists. It routes a claim to the resume path; the
	// row is flipped delivered only by Consume, called right before the
	// message is actually delivered — so a crash between the claim and
	// delivery (e.g. during a slow workspace rehydrate) leaves the row for
	// the next claim to re-deliver, rather than losing the message and
	// re-driving the run as an ordinary blueprint step.
	Peek(ctx context.Context, orgID, runID string) (message, userID string, ok bool, err error)

	// Consume atomically flips the pending input delivered and returns it
	// (UPDATE ... RETURNING), or ok=false when none exists. Called right
	// before delivery on the claiming executor's resume path — exactly like
	// the staged-injection flush, delivered exactly once.
	Consume(ctx context.Context, orgID, runID string) (message, userID string, ok bool, err error)
}
