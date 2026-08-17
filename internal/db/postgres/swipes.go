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
type swipeStore struct{ q queryer }

func newSwipeStore(q queryer) db.SwipeStore { return &swipeStore{q: q} }

var _ db.SwipeStore = (*swipeStore)(nil)

func (s *swipeStore) RecordSwipe(ctx context.Context, orgID string, taskID, action string, hesitationMs int, snoozeUntil *time.Time) (string, error) {
	// See sqlite/swipes.go for the full rationale. Summary: claim +
	// delegate + reassign (TFAC-561) are responsibility-only and MUST
	// NOT touch the lifecycle status (or in_progress / in_review tasks
	// lose their stage during takeover/delegate/reassign via the
	// assignee picker). Terminal swipes (dismiss / complete) write
	// status + closed_at + close_reason. Snooze writes the caller's
	// wake time; the standalone snooze route goes through SnoozeTask.
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
			return "", db.ErrSnoozeUntilRequired
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
			if _, err := tx.ExecContext(ctx,
				`UPDATE tasks
				    SET status = CASE WHEN status = 'snoozed' THEN 'queued' ELSE status END,
				        snooze_until = NULL
				  WHERE org_id = $1 AND id = $2`,
				orgID, taskID,
			); err != nil {
				return err
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
		_, err := tx.ExecContext(ctx,
			`UPDATE tasks
			    SET status = $1,
			        snooze_until = $2,
			        closed_at = $3,
			        close_reason = $4
			  WHERE org_id = $5 AND id = $6`,
			newStatus, snoozeVal, closedAt, reason, orgID, taskID,
		)
		return err
	}); err != nil {
		return "", err
	}
	return newStatus, nil
}

func (s *swipeStore) SnoozeTask(ctx context.Context, orgID string, taskID string, until time.Time, hesitationMs int) (bool, error) {
	var ok bool
	err := s.runInTx(ctx, func(tx *sql.Tx) error {
		// Audit row first so a refused snooze rolls both back as a
		// unit via tx abort. Mirrors the SQLite impl.
		if err := insertSwipeEvent(ctx, tx, orgID, taskID, "snooze", &hesitationMs); err != nil {
			return err
		}
		// Claim guard: snooze is queue-only post-invariant ("snoozed
		// ↔ both claim cols NULL"). Inline UPDATE so the WHERE
		// clause's claim-column guards live with the snooze write.
		res, err := tx.ExecContext(ctx,
			`UPDATE tasks
			    SET status = 'snoozed', snooze_until = $1
			  WHERE org_id = $2 AND id = $3
			    AND claimed_by_agent_id IS NULL
			    AND claimed_by_user_id  IS NULL`,
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

func (s *swipeStore) UndoLastSwipe(ctx context.Context, orgID string, taskID string) error {
	return s.runInTx(ctx, func(tx *sql.Tx) error {
		if err := insertSwipeEvent(ctx, tx, orgID, taskID, "undo", nil); err != nil {
			return err
		}
		// Undo mirrors requeue's full reset — claim cols
		// also clear. A claim/delegate swipe stamps the relevant
		// claim col; leaving it on the row would keep the task in
		// the owner's lane even after status returns to 'queued'.
		// Clear both cols so the task lands back in the team's
		// unclaimed triage queue, matching RequeueTask's shape.
		// Clear close metadata too — undoing a dismiss /
		// complete swipe means the task isn't terminal anymore.
		_, err := tx.ExecContext(ctx,
			`UPDATE tasks
			    SET status = $1,
			        snooze_until = NULL,
			        claimed_by_agent_id = NULL,
			        claimed_by_user_id  = NULL,
			        closed_at = NULL,
			        close_reason = NULL
			  WHERE org_id = $2 AND id = $3`,
			"queued", orgID, taskID,
		)
		return err
	})
}

// runInTx is the Postgres-side counterpart of sqlite's inTx — opens
// a tx on s.q if it's a *sql.DB, or runs the closure against the
// caller's *sql.Tx if we're already inside WithTx. Lets mutating
// store methods share atomicity-boundary code regardless of
// composition context.
func (s *swipeStore) runInTx(ctx context.Context, fn func(*sql.Tx) error) error {
	switch v := s.q.(type) {
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
