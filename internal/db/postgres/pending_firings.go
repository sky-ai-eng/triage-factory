package postgres

import (
	"context"
	"database/sql"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// pendingFiringsStore is the Postgres impl of db.PendingFiringsStore.
// Wired against the admin pool in postgres.New: the router has no per-
// user identity and the drain sweeper runs as a background goroutine,
// so impersonating any one user via the app pool would be wrong.
// Defense-in-depth org_id filters fire in every WHERE/INSERT clause
// alongside the RLS policy (which gates by an EXISTS subquery against
// tasks rather than a bare current_org_id() check — the policy is
// kept honest by the explicit filter here in case the policy is ever
// loosened).
//
// The per-entity firing gate's runs-shaped half lives on
// AgentRunStore (HasActiveAutoRunForEntity) — strict ownership. The
// router composes the gate from this store's HasPendingForEntity +
// AgentRunStore's HasActiveAutoRunForEntity.
type pendingFiringsStore struct{ q queryer }

func newPendingFiringsStore(q queryer) db.PendingFiringsStore {
	return &pendingFiringsStore{q: q}
}

var _ db.PendingFiringsStore = (*pendingFiringsStore)(nil)

func (s *pendingFiringsStore) Enqueue(ctx context.Context, orgID, userID, entityID, taskID, triggerID, triggeringEventID string) (bool, error) {
	// creator_user_id is NOT NULL in the Postgres schema. Resolution
	// mirrors AgentRunStore.createManual: prefer the caller-supplied
	// userID, fall back to the org owner. tf.current_user_id() is
	// intentionally skipped — admin-pool inserts run without JWT
	// claims, so the helper would return NULL and the COALESCE would
	// walk straight to org owner anyway.
	//
	// LocalDefaultUserID sentinel handling: the router still passes
	// runmode.LocalDefaultUserID until D9 retrofits handler-
	// level claims. That sentinel UUID has no FK target in a multi-
	// mode users table, so binding it directly would trip
	// pending_firings_creator_user_id_fkey on every busy-entity
	// enqueue. Normalize to empty here so NULLIF collapses to NULL
	// and COALESCE walks to the org-owner fallback. Same shape as
	// AgentRunStore.createManual.
	//
	// queued_at uses the schema default (now()) so the insert and
	// the index agree on the timestamp source — no clock skew between
	// app-side time.Now() and the partial index's FIFO ordering.
	creatorBind := userID
	if creatorBind == runmode.LocalDefaultUserID {
		creatorBind = ""
	}
	res, err := s.q.ExecContext(ctx, `
		INSERT INTO pending_firings
		  (org_id, creator_user_id, entity_id, task_id, trigger_id, triggering_event_id, status)
		VALUES
		  ($1,
		   COALESCE(NULLIF($2, '')::uuid, (SELECT owner_user_id FROM orgs WHERE id = $1)),
		   $3, $4, $5, $6, 'pending')
		ON CONFLICT (task_id, trigger_id) WHERE status = 'pending' DO NOTHING
	`, orgID, creatorBind, entityID, taskID, triggerID, triggeringEventID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *pendingFiringsStore) PopForEntity(ctx context.Context, orgID, entityID string) (*domain.PendingFiring, error) {
	row := s.q.QueryRowContext(ctx, `
		SELECT id, entity_id, task_id, trigger_id, triggering_event_id,
		       status, COALESCE(skip_reason, ''), queued_at, drained_at, fired_run_id
		FROM pending_firings
		WHERE org_id = $1 AND entity_id = $2 AND status = 'pending'
		ORDER BY queued_at ASC, id ASC
		LIMIT 1
	`, orgID, entityID)
	return scanPgPendingFiring(row)
}

func (s *pendingFiringsStore) MarkFired(ctx context.Context, orgID string, firingID int64, runID string) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE pending_firings
		SET status = 'fired', drained_at = now(), fired_run_id = $1
		WHERE org_id = $2 AND id = $3 AND status = 'pending'
	`, runID, orgID, firingID)
	return err
}

func (s *pendingFiringsStore) MarkSkipped(ctx context.Context, orgID string, firingID int64, reason string) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE pending_firings
		SET status = 'skipped_stale', drained_at = now(), skip_reason = $1
		WHERE org_id = $2 AND id = $3 AND status = 'pending'
	`, reason, orgID, firingID)
	return err
}

func (s *pendingFiringsStore) HasPendingForEntity(ctx context.Context, orgID, entityID string) (bool, error) {
	var count int
	err := s.q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pending_firings
		WHERE org_id = $1 AND entity_id = $2 AND status = 'pending'
	`, orgID, entityID).Scan(&count)
	return count > 0, err
}

func (s *pendingFiringsStore) ListEntitiesWithPending(ctx context.Context, orgID string) ([]string, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT DISTINCT entity_id FROM pending_firings
		WHERE org_id = $1 AND status = 'pending'
	`, orgID)
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
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, entity_id, task_id, trigger_id, triggering_event_id,
		       status, COALESCE(skip_reason, ''), queued_at, drained_at, fired_run_id
		FROM pending_firings
		WHERE org_id = $1 AND entity_id = $2
		ORDER BY queued_at ASC, id ASC
	`, orgID, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.PendingFiring{}
	for rows.Next() {
		f, err := scanPgPendingFiringRow(rows)
		if err != nil {
			return nil, err
		}
		if f != nil {
			out = append(out, *f)
		}
	}
	return out, rows.Err()
}

// scanPgPendingFiring scans a sql.Row into *domain.PendingFiring.
// (nil, nil) on sql.ErrNoRows so callers can treat "no pending" as a
// non-error empty result. Mirrors the SQLite-side helper.
func scanPgPendingFiring(row *sql.Row) (*domain.PendingFiring, error) {
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

// scanPgPendingFiringRow is the sql.Rows variant.
func scanPgPendingFiringRow(rows *sql.Rows) (*domain.PendingFiring, error) {
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
