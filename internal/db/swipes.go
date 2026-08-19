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
// Atomicity: every mutating method writes the swipe_events row AND
// updates the corresponding task in a single transaction — a partial
// state ("status updated but no audit row" or vice versa) would
// break the Board's undo flow and the analytics views.
type SwipeStore interface {
	// RecordSwipe inserts a swipe_events row and transitions the
	// task's lifecycle (status + snooze_until) in one tx. Action
	// → status mapping: "dismiss" → dismissed, "complete" → done,
	// "snooze" → snoozed (with snoozeUntil as the wake time);
	// "claim", "delegate" and "reassign" map to status='queued'
	// — those are responsibility-axis actions and don't move
	// lifecycle, but the no-op coercion is kept for defensive
	// idempotency. Unknown action defaults to 'queued' so a
	// misuse doesn't silently strand the task. Returns the new
	// task status the handler echoes back in the JSON response.
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
	RecordSwipe(ctx context.Context, orgID string, taskID, action string, hesitationMs int, snoozeUntil *time.Time) (string, error)

	// SnoozeTask is the snooze-specific swipe: writes a 'snooze'
	// swipe_events row + sets tasks.snooze_until and
	// tasks.status='snoozed'. Separate from RecordSwipe because the
	// timestamp parameter has no other use and the action is fixed.
	//
	// Invariant: snooze is queue-only ("snoozed ↔ both
	// claim cols NULL"). The UPDATE refuses on a claimed task and
	// returns ok=false; the audit row is rolled back atomically so
	// a refused gesture leaves no state change. Handler maps
	// ok=false to 409.
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

	// UndoLastSwipe reverses the caller's own last gesture on a task:
	// it writes an 'undo' swipe_events row + flips the task back to
	// 'queued' with snooze_until cleared. The undo toast maps to this;
	// the audit row makes the undo itself visible in the swipe-history
	// view.
	//
	// It is scoped to userID's own audit rows and refuses (ok=false,
	// nothing written) when the caller's most recent row on this task
	// is absent or is itself an 'undo'. Without that guard the route is
	// a force-reset for any task the caller can see, and a second undo
	// re-reverses a gesture that was already reversed. "Put this back
	// in the queue" as a deliberate state change is RequeueTask.
	UndoLastSwipe(ctx context.Context, orgID string, taskID, userID string) (ok bool, err error)
}

// ErrSnoozeUntilRequired is returned by RecordSwipe when action is
// "snooze" but no wake time was supplied. The wake sweep requeues on
// snooze_until, so a snoozed row without one is parked forever —
// every dialect refuses the write rather than persisting it.
var ErrSnoozeUntilRequired = errors.New("db: snooze requires a wake time")
