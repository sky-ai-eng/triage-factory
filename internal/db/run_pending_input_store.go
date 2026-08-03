package db

import "context"

// RunPendingInputStore is the durable half of resume-by-enqueue (TFAC-585):
// the message (+ acting user) recorded before a parked run's continuation
// is re-queued as ordinary claimable work, so a crash between "message
// recorded" and "process spawned" is recoverable by the standard boot
// sweep rather than an ad-hoc path. Stored as an undelivered messages row
// (role='user', blank subtype, delivered=false) — the pending input IS the
// user's next transcript message, awaiting delivery. See
// docs/for-agents/specs/horizontal-scaling/README.md §5.2.
//
// Both dialects (unlike RunSignalStore): local mode's dispatcher claims its
// own resumed runs through the identical queue path — resume-by-enqueue
// applies in every mode, TF_ROLE=all included (decision log #7). Store runs
// claims-scoped through TxStores.RunPendingInput, so the input write lands
// under the resuming user's claims. Consume runs system-side off the
// top-level (admin-pool in Postgres) store: the dispatcher's claim path is a
// goroutine with no request context.
//
// The queue APPENDS. Several messages sent to one parked conversation all
// survive and are all delivered — the same thing the native loop does with
// its own undelivered rows, reached here by joining them into the single
// prompt string an SDK resume invocation takes. The runtimes differ in how
// the text arrives, not in what happens to a message someone typed.
//
// Attribution, since a queue with several authors has no single honest one:
// Peek and Consume return the NEWEST row's user, and the delivering claim
// routes its writes under that user's synthetic claims. So a teammate who
// adds to your follow-up owns the turn that carries both. This is not
// derivable from the call site — dispatchResumeClaim just receives a userID —
// which is why the rule lives here.
type RunPendingInputStore interface {
	// Store durably records a message to deliver on the run's next claim,
	// appending to whatever is already queued rather than replacing it.
	//
	// Deliberately NOT idempotent: a retried "requeue failed" request queues
	// its text a second time, and both copies are delivered. That is the
	// accepted cost of the guarantee — a model reading the same sentence
	// twice recovers on its own, and silently deleting something a person
	// typed does not.
	//
	// No DB constraint backs the row shape: a partial unique index on
	// undelivered user rows cannot exist on the shared messages table,
	// because curator conversations legitimately queue several undelivered
	// user turns. The predicate below is the whole definition.
	Store(ctx context.Context, orgID, runID, userID, message string) error

	// Peek reads the queued input for a run WITHOUT flipping it, or
	// ok=false when none exists. It returns exactly the text Consume would
	// — every queued row, oldest first, joined by a blank line — so the
	// routing decision (dispatchClaimedRun) and the delivery
	// (dispatchResumeClaim) cannot disagree about what is pending.
	//
	// Rows are flipped delivered only by Consume, called right before the
	// message is actually delivered — so a crash between the claim and
	// delivery (e.g. during a slow workspace rehydrate) leaves them for the
	// next claim to re-deliver, rather than losing the message and
	// re-driving the run as an ordinary blueprint step.
	Peek(ctx context.Context, orgID, runID string) (message, userID string, ok bool, err error)

	// Consume atomically flips every queued row delivered and returns them
	// joined oldest-first (UPDATE ... RETURNING), or ok=false when none
	// exists. Called right before delivery on the claiming executor's resume
	// path — exactly like the staged-injection flush, delivered exactly once.
	Consume(ctx context.Context, orgID, runID string) (message, userID string, ok bool, err error)
}

// PendingInputSeparator joins the conversation's queued messages into the one
// prompt string an SDK resume takes. A blank line is the single-prompt
// equivalent of the separate user messages the native loop would assemble:
// enough for a model to read two things a person said as two things.
//
// Declared here rather than per-dialect so Peek and Consume cannot drift
// apart across backends — the routing decision and the delivery compare
// their text.
const PendingInputSeparator = "\n\n"
