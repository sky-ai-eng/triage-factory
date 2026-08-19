package sqlite

import (
	"context"
	"errors"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// swipeStore is the SQLite impl of db.SwipeStore. SQL bodies are
// ported verbatim from the pre-D2 internal/db/swipes.go; behavioral
// changes are:
//
//   - assertLocalOrg at every method entry,
//   - context propagation on every Exec/Begin,
//   - inTx wraps the multi-statement methods so a partial
//     swipe_events INSERT + tasks UPDATE can't strand the row.
type swipeStore struct{ q queryer }

func newSwipeStore(q queryer) db.SwipeStore { return &swipeStore{q: q} }

var _ db.SwipeStore = (*swipeStore)(nil)

func (s *swipeStore) RecordSwipe(ctx context.Context, orgID string, taskID, action string, hesitationMs int, snoozeUntil *time.Time) (string, bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", false, err
	}
	// Action → effect mapping. The responsibility axis
	// (who owns this) is split off the lifecycle axis (where in its life the
	// task is). claim + delegate + reassign (TFAC-561) are
	// responsibility-only — the handler stamps claim columns and may
	// already have moved the lifecycle status (e.g. snooze-wake inside
	// ClaimQueuedForUser). This audit path MUST NOT write status for
	// those actions or it would clobber the lifecycle status of an
	// in_progress / in_review task during a takeover/delegate/reassign
	// (the assignee picker exercises all three).
	// Only dismiss + complete are genuine lifecycle moves recorded
	// here; snooze flows through SnoozeTask separately.
	//
	// closed_at + close_reason are written on terminal swipes
	// so the Board's Done-column 7-day cap actually applies. They're
	// NOT cleared on claim/delegate because every task-action route
	// refuses on a terminal task at the entry — so a row reaching this
	// path with stale close metadata isn't a state the handlers
	// permit. Re-open paths (RequeueTask / UndoLastSwipe) clear the
	// close columns explicitly.
	terminal := action == "dismiss" || action == "complete"
	var newStatus, closeReason string
	// snoozeVal is what lands in tasks.snooze_until: the caller's wake
	// time on a snooze, NULL on every other action (each of which is a
	// transition out of a snooze, if the row was in one).
	var snoozeVal any
	switch action {
	case "claim", "delegate", "reassign":
		// Audit-only path — read the current status to return so the
		// caller's WS broadcast carries the right value.
	case "dismiss":
		newStatus = "dismissed"
		closeReason = "user_dismissed"
	case "complete":
		newStatus = "done"
		closeReason = "user_completed"
	case "snooze":
		// The snooze arm. It must write a real wake time — the queue's
		// wake sweep requeues on snooze_until, so a snoozed row without
		// one never comes back. Refuse rather than write that state.
		if snoozeUntil == nil {
			return "", false, db.ErrSnoozeUntilRequired
		}
		newStatus = "snoozed"
		snoozeVal = snoozeUntil.UTC()
	default:
		// Unknown action — same fallback as before, write 'queued'.
		newStatus = "queued"
	}
	err := inTx(ctx, s.q, func(q queryer) error {
		if _, err := q.ExecContext(ctx,
			`INSERT INTO swipe_events (task_id, action, hesitation_ms) VALUES (?, ?, ?)`,
			taskID, action, hesitationMs,
		); err != nil {
			return err
		}
		if newStatus == "" {
			// claim / delegate: preserve in_progress / in_review across
			// takeover, but flip 'snoozed' → 'queued' so the
			// "snoozed ↔ unclaimed" invariant holds even when a code
			// path bypasses the claim helpers (which do the wake
			// atomically under normal operation). The CASE expression
			// keeps every non-snoozed status intact — load-bearing for
			// the assignee picker's take-over-from-bot path, which
			// hits an in_progress / in_review row.
			res, err := q.ExecContext(ctx,
				`UPDATE tasks
				   SET status = CASE WHEN status = 'snoozed' THEN 'queued' ELSE status END,
				       snooze_until = NULL
				 WHERE id = ?
				   AND status NOT IN ('done', 'dismissed')`,
				taskID,
			)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if n == 0 {
				return errTerminalRefused
			}
			// Read-back so the caller's WS broadcast carries the
			// actual post-mutation status.
			row := q.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, taskID)
			return row.Scan(&newStatus)
		}
		var closedAt any
		var reason any
		if terminal {
			closedAt = time.Now().UTC()
			reason = closeReason
		}
		// The status predicate is what makes a second close a no-op
		// rather than a rewrite: two callers closing the same task
		// concurrently both pass their own pre-read, and only the one
		// that gets here first finds a row to update.
		res, err := q.ExecContext(ctx,
			`UPDATE tasks
			   SET status = ?,
			       snooze_until = ?,
			       closed_at = ?,
			       close_reason = ?
			 WHERE id = ?
			   AND status NOT IN ('done', 'dismissed')`,
			newStatus, snoozeVal, closedAt, reason, taskID,
		)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return errTerminalRefused
		}
		return nil
	})
	if errors.Is(err, errTerminalRefused) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return newStatus, true, nil
}

// errTerminalRefused signals RecordSwipe's status predicate refused: the task
// closed between the caller's read and this write. Distinct from a real DB
// error so RecordSwipe can return ("", false, nil) while still triggering
// inTx's deferred rollback for the audit row it already inserted.
var errTerminalRefused = errors.New("sqlite swipes: task is closed")

func (s *swipeStore) SnoozeTask(ctx context.Context, orgID string, taskID string, until time.Time, hesitationMs int) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	var ok bool
	err := inTx(ctx, s.q, func(q queryer) error {
		// Audit row first so a refused snooze rolls both back as a
		// unit via the tx abort. If we wrote audit on refuse we'd
		// leave an "attempted snooze" log entry plus zero state
		// change — not useful, and inconsistent with the
		// "refused gesture leaves no trace" semantic the helper
		// callers depend on.
		if _, err := q.ExecContext(ctx,
			`INSERT INTO swipe_events (task_id, action, hesitation_ms) VALUES (?, 'snooze', ?)`,
			taskID, hesitationMs,
		); err != nil {
			return err
		}
		// Claim guard: snooze is queue-only post-invariant. A
		// claimed task being snoozed would create a state that no
		// flow in the code path knows how to handle correctly
		// (drain skips it, re-derive skips it, Board doesn't
		// render the SnoozedBadge in a claimed lane). Refuse here;
		// caller surfaces 409 and the user can requeue first.
		//
		// The status predicate is the other half: a closed task can have
		// both claim columns NULL, so without it a dismiss landing between
		// the caller's read and this write would park a closed task in the
		// snoozed lane for the wake sweep to resurrect.
		res, err := q.ExecContext(ctx,
			`UPDATE tasks
			    SET status = 'snoozed', snooze_until = ?
			  WHERE id = ?
			    AND claimed_by_agent_id IS NULL
			    AND claimed_by_user_id  IS NULL
			    AND status NOT IN ('done', 'dismissed')`,
			until.UTC(), taskID,
		)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			// Refused: roll the audit row back too via the sentinel.
			return errSnoozeRefused
		}
		ok = true
		return nil
	})
	if errors.Is(err, errSnoozeRefused) {
		return false, nil
	}
	return ok, err
}

// errSnoozeRefused signals the snooze-on-claimed-task guard tripped.
// Distinct from a real DB error so SnoozeTask can return (false, nil)
// while still triggering inTx's deferred rollback for the audit row.
var errSnoozeRefused = errors.New("sqlite swipes: snooze refused (task is claimed)")

func (s *swipeStore) RequeueTask(ctx context.Context, orgID string, taskID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	var ok bool
	err := inTx(ctx, s.q, func(q queryer) error {
		// Requeue clears both claim cols too — putting a
		// task back in the team's triage queue means it's no longer
		// claimed by anyone (the derived queue filter requires both
		// claim cols NULL).
		// Also clear close metadata — re-queueing a
		// previously-terminal task means it isn't terminal anymore,
		// and the Board's Done-column 7-day cap reads closed_at to
		// gate visibility.
		res, err := q.ExecContext(ctx,
			`UPDATE tasks
			    SET status = 'queued',
			        snooze_until = NULL,
			        claimed_by_agent_id = NULL,
			        claimed_by_user_id  = NULL,
			        closed_at = NULL,
			        close_reason = NULL
			  WHERE id = ?`,
			taskID,
		)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		ok = n > 0
		return nil
	})
	return ok, err
}

func (s *swipeStore) ClearSnooze(ctx context.Context, orgID string, taskID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	// Status predicate in the WHERE clause, not a read-then-write: the
	// refusal and the write are then one statement, so a concurrent
	// requeue or dismiss can't land between the check and the update.
	res, err := s.q.ExecContext(ctx,
		`UPDATE tasks
		    SET status = 'queued',
		        snooze_until = NULL
		  WHERE id = ?
		    AND status = 'snoozed'`,
		taskID,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *swipeStore) UndoLastSwipe(ctx context.Context, orgID string, taskID, userID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	var ok bool
	err := inTx(ctx, s.q, func(q queryer) error {
		// Undo reverses the caller's own last gesture, and only while that
		// gesture is still the last thing that happened to the task. Both
		// halves matter: "mine" alone would let a stale gesture reverse
		// somebody else's later dismiss (gestures don't expire, and
		// RequeueTask deliberately logs nothing, so a released claim stays
		// a user's newest row indefinitely), and "not already undone"
		// alone would let a second undo re-reverse.
		//
		// The predicate rides the UPDATE rather than a read before it, so
		// the check and the write are one statement: two undos racing on
		// the same task can't both pass. The audit row goes in AFTER, or
		// it would be the newest row and shadow the very thing being
		// tested. A task with no gestures at all reads NULL here, which
		// fails the comparison and refuses — the same answer.
		//
		// Undo mirrors requeue's full reset — claim cols also clear. A
		// claim/delegate gesture stamps the relevant claim col; the claim
		// col left on the row would keep the task in the owner's lane even
		// after status returns to 'queued'. Clear both cols so the task
		// lands back in the team's unclaimed triage queue, the same shape
		// /requeue produces. Close metadata clears too — undoing a dismiss
		// or complete means the task isn't terminal anymore.
		res, err := q.ExecContext(ctx,
			`UPDATE tasks
			    SET status = 'queued',
			        snooze_until = NULL,
			        claimed_by_agent_id = NULL,
			        claimed_by_user_id  = NULL,
			        closed_at = NULL,
			        close_reason = NULL
			  WHERE id = ?
			    AND (SELECT creator_user_id FROM swipe_events
			          WHERE task_id = ? ORDER BY id DESC LIMIT 1) = ?
			    AND (SELECT action FROM swipe_events
			          WHERE task_id = ? ORDER BY id DESC LIMIT 1) <> 'undo'`,
			taskID, taskID, userID, taskID,
		)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return errNothingToUndo
		}
		if _, err := q.ExecContext(ctx,
			`INSERT INTO swipe_events (task_id, action) VALUES (?, 'undo')`,
			taskID,
		); err != nil {
			return err
		}
		ok = true
		return nil
	})
	if errors.Is(err, errNothingToUndo) {
		return false, nil
	}
	return ok, err
}

// errNothingToUndo signals UndoLastSwipe's guard tripped: the task's newest
// gesture isn't the caller's, or is itself an undo. Distinct from a real DB
// error so UndoLastSwipe can return (false, nil) while still triggering
// inTx's deferred rollback.
var errNothingToUndo = errors.New("sqlite swipes: the task's newest gesture isn't the caller's to undo")
