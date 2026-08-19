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
// the only consumer is the Board's swipe-card handler. Keeping the
// surface narrow means the handler imports a 4-method interface
// instead of the full task lifecycle.
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
	// "claim" and "delegate" map to status='queued'
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

	// UndoLastSwipe writes an 'undo' swipe_events row + flips the
	// task back to 'queued' with snooze_until cleared. The Board's
	// undo button maps to this; the audit row makes the undo itself
	// visible in the swipe-history view.
	UndoLastSwipe(ctx context.Context, orgID string, taskID string) (ok bool, err error)
}

// ErrSnoozeUntilRequired is returned by RecordSwipe when action is
// "snooze" but no wake time was supplied. The wake sweep requeues on
// snooze_until, so a snoozed row without one is parked forever —
// every dialect refuses the write rather than persisting it.
var ErrSnoozeUntilRequired = errors.New("db: snooze requires a wake time")
