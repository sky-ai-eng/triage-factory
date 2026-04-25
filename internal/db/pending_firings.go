package db

import (
	"database/sql"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// EnqueuePendingFiring inserts a pending firing for (entity, task, trigger).
// The partial unique index on (task_id, trigger_id) WHERE status='pending'
// enforces dedup: a second enqueue for the same combo while the first is
// still pending becomes a no-op via ON CONFLICT DO NOTHING. Keeping the
// oldest queued_at preserves FIFO fairness — a firing that's been waiting
// longer doesn't get pushed to the back of the line by a duplicate event.
//
// Returns true if a row was newly inserted, false if the conflict path
// fired. Callers can use this to log enqueue vs collapse.
func EnqueuePendingFiring(database *sql.DB, entityID, taskID, triggerID, triggeringEventID string) (bool, error) {
	res, err := database.Exec(`
		INSERT INTO pending_firings (entity_id, task_id, trigger_id, triggering_event_id, status, queued_at)
		VALUES (?, ?, ?, ?, 'pending', ?)
		ON CONFLICT (task_id, trigger_id) WHERE status = 'pending' DO NOTHING
	`, entityID, taskID, triggerID, triggeringEventID, time.Now())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// PopPendingFiringForEntity returns the oldest pending firing for the entity,
// or nil if none. Does not mutate or reserve the row — the drainer is
// responsible for marking the row 'fired' or 'skipped_stale' once it decides
// the outcome. Callers must not assume the read-then-mark sequence is safe on
// its own: concurrent drains can observe the same pending row before either
// marks it. The caller must therefore serialize draining for an entity or use
// transactional/locking handling around the read→decision→mark sequence.
func PopPendingFiringForEntity(database *sql.DB, entityID string) (*domain.PendingFiring, error) {
	row := database.QueryRow(`
		SELECT id, entity_id, task_id, trigger_id, triggering_event_id,
		       status, COALESCE(skip_reason, ''), queued_at, drained_at, fired_run_id
		FROM pending_firings
		WHERE entity_id = ? AND status = 'pending'
		ORDER BY queued_at ASC, id ASC
		LIMIT 1
	`, entityID)
	return scanPendingFiring(row)
}

// MarkPendingFiringFired transitions a pending firing to 'fired' and records
// the run that resulted from it. Called after the drainer successfully
// invokes Spawner.Delegate.
func MarkPendingFiringFired(database *sql.DB, firingID int64, runID string) error {
	_, err := database.Exec(`
		UPDATE pending_firings
		SET status = 'fired', drained_at = ?, fired_run_id = ?
		WHERE id = ? AND status = 'pending'
	`, time.Now(), runID, firingID)
	return err
}

// MarkPendingFiringSkipped transitions a pending firing to 'skipped_stale'
// with a reason describing why the drainer didn't fire it (task closed,
// trigger disabled, breaker tripped, fire-time error). Skipping doesn't
// halt the drain loop — the next pending firing for the entity is still
// considered.
func MarkPendingFiringSkipped(database *sql.DB, firingID int64, reason string) error {
	_, err := database.Exec(`
		UPDATE pending_firings
		SET status = 'skipped_stale', drained_at = ?, skip_reason = ?
		WHERE id = ? AND status = 'pending'
	`, time.Now(), reason, firingID)
	return err
}

// HasActiveAutoRunForEntity returns true if any task on the entity has a
// non-terminal run that was fired by the auto-delegation path
// (trigger_type='event'). Manual delegations are intentionally excluded:
// per the SKY-189 design, manual is fully decoupled from the queue and
// doesn't participate in the per-entity gate. The terminal-state list
// matches HasActiveRunForTask — same semantics, different scope.
func HasActiveAutoRunForEntity(database *sql.DB, entityID string) (bool, error) {
	var count int
	err := database.QueryRow(`
		SELECT COUNT(*) FROM runs r
		JOIN tasks t ON t.id = r.task_id
		WHERE t.entity_id = ?
		  AND r.trigger_type = 'event'
		  AND r.status NOT IN ('completed', 'failed', 'cancelled', 'task_unsolvable', 'pending_approval')
	`, entityID).Scan(&count)
	return count > 0, err
}

// HasPendingFiringForEntity returns true iff the entity has any
// pending_firings row in 'pending' status. Combined with
// HasActiveAutoRunForEntity in EntityCanFireImmediately to enforce FIFO
// drainage — a new event matching a trigger doesn't jump the queue while
// older firings are still waiting.
func HasPendingFiringForEntity(database *sql.DB, entityID string) (bool, error) {
	var count int
	err := database.QueryRow(`
		SELECT COUNT(*) FROM pending_firings
		WHERE entity_id = ? AND status = 'pending'
	`, entityID).Scan(&count)
	return count > 0, err
}

// EntityCanFireImmediately returns true iff a new firing for the entity
// can fire right now without enqueueing — i.e., no active auto run AND
// no pending firings ahead of it. The pending-queue half preserves FIFO
// order: if older firings are waiting, a new one queues behind them rather
// than skipping the line.
func EntityCanFireImmediately(database *sql.DB, entityID string) (bool, error) {
	active, err := HasActiveAutoRunForEntity(database, entityID)
	if err != nil {
		return false, err
	}
	if active {
		return false, nil
	}
	pending, err := HasPendingFiringForEntity(database, entityID)
	if err != nil {
		return false, err
	}
	return !pending, nil
}

// ListPendingFiringsForEntity returns all pending_firings rows for an
// entity in queue order (oldest first), regardless of status. Used by
// debug/audit views to show the queue's full history for an entity.
func ListPendingFiringsForEntity(database *sql.DB, entityID string) ([]domain.PendingFiring, error) {
	rows, err := database.Query(`
		SELECT id, entity_id, task_id, trigger_id, triggering_event_id,
		       status, COALESCE(skip_reason, ''), queued_at, drained_at, fired_run_id
		FROM pending_firings
		WHERE entity_id = ?
		ORDER BY queued_at ASC, id ASC
	`, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.PendingFiring
	for rows.Next() {
		f, err := scanPendingFiringRow(rows)
		if err != nil {
			return nil, err
		}
		if f != nil {
			out = append(out, *f)
		}
	}
	return out, rows.Err()
}

// scanPendingFiring scans a sql.Row into a *domain.PendingFiring. Returns
// (nil, nil) on sql.ErrNoRows so callers can treat "no pending" as a
// non-error empty result.
func scanPendingFiring(row *sql.Row) (*domain.PendingFiring, error) {
	var (
		f          domain.PendingFiring
		drainedAt  sql.NullTime
		firedRunID sql.NullString
	)
	err := row.Scan(
		&f.ID, &f.EntityID, &f.TaskID, &f.TriggerID, &f.TriggeringEventID,
		&f.Status, &f.SkipReason, &f.QueuedAt, &drainedAt, &firedRunID,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if drainedAt.Valid {
		t := drainedAt.Time
		f.DrainedAt = &t
	}
	if firedRunID.Valid {
		s := firedRunID.String
		f.FiredRunID = &s
	}
	return &f, nil
}

// scanPendingFiringRow is the sql.Rows variant of scanPendingFiring.
func scanPendingFiringRow(rows *sql.Rows) (*domain.PendingFiring, error) {
	var (
		f          domain.PendingFiring
		drainedAt  sql.NullTime
		firedRunID sql.NullString
	)
	err := rows.Scan(
		&f.ID, &f.EntityID, &f.TaskID, &f.TriggerID, &f.TriggeringEventID,
		&f.Status, &f.SkipReason, &f.QueuedAt, &drainedAt, &firedRunID,
	)
	if err != nil {
		return nil, err
	}
	if drainedAt.Valid {
		t := drainedAt.Time
		f.DrainedAt = &t
	}
	if firedRunID.Valid {
		s := firedRunID.String
		f.FiredRunID = &s
	}
	return &f, nil
}
