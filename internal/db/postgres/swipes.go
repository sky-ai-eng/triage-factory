package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// swipeStore is the Postgres impl of db.SwipeStore. SQL is fresh
// against D3's schema:
//
//   - swipe_events has org_id + creator_user_id columns NOT NULL,
//     populated via tf.current_user_id() (request-path) or the
//     COALESCE-to-org-owner fallback (system/test-path), same
//     pattern PromptStore.Create uses.
//   - tasks updates include org_id in WHERE as defense in depth
//     alongside RLS — if RLS were ever bypassed the org filter
//     still applies.
//
// Atomicity matches SQLite: each mutating method wraps the
// swipe_events INSERT + tasks UPDATE in a single tx so a partial
// state can't leak.
type swipeStore struct {
	q queryer
	// admin is the RLS-bypassed pool, used by UndoLastSwipe alone. See the
	// rationale on that method: its guard has to see swipe_events rows the
	// requesting user did not write, and swipe_events_select scopes SELECT
	// to creator_user_id = tf.current_user_id().
	admin queryer
}

func newSwipeStore(q, admin queryer) db.SwipeStore { return &swipeStore{q: q, admin: admin} }

var _ db.SwipeStore = (*swipeStore)(nil)

func (s *swipeStore) RecordSwipe(ctx context.Context, orgID string, taskID, action string, hesitationMs int, snoozeUntil *time.Time) (string, bool, error) {
	// See sqlite/swipes.go for the full rationale. Summary: claim +
	// delegate + reassign (TFAC-561) are responsibility-only and MUST
	// NOT touch the lifecycle status (or in_progress / in_review tasks
	// lose their stage during takeover/delegate/reassign via the
	// assignee picker). Terminal swipes (dismiss / complete) write
	// status + closed_at + close_reason. Snooze writes the caller's
	// wake time; the task PATCH's snooze arm goes through SnoozeTask.
	terminal := action == "dismiss" || action == "complete"
	var newStatus, closeReason string
	// snoozeVal is what lands in tasks.snooze_until — the wake time on
	// a snooze, NULL on every other action. See the SQLite mirror.
	var snoozeVal any
	switch action {
	case "claim", "delegate", "reassign":
		// Audit-only path.
	case "dismiss":
		newStatus = "dismissed"
		closeReason = "user_dismissed"
	case "complete":
		newStatus = "done"
		closeReason = "user_completed"
	case "snooze":
		// A snoozed row with no wake time is parked forever — the
		// wake sweep requeues on snooze_until. Refuse rather than
		// write it. See the SQLite mirror for the full rationale.
		if snoozeUntil == nil {
			return "", false, db.ErrSnoozeUntilRequired
		}
		newStatus = "snoozed"
		snoozeVal = *snoozeUntil
	default:
		newStatus = "queued"
	}
	if err := s.runInTx(ctx, func(tx *sql.Tx) error {
		if err := insertSwipeEvent(ctx, tx, orgID, taskID, action, &hesitationMs); err != nil {
			return err
		}
		if newStatus == "" {
			// claim / delegate: preserve in_progress / in_review
			// across takeover, but flip 'snoozed' → 'queued' so the
			// "snoozed ↔ unclaimed" invariant holds when a
			// path bypasses the claim helpers. See SQLite mirror for
			// the full rationale.
			res, err := tx.ExecContext(ctx,
				`UPDATE tasks
				    SET status = CASE WHEN status = 'snoozed' THEN 'queued' ELSE status END,
				        snooze_until = NULL
				  WHERE org_id = $1 AND id = $2
				    AND status NOT IN ('done', 'dismissed')`,
				orgID, taskID,
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
			row := tx.QueryRowContext(ctx,
				`SELECT status FROM tasks WHERE org_id = $1 AND id = $2`,
				orgID, taskID,
			)
			return row.Scan(&newStatus)
		}
		var closedAt any
		var reason any
		if terminal {
			closedAt = time.Now().UTC()
			reason = closeReason
		}
		// The status predicate is what makes a second close a no-op
		// rather than a rewrite — see the SQLite mirror.
		res, err := tx.ExecContext(ctx,
			`UPDATE tasks
			    SET status = $1,
			        snooze_until = $2,
			        closed_at = $3,
			        close_reason = $4
			  WHERE org_id = $5 AND id = $6
			    AND status NOT IN ('done', 'dismissed')`,
			newStatus, snoozeVal, closedAt, reason, orgID, taskID,
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
	}); err != nil {
		if errors.Is(err, errTerminalRefused) {
			return "", false, nil
		}
		return "", false, err
	}
	return newStatus, true, nil
}

// errTerminalRefused signals RecordSwipe's status predicate refused: the task
// closed between the caller's read and this write. Sentinel distinct from real
// DB errors so RecordSwipe can return ("", false, nil) while triggering the
// deferred tx rollback for the audit row it already inserted.
var errTerminalRefused = errors.New("postgres swipes: task is closed")

func (s *swipeStore) SnoozeTask(ctx context.Context, orgID string, taskID string, until time.Time, hesitationMs int) (bool, error) {
	var ok bool
	err := s.runInTx(ctx, func(tx *sql.Tx) error {
		// Audit row first so a refused snooze rolls both back as a
		// unit via tx abort. Mirrors the SQLite impl.
		if err := insertSwipeEvent(ctx, tx, orgID, taskID, "snooze", &hesitationMs); err != nil {
			return err
		}
		// Claim guard: snooze is queue-only post-invariant ("snoozed
		// ↔ both claim cols NULL"), plus the status predicate that keeps
		// a task closed under us out of the snoozed lane. Inline UPDATE
		// so both guards live with the snooze write. See the SQLite
		// mirror for the full rationale.
		res, err := tx.ExecContext(ctx,
			`UPDATE tasks
			    SET status = 'snoozed', snooze_until = $1
			  WHERE org_id = $2 AND id = $3
			    AND claimed_by_agent_id IS NULL
			    AND claimed_by_user_id  IS NULL
			    AND status NOT IN ('done', 'dismissed')`,
			until, orgID, taskID,
		)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
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
// Sentinel distinct from real DB errors so SnoozeTask can return
// (false, nil) while triggering the deferred tx rollback for the
// audit row.
var errSnoozeRefused = errors.New("postgres swipes: snooze refused (task is claimed)")

func (s *swipeStore) RequeueTask(ctx context.Context, orgID string, taskID string) (bool, error) {
	var ok bool
	err := s.runInTx(ctx, func(tx *sql.Tx) error {
		// Requeue puts a task back in the team's triage
		// queue, which means it's no longer claimed by anyone. Clear
		// both claim cols in the same UPDATE so the derived queue
		// filter (claim cols all NULL + status 'queued') picks the
		// row up immediately. Status reset to 'queued' covers the
		// snoozed-back-to-queue path too.
		// Also clear close metadata — re-queueing a
		// previously-terminal task means it isn't terminal anymore.
		res, err := tx.ExecContext(ctx,
			`UPDATE tasks
			    SET status = 'queued',
			        snooze_until = NULL,
			        claimed_by_agent_id = NULL,
			        claimed_by_user_id  = NULL,
			        closed_at = NULL,
			        close_reason = NULL
			  WHERE org_id = $1 AND id = $2`,
			orgID, taskID,
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
	// Status predicate in the WHERE clause, not a read-then-write: the
	// refusal and the write are then one statement, so a concurrent
	// requeue or dismiss can't land between the check and the update.
	res, err := s.q.ExecContext(ctx,
		`UPDATE tasks
		    SET status = 'queued',
		        snooze_until = NULL
		  WHERE org_id = $1 AND id = $2
		    AND status = 'snoozed'`,
		orgID, taskID,
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
	var ok bool
	// The whole method runs on the admin pool, which is the only way its
	// guard can be correct: the guard has to know whether the task's newest
	// gesture belongs to the caller, and swipe_events_select scopes SELECT to
	// the requesting user's own rows — under RLS "the newest row I can see"
	// is always mine, which is exactly the question being asked. Authorizing
	// in Go and then routing around RLS is the same shape
	// ReassignClaimToUserSystem uses: the handler has already resolved the
	// org from session claims, run its viewer gate, and read the task under
	// RLS (so a task the caller can't see 404s before this is reached).
	// org_id stays in every WHERE as defense in depth.
	err := runInTxOn(ctx, s.admin, func(tx *sql.Tx) error {
		// Serialize concurrent undos on this task before either evaluates
		// its guard. Without it two callers can both pass and both write —
		// the row lock plus READ COMMITTED's per-statement snapshot means
		// the second one's guard below sees the first one's committed
		// 'undo' row and refuses.
		if _, err := tx.ExecContext(ctx,
			`SELECT 1 FROM tasks WHERE org_id = $1 AND id = $2 FOR UPDATE`,
			orgID, taskID,
		); err != nil {
			return err
		}
		// Undo reverses the caller's own last gesture, and only while that
		// gesture is still the last thing that happened to the task. See the
		// SQLite mirror for why both halves are load-bearing, why the
		// predicate rides the UPDATE, and why the audit row goes in after.
		res, err := tx.ExecContext(ctx,
			`UPDATE tasks
			    SET status = 'queued',
			        snooze_until = NULL,
			        claimed_by_agent_id = NULL,
			        claimed_by_user_id  = NULL,
			        closed_at = NULL,
			        close_reason = NULL
			  WHERE org_id = $1 AND id = $2
			    AND (SELECT creator_user_id FROM swipe_events
			          WHERE org_id = $1 AND task_id = $2 ORDER BY id DESC LIMIT 1) = $3
			    AND (SELECT action FROM swipe_events
			          WHERE org_id = $1 AND task_id = $2 ORDER BY id DESC LIMIT 1) <> 'undo'`,
			orgID, taskID, userID,
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
		if err := insertSwipeEventAs(ctx, tx, orgID, taskID, "undo", userID, nil); err != nil {
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
// gesture isn't the caller's, or is itself an undo. Sentinel distinct from
// real DB errors so UndoLastSwipe can return (false, nil) while triggering
// the deferred tx rollback.
var errNothingToUndo = errors.New("postgres swipes: the task's newest gesture isn't the caller's to undo")

// runInTx is the Postgres-side counterpart of sqlite's inTx — opens
// a tx on s.q if it's a *sql.DB, or runs the closure against the
// caller's *sql.Tx if we're already inside WithTx. Lets mutating
// store methods share atomicity-boundary code regardless of
// composition context.
func (s *swipeStore) runInTx(ctx context.Context, fn func(*sql.Tx) error) error {
	return runInTxOn(ctx, s.q, fn)
}

// runInTxOn is runInTx against a named queryer, so UndoLastSwipe can open its
// transaction on the admin pool while every other method stays on s.q.
func runInTxOn(ctx context.Context, q queryer, fn func(*sql.Tx) error) error {
	switch v := q.(type) {
	case *sql.Tx:
		return fn(v)
	case *sql.DB:
		tx, err := v.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := fn(tx); err != nil {
			return err
		}
		return tx.Commit()
	default:
		return errors.New("postgres swipes: unexpected queryer type")
	}
}

// insertSwipeEventAs writes an audit row attributed to an explicit user. The
// admin pool has no JWT claims for tf.current_user_id() to resolve, so the
// caller that routed around RLS has to name the creator it already
// authorized — the COALESCE fallback would otherwise stamp the org owner.
func insertSwipeEventAs(ctx context.Context, tx *sql.Tx, orgID, taskID, action, creatorUserID string, hesitationMs *int) error {
	var hesitation any
	if hesitationMs != nil {
		hesitation = *hesitationMs
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO swipe_events (org_id, creator_user_id, task_id, action, hesitation_ms)
		VALUES ($1, $2, $3, $4, $5)
	`, orgID, creatorUserID, taskID, action, hesitation); err != nil {
		return fmt.Errorf("insert swipe_events: %w", err)
	}
	return nil
}

func insertSwipeEvent(ctx context.Context, tx *sql.Tx, orgID, taskID, action string, hesitationMs *int) error {
	// creator_user_id NOT NULL — use tf.current_user_id() (request
	// path) with COALESCE-to-org-owner for the system / test path,
	// same fallback PromptStore.Create uses so all writes share
	// one creator-resolution rule.
	var hesitation any
	if hesitationMs != nil {
		hesitation = *hesitationMs
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO swipe_events (org_id, creator_user_id, task_id, action, hesitation_ms)
		VALUES ($1,
			COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $1)),
			$2, $3, $4)
	`, orgID, taskID, action, hesitation)
	if err != nil {
		return fmt.Errorf("insert swipe_events: %w", err)
	}
	return nil
}
