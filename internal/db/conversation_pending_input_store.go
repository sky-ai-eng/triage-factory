package db

import "context"

// ConversationPendingInputStore is the READ half of resume-by-enqueue (TFAC-585): the
// messages (+ acting user) recorded before a parked conversation's continuation
// is re-queued as ordinary claimable work, so a crash between "message recorded"
// and "process spawned" is recoverable by the standard boot sweep rather than
// an ad-hoc path. See docs/for-agents/specs/horizontal-scaling/README.md §5.2.
//
// It owns no writer, and that is deliberate. The queue is not a side table: it
// is the undelivered tail of the conversation's own transcript (role='user',
// blank subtype, delivered=false, window_state='active'), so a follow-up is
// written by the ordinary transcript insert — ConversationStore.InsertMessage,
// from Spawner.queueFollowUp — and this store only reads and retires those
// rows. A second writer spelling the same INSERT is how the two runtimes drifted
// apart the first time. The predicate below is the whole definition of the
// shape; no DB constraint backs it, because a partial unique index on
// undelivered user rows cannot exist on the shared messages table (a
// conversation legitimately queues several follow-ups while busy).
//
// Both dialects (unlike ConversationSignalStore): local mode's dispatcher claims its
// own resumed conversations through the identical queue path — resume-by-enqueue
// applies in every mode, TF_ROLE=all included (decision log #7). Peek and
// Consume run system-side off the top-level (admin-pool in Postgres) store:
// the dispatcher's claim path is a goroutine with no request context.
//
// The queue APPENDS, because the write does. Several messages sent to one
// parked conversation all survive and are all delivered — the same thing the
// native loop does with its own undelivered rows, reached here by joining them
// into the single prompt string an SDK resume invocation takes. The runtimes
// differ in how the text arrives, not in what happens to a message someone
// typed. Nothing is idempotent either: a retried "requeue failed" request
// queues its text a second time and both copies are delivered, which is the
// accepted cost — a model reading the same sentence twice recovers on its own,
// and silently deleting something a person typed does not.
//
// Attribution, since a queue with several authors has no single honest one:
// Peek and Consume return the NEWEST row's user, and the delivering claim
// routes its writes under that user's synthetic claims. So a teammate who
// adds to your follow-up owns the turn that carries both. This is not
// derivable from the call site — dispatchResumeClaim just receives a userID —
// which is why the rule lives here.
type ConversationPendingInputStore interface {
	// Peek reads the queued input for a conversation WITHOUT flipping it, or
	// ok=false when none exists. It returns exactly the text Consume would
	// — every queued row, oldest first, joined by a blank line — so the
	// routing decision (dispatchClaimedConversation) and the delivery
	// (dispatchResumeClaim) cannot disagree about what is pending.
	//
	// Rows are flipped delivered only by Consume, called right before the
	// message is actually delivered — so a crash between the claim and
	// delivery (e.g. during a slow workspace rehydrate) leaves them for the
	// next claim to re-deliver, rather than losing the message and
	// re-driving the conversation as an ordinary blueprint step.
	Peek(ctx context.Context, orgID, conversationID string) (message, userID string, ok bool, err error)

	// Consume atomically flips every queued row delivered and returns them
	// joined oldest-first (UPDATE ... RETURNING), or ok=false when none
	// exists. Called right before delivery on the claiming executor's resume
	// path — exactly like the staged-injection flush, delivered exactly once.
	Consume(ctx context.Context, orgID, conversationID string) (message, userID string, ok bool, err error)
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
