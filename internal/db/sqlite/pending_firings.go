package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// pendingFiringsStore is the SQLite impl of db.PendingFiringsStore.
// SQL bodies are moved verbatim from the pre-D2
// internal/db/pending_firings.go; the only behavioral changes are the
// orgID assertion at each method entry and the ctx-aware database/sql
// methods.
//
// userID is accepted on Enqueue for signature parity with the Postgres
// impl but ignored — the local SQLite schema has no creator column on
// pending_firings.
type pendingFiringsStore struct{ q queryer }

func newPendingFiringsStore(q queryer) db.PendingFiringsStore {
	return &pendingFiringsStore{q: q}
}

var _ db.PendingFiringsStore = (*pendingFiringsStore)(nil)

func (s *pendingFiringsStore) Enqueue(ctx context.Context, orgID, userID, entityID, taskID, triggerID, triggeringEventID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	_ = userID // ignored in local mode
	// The dedup target includes 'draining': a firing mid-drain is still
	// queued intent for (task, trigger), and a duplicate enqueued during
	// the drain window would fire a second run for the same intent as
	// soon as the first one's run terminates.
	res, err := s.q.ExecContext(ctx, `
		INSERT INTO pending_firings (entity_id, task_id, trigger_id, triggering_event_id, status, queued_at)
		VALUES (?, ?, ?, ?, 'pending', ?)
		ON CONFLICT (task_id, trigger_id) WHERE status IN ('pending', 'draining') DO NOTHING
	`, entityID, taskID, triggerID, triggeringEventID, time.Now())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// PopForTask is a claiming pop, mirroring the Postgres impl's shape:
// the UPDATE ... WHERE id = (oldest pending) RETURNING form
// atomically flips the row to 'draining' as it reads it. SQLite/local is
// single-worker (N=1) so there's no concurrent-drain race to close here —
// this exists for interface conformance with the Postgres impl (the shared
// dbtest suite exercises identical claiming semantics against both
// backends) rather than a correctness fix on this side.
func (s *pendingFiringsStore) PopForTask(ctx context.Context, orgID, taskID string) (*domain.PendingFiring, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx, `
		UPDATE pending_firings
		SET status = 'draining', claimed_at = ?
		WHERE id = (
			SELECT id FROM pending_firings
			WHERE task_id = ? AND status = 'pending'
			ORDER BY queued_at ASC, id ASC
			LIMIT 1
		)
		RETURNING id, entity_id, task_id, trigger_id, triggering_event_id,
		          status, COALESCE(skip_reason, ''), queued_at, drained_at, fired_run_id
	`, time.Now(), taskID)
	return scanSqlitePendingFiring(row)
}

func (s *pendingFiringsStore) Release(ctx context.Context, orgID string, firingID int64) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE pending_firings
		SET status = 'pending', claimed_at = NULL
		WHERE id = ? AND status = 'draining'
	`, firingID)
	return err
}

// RequeueStaleDraining is the claiming pop's crash recovery — see the
// interface doc. NULL claimed_at 'draining' rows (claimed before the
// column existed) are recovered unconditionally.
func (s *pendingFiringsStore) RequeueStaleDraining(ctx context.Context, orgID string, before time.Time) (int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return 0, err
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE pending_firings
		SET status = 'pending', claimed_at = NULL
		WHERE status = 'draining'
		  AND (claimed_at IS NULL OR claimed_at < ?)
	`, before)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *pendingFiringsStore) MarkFired(ctx context.Context, orgID string, firingID int64, runID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE pending_firings
		SET status = 'fired', drained_at = ?, fired_run_id = ?
		WHERE id = ? AND status = 'draining'
	`, time.Now(), runID, firingID)
	return err
}

func (s *pendingFiringsStore) MarkSkipped(ctx context.Context, orgID string, firingID int64, reason string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE pending_firings
		SET status = 'skipped_stale', drained_at = ?, skip_reason = ?
		WHERE id = ? AND status = 'draining'
	`, time.Now(), reason, firingID)
	return err
}

func (s *pendingFiringsStore) HasPendingForTask(ctx context.Context, orgID, taskID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	// 'draining' counts as queued intent (see the interface doc): a drain
	// mid-flight must keep the task gate closed or a fresh event in
	// that window jumps the queue.
	var count int
	err := s.q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pending_firings
		WHERE task_id = ? AND status IN ('pending', 'draining')
	`, taskID).Scan(&count)
	return count > 0, err
}

func (s *pendingFiringsStore) ListTasksWithPending(ctx context.Context, orgID string) ([]string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT DISTINCT task_id FROM pending_firings WHERE status = 'pending'
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *pendingFiringsStore) ListForEntity(ctx context.Context, orgID, entityID string) ([]domain.PendingFiring, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
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
	out := []domain.PendingFiring{}
	for rows.Next() {
		f, err := scanSqlitePendingFiringRow(rows)
		if err != nil {
			return nil, err
		}
		if f != nil {
			out = append(out, *f)
		}
	}
	return out, rows.Err()
}

// scanSqlitePendingFiring scans a sql.Row into *domain.PendingFiring.
// (nil, nil) on sql.ErrNoRows so callers can treat "no pending" as a
// non-error empty result.
func scanSqlitePendingFiring(row *sql.Row) (*domain.PendingFiring, error) {
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

// scanSqlitePendingFiringRow is the sql.Rows variant.
func scanSqlitePendingFiringRow(rows *sql.Rows) (*domain.PendingFiring, error) {
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
