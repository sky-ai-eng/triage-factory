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
	// done/dismissed since the caller read it — and writes nothing,
	// audit row included. Handler maps ok=false to 409.
	SnoozeTask(ctx context.Context, orgID string, taskID string, until time.Time, hesitationMs int) (ok bool, err error)

	// RequeueTask sends a task back to the queue WITHOUT recording a
	// swipe_events row. Used by drag-to-Queue and the "Return to
	// queue" button — gestures that aren't swipes, so the audit log
	// stays a clean view of swipe-card decisions. Returns ok=false
	// when no row matched (the public /requeue endpoint maps that
	// to 404 instead of a silent 200 on a bogus id). It clears the
	// claim and the source_status the claim recorded with it, and any
	// held close: putting the task back in the queue overrides the event
	// that resolved it, as reopening a closed task does, and the requeue's
	// handler ends the conversations the close waited on.
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
	// that gesture is the caller's and is still the last thing that
	// happened to the task: it writes an 'undo' swipe_events row + flips
	// the task back to 'queued' with snooze_until cleared. The undo toast
	// maps to this; the audit row makes the undo itself visible in the
	// swipe-history view.
	//
	// The guard has three parts, all on the task's NEWEST swipe_events
	// row, whoever wrote it. That row must belong to userID: not the
	// weaker "userID has some gesture here", because gestures never
	// expire and RequeueTask deliberately writes none, so a released
	// claim stays a user's own newest row indefinitely, and a caller
	// holding one could otherwise reverse an action somebody else took
	// afterwards. Its action must be one undo can reverse, which 'undo'
	// is not, so a second undo can't re-reverse a gesture already
	// reversed. And the task must still be in the state that action left
	// (UndoableGestureStateSQL): only gestures write swipe_events, so a
	// close, an automated agent claim, or a wake that landed after the
	// caller's gesture leaves that gesture the newest row, and only the
	// task's own state shows it is no longer the last thing that
	// happened. Without that part an undo would reopen a task an event
	// closed, or take a task off the agent that claimed it, and record
	// the reversal as an undo of something else. "Put this back in the
	// queue" as a deliberate state change is RequeueTask.
	//
	// The guard rides the UPDATE rather than a read before it, so the
	// check and the write are one statement: two undos racing on one task
	// can't both pass, and a change landing after the handler's own reads
	// and write gate still refuses the undo. The audit row goes in after,
	// or it would be the newest row and shadow the thing being tested.
	// Implementations need to read rows the requesting user did not
	// write, which is why the Postgres one runs on the admin pool — see
	// its own comment. Handlers map ok=false to 409. Like RequeueTask, a
	// successful undo clears the claim and its source_status, and any
	// held close.
	UndoLastSwipe(ctx context.Context, orgID string, taskID, userID string) (ok bool, err error)
}

// The close_reason values a dismiss and a complete gesture stamp. Nothing else
// writes them, which is what lets UndoableGestureStateSQL tell a task the
// caller closed from one an event, a run, or a merge closed after them.
const (
	UserDismissedCloseReason = "user_dismissed"
	UserCompletedCloseReason = "user_completed"
)

// UndoableGestureStateSQL renders UndoLastSwipe's state guard, one text for
// both dialects: the state each gesture an undo can reverse leaves the task
// in, as a predicate over the tasks row the undo's UPDATE targets. actionExpr
// is the newest swipe_events row's action and callerExpr the caller's user id,
// each as the dialect spells it. An action with no arm, 'undo' among them,
// refuses, and so does a NULL from any arm.
//
// A dismiss or a complete leaves the task closed with its own close_reason,
// so the undo refuses once the task has been reopened, or closed by anything
// else, since. A snooze leaves it snoozed, so a wake since refuses. A claim
// leaves it open and held by the caller, and a reassign open and held by a
// person, so an agent claim, a close, or a requeue since refuses.
//
// A delegate leaves it open and held by an agent, which is also what an
// automated agent claim leaves; the row records nothing that tells the two
// apart, so an automated claim that lands after the task was put back in the
// queue passes for the caller's delegate. A held close moves no status or
// claim, so one that an event left after the caller's gesture passes too, and
// the undo clears it.
func UndoableGestureStateSQL(actionExpr, callerExpr string) string {
	const open = `tasks.status NOT IN ('done', 'dismissed')`
	// TODO(TFAC-1379): refuse an undo when an automated agent claim or a held
	// close landed after the caller's gesture; both need the task to record
	// which change came last, which no column does.
	return `CASE ` + actionExpr + `
		WHEN 'dismiss'  THEN tasks.status = 'dismissed' AND tasks.close_reason = '` + UserDismissedCloseReason + `'
		WHEN 'complete' THEN tasks.status = 'done' AND tasks.close_reason = '` + UserCompletedCloseReason + `'
		WHEN 'snooze'   THEN tasks.status = 'snoozed'
		WHEN 'claim'    THEN ` + open + ` AND tasks.claimed_by_user_id = ` + callerExpr + `
		WHEN 'reassign' THEN ` + open + ` AND tasks.claimed_by_user_id IS NOT NULL
		WHEN 'delegate' THEN ` + open + ` AND tasks.claimed_by_agent_id IS NOT NULL
		ELSE FALSE
	END`
}

// ErrSnoozeUntilRequired is returned by RecordSwipe when action is
// "snooze" but no wake time was supplied. The wake sweep requeues on
// snooze_until, so a snoozed row without one is parked forever —
// every dialect refuses the write rather than persisting it.
var ErrSnoozeUntilRequired = errors.New("db: snooze requires a wake time")
