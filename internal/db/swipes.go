package db

import (
	"context"
	"errors"
	"time"
)

//go:generate go run github.com/vektra/mockery/v2 --name=SwipeStore --output=./mocks --case=underscore --with-expecter

// SwipeStore owns the audit-log of user swipe decisions on tasks
// (swipe_events) plus the task-status writes that follow each swipe.
// Logically split from TaskStore because the swipe surface has a
// distinct shape: it's append-only audit + a status transition, and
// its consumers are the task-action routes. Keeping the surface
// narrow means those handlers import this instead of the full task
// lifecycle.
//
// Atomicity: a method that writes both an audit row and a task update
// does so in a single transaction — a partial state ("status updated
// but no audit row" or vice versa) would break the undo flow and the
// analytics views, and it is what lets a refused write leave no trace
// at all. RequeueTask and ClearSnooze are the deliberate exceptions:
// they change task state and write no audit row, because neither is a
// card gesture (see each method).
//
// Where a method refuses on task state, the predicate rides its own
// UPDATE's WHERE clause rather than a check before it. A handler that
// reads the task in one statement and writes in another cannot close a
// state hole — the row can change in between — so these predicates are
// the enforcement and the ok bools report what they refused; a
// handler's pre-read only buys the common case a more specific answer.
// RequeueTask is the one method with nothing to refuse: re-opening
// whatever it is handed is the whole job, so its ok=false means only
// that no such row exists.
type SwipeStore interface {
	// RecordSwipe inserts a swipe_events row and transitions the
	// task's lifecycle (status + snooze_until) in one tx. Action
	// → status mapping: "dismiss" → dismissed, "complete" → done,
	// "snooze" → snoozed (with snoozeUntil as the wake time).
	// "claim", "delegate" and "reassign" are responsibility-axis
	// actions and leave the lifecycle status where it stands — a
	// takeover of an in_progress task keeps it in_progress — except
	// that a snoozed row wakes to 'queued', since "snoozed" and
	// "claimed by somebody" is a state nothing downstream renders or
	// drains. Unknown action defaults to 'queued' so a misuse
	// doesn't silently strand the task. Returns the resulting task
	// status, which the handler echoes back in its JSON response.
	//
	// A closed task takes no swipe: the UPDATE carries its own
	// status predicate, so a row that went done/dismissed since the
	// caller read it returns ok=false with nothing written — audit
	// row included. That predicate is the ONLY thing standing
	// between two concurrent closes and a rewritten closed_at /
	// close_reason; a handler-side pre-read can't be, because the
	// row can go terminal in the window between the two statements.
	// Handlers map ok=false to 409.
	//
	// snoozeUntil is the wake time and is REQUIRED for action
	// "snooze" — a nil pointer there returns
	// ErrSnoozeUntilRequired rather than writing the row, so no
	// caller can persist status='snoozed' with snooze_until NULL
	// (an indefinite snooze the wake sweep never returns to the
	// queue). It is ignored for every other action, which clears
	// snooze_until as before.
	//
	// The audit contract: the handler calls this AFTER
	// any responsibility-axis mutation has accepted (for claim/
	// delegate), so swipe_events only records completed gestures.
	// A refused claim/delegate is never audited. For dismiss/
	// snooze/complete the action IS the state change, so this
	// method is the audit + lifecycle write in one step.
	RecordSwipe(ctx context.Context, orgID string, taskID, action string, hesitationMs int, snoozeUntil *time.Time) (newStatus string, ok bool, err error)

	// SnoozeTask is the snooze-specific swipe: writes a 'snooze'
	// swipe_events row + sets tasks.snooze_until and
	// tasks.status='snoozed'. Separate from RecordSwipe because the
	// timestamp parameter has no other use and the action is fixed.
	//
	// Invariant: snooze is queue-only ("snoozed ↔ both
	// claim cols NULL"), and a closed task is never parked. The
	// UPDATE carries both predicates and returns ok=false when
	// either refuses — a claimed task, or one that went
	// done/dismissed since the caller read it; the audit row is
	// rolled back atomically so a refused gesture leaves no state
	// change. Handler maps ok=false to 409.
	SnoozeTask(ctx context.Context, orgID string, taskID string, until time.Time, hesitationMs int) (ok bool, err error)

	// RequeueTask sends a task back to the queue WITHOUT recording a
	// swipe_events row. Used by drag-to-Queue and the "Return to
	// queue" button — gestures that aren't swipes, so the audit log
	// stays a clean view of swipe-card decisions. Returns ok=false
	// when no row matched (the public /requeue endpoint maps that
	// to 404 instead of a silent 200 on a bogus id).
	RequeueTask(ctx context.Context, orgID string, taskID string) (ok bool, err error)

	// ClearSnooze wakes a snoozed task early: status 'snoozed' →
	// 'queued' with snooze_until cleared, and no swipe_events row —
	// waking is a field write, not a card gesture, the same way
	// RequeueTask is. Guarded on the current status: a clear against a
	// task that isn't snoozed returns ok=false rather than dragging an
	// in_progress or closed row back into the queue. Handler maps
	// ok=false to 409.
	ClearSnooze(ctx context.Context, orgID string, taskID string) (ok bool, err error)

	// UndoLastSwipe reverses the last gesture made on a task, provided
	// that gesture is the caller's: it writes an 'undo' swipe_events
	// row + flips the task back to 'queued' with snooze_until cleared.
	// The undo toast maps to this; the audit row makes the undo itself
	// visible in the swipe-history view.
	//
	// The guard is on the task's NEWEST swipe_events row, whoever wrote
	// it: that row must belong to userID and must not itself be an
	// 'undo'. Both halves are load-bearing, and the first is not the
	// weaker "userID has some gesture here" — gestures never expire and
	// RequeueTask deliberately writes none, so a released claim stays a
	// user's own newest row indefinitely, and a caller holding one could
	// otherwise reverse an action somebody else took afterwards (wiping
	// their closed_at / close_reason, and the task's claim with it).
	// Write access is team-scoped, so that caller need not be anyone in
	// particular. The second half stops a second undo re-reversing a
	// gesture already reversed. "Put this back in the queue" as a
	// deliberate state change is RequeueTask.
	//
	// The guard rides the UPDATE rather than a read before it, so the
	// check and the write are one statement and two undos racing on one
	// task can't both pass; the audit row goes in after, or it would be
	// the newest row and shadow the thing being tested. Implementations
	// need to read rows the requesting user did not write, which is why
	// the Postgres one runs on the admin pool — see its own comment.
	// Handlers map ok=false to 409.
	UndoLastSwipe(ctx context.Context, orgID string, taskID, userID string) (ok bool, err error)
}

// ErrSnoozeUntilRequired is returned by RecordSwipe when action is
// "snooze" but no wake time was supplied. The wake sweep requeues on
// snooze_until, so a snoozed row without one is parked forever —
// every dialect refuses the write rather than persisting it.
var ErrSnoozeUntilRequired = errors.New("db: snooze requires a wake time")
