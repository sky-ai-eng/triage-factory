package postgres

import (
	"context"
	"database/sql"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// pollReadinessStore is the Postgres impl of db.PollReadinessStore. Wired
// against the ADMIN (BYPASSRLS) pool in postgres.New: poll_readiness is a
// system table (RLS deny-by-default, REVOKEd from the app roles) — callers
// already hold an authorized orgID (session claims resolved by the
// handler, or the poller's own system context), so there is no per-row RLS
// policy to gate a browsable surface with, same posture as instances.
type pollReadinessStore struct{ admin queryer }

func newPollReadinessStore(admin queryer) db.PollReadinessStore {
	return &pollReadinessStore{admin: admin}
}

var _ db.PollReadinessStore = (*pollReadinessStore)(nil)

func (s *pollReadinessStore) MarkRestarted(ctx context.Context, orgID, source string) error {
	_, err := s.admin.ExecContext(ctx, `
		INSERT INTO poll_readiness (org_id, source, restarted_at, last_poll_at, announce_pending)
		VALUES ($1, $2, now(), NULL, false)
		ON CONFLICT (org_id, source) DO UPDATE SET
			restarted_at = EXCLUDED.restarted_at,
			last_poll_at = NULL
	`, orgID, source)
	return err
}

func (s *pollReadinessStore) MarkPollComplete(ctx context.Context, orgID, source string, startedAt time.Time) error {
	var startedAtParam any
	if !startedAt.IsZero() {
		startedAtParam = startedAt
	}
	_, err := s.admin.ExecContext(ctx, `
		INSERT INTO poll_readiness (org_id, source, last_poll_at)
		VALUES ($1, $2, now())
		ON CONFLICT (org_id, source) DO UPDATE SET
			last_poll_at = EXCLUDED.last_poll_at
		WHERE $3::timestamptz IS NULL
		   OR poll_readiness.restarted_at IS NULL
		   OR $3::timestamptz >= poll_readiness.restarted_at
	`, orgID, source, startedAtParam)
	return err
}

func (s *pollReadinessStore) Ready(ctx context.Context, orgID, source string) (bool, error) {
	var restartedAt, lastPollAt sql.NullTime
	err := s.admin.QueryRowContext(ctx, `
		SELECT restarted_at, last_poll_at FROM poll_readiness WHERE org_id = $1 AND source = $2
	`, orgID, source).Scan(&restartedAt, &lastPollAt)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !lastPollAt.Valid {
		return false, nil
	}
	if !restartedAt.Valid {
		return true, nil
	}
	return lastPollAt.Time.After(restartedAt.Time), nil
}

func (s *pollReadinessStore) TakeAnnouncePending(ctx context.Context, orgID, source string) (bool, error) {
	res, err := s.admin.ExecContext(ctx, `
		UPDATE poll_readiness SET announce_pending = false
		WHERE org_id = $1 AND source = $2 AND announce_pending = true
	`, orgID, source)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *pollReadinessStore) SetAnnouncePending(ctx context.Context, orgID, source string) error {
	_, err := s.admin.ExecContext(ctx, `
		INSERT INTO poll_readiness (org_id, source, announce_pending)
		VALUES ($1, $2, true)
		ON CONFLICT (org_id, source) DO UPDATE SET announce_pending = true
	`, orgID, source)
	return err
}

func (s *pollReadinessStore) LastPollTimes(ctx context.Context, orgID string) (map[string]time.Time, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT source, last_poll_at FROM poll_readiness
		WHERE org_id = $1 AND last_poll_at IS NOT NULL
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var source string
		var at time.Time
		if err := rows.Scan(&source, &at); err != nil {
			return nil, err
		}
		out[source] = at.UTC()
	}
	return out, rows.Err()
}
