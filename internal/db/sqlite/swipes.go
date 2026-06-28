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

func (s *swipeStore) RecordSwipe(ctx context.Context, orgID string, taskID, action string, hesitationMs int) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	// Action → effect mapping. SKY-261 B+ split the responsibility axis
	// (who owns this) off the lifecycle axis (where in its life the
	// task is). claim + delegate are responsibility-only — the handler
	// stamps claim columns and may already have moved the lifecycle
	// status (e.g. snooze-wake inside ClaimQueuedForUser). This audit
	// path MUST NOT write status for those actions or it would clobber
	// the lifecycle status of an in_progress / in_review task during a
	// takeover/delegate (SKY-330's assignee picker exercises both).
	// Only dismiss + complete are genuine lifecycle moves recorded
	// here; snooze flows through SnoozeTask separately.
	//
	// SKY-330: closed_at + close_reason are written on terminal swipes
	// so the Board's Done-column 7-day cap actually applies. They're
	// NOT cleared on claim/delegate because the swipe handler refuses
	// claim transitions on terminal tasks at the entry — so a row
	// reaching this path with stale close metadata isn't a state the
	// handler permits. Re-open paths (RequeueTask / UndoLastSwipe)
	// clear the close columns explicitly.
	terminal := action == "dismiss" || action == "complete"
	var newStatus, closeReason string
	switch action {
	case "claim", "delegate":
		// Audit-only path — read the current status to return so the
		// caller's WS broadcast carries the right value.
	case "dismiss":
		newStatus = "dismissed"
		closeReason = "user_dismissed"
	case "complete":
		newStatus = "done"
		closeReason = "user_completed"
	case "snooze":
		// Defensive: the FE routes snoozing through
		// /api/tasks/{id}/snooze → SnoozeTask, but handleSwipe still
		// accepts action='snooze' and falls into the dismiss/snooze/
		// complete branch that calls RecordSwipe. If a request ever
		// lands here, write status='snoozed' so the audit row at least
		// matches the lifecycle write — without this, the default arm
		// below would silently write 'queued' and the snooze "would
		// have happened" without happening. snooze_until stays NULL
		// (handler doesn't pass it through this path), which the FE
		// doesn't produce in practice.
		newStatus = "snoozed"
	default:
		// Unknown action — same fallback as pre-SKY-261, write 'queued'.
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
			// takeover, but flip 'snoozed' → 'queued' so the SKY-261
			// "snoozed ↔ unclaimed" invariant holds even when a code
			// path bypasses the claim helpers (which do the wake
			// atomically under normal operation). The CASE expression
			// keeps every non-snoozed status intact — load-bearing for
			// the assignee picker's take-over-from-bot path, which
			// hits an in_progress / in_review row.
			if _, err := q.ExecContext(ctx,
				`UPDATE tasks
				   SET status = CASE WHEN status = 'snoozed' THEN 'queued' ELSE status END,
				       snooze_until = NULL
				 WHERE id = ?`,
				taskID,
			); err != nil {
				return err
			}
			// Read-back so the caller's WS broadcast carries the
			// actual post-mutation status.
			row := q.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, taskID)
			return row.Scan(&newStatus)
		}
		var closedAt any
		var reason any
		if terminal {
			closedAt = time.Now()
			reason = closeReason
		}
		_, err := q.ExecContext(ctx,
			`UPDATE tasks
			   SET status = ?,
			       snooze_until = NULL,
			       closed_at = ?,
			       close_reason = ?
			 WHERE id = ?`,
			newStatus, closedAt, reason, taskID,
		)
		return err
	})
	if err != nil {
		return "", err
	}
	return newStatus, nil
}

func (s *swipeStore) SnoozeTask(ctx context.Context, orgID string, taskID string, until time.Time, hesitationMs int) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	var ok bool
	err := inTx(ctx, s.q, func(q queryer) error {
		// Audit row first so a refused snooze rolls both back as a
		// unit via the tx abort. If we wrote audit on refuse we'd
		// leave an "attempted snooze" log entry plus zero state
		// change — not useful, and inconsistent with the SKY-261 B+
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
		res, err := q.ExecContext(ctx,
			`UPDATE tasks
			    SET status = 'snoozed', snooze_until = ?
			  WHERE id = ?
			    AND claimed_by_agent_id IS NULL
			    AND claimed_by_user_id  IS NULL`,
			until, taskID,
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
		// SKY-261 B+: Requeue clears both claim cols too — putting a
		// task back in the team's triage queue means it's no longer
		// claimed by anyone (the derived queue filter requires both
		// claim cols NULL).
		// SKY-330: also clear close metadata — re-queueing a
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

func (s *swipeStore) UndoLastSwipe(ctx context.Context, orgID string, taskID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	return inTx(ctx, s.q, func(q queryer) error {
		if _, err := q.ExecContext(ctx,
			`INSERT INTO swipe_events (task_id, action) VALUES (?, 'undo')`,
			taskID,
		); err != nil {
			return err
		}
		// SKY-261 B+: undo mirrors requeue's full reset — claim cols
		// also clear. A claim/delegate swipe stamps the relevant
		// claim col; the post-swipe-handler teardown
		// (teardownTaskArtifacts + spawner.Cancel for the
		// dismiss/complete/claim paths) is the side-effect, but the
		// claim col left on the row would keep the task in the
		// owner's lane even after status returns to 'queued'. Clear
		// both cols so the task lands back in the team's unclaimed
		// triage queue, the same shape /requeue produces.
		// SKY-330: clear close metadata too — undoing a dismiss /
		// complete swipe means the task isn't terminal anymore.
		_, err := q.ExecContext(ctx,
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
		return err
	})
}
