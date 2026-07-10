package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// pollReadinessStore is the SQLite impl of db.PollReadinessStore. SQLite is
// N=1 (one process, one org via runmode.LocalDefaultOrgID), so this is a
// storage move from the old in-memory Server/announcer fields, not a
// behavior change.
type pollReadinessStore struct{ q queryer }

func newPollReadinessStore(q queryer) db.PollReadinessStore {
	return &pollReadinessStore{q: q}
}

var _ db.PollReadinessStore = (*pollReadinessStore)(nil)

func (s *pollReadinessStore) MarkRestarted(ctx context.Context, orgID, source string) error {
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO poll_readiness (org_id, source, restarted_at, last_poll_at, announce_pending)
		VALUES (?, ?, ?, NULL, 0)
		ON CONFLICT (org_id, source) DO UPDATE SET
			restarted_at = excluded.restarted_at,
			last_poll_at = NULL
	`, orgID, source, time.Now().UTC())
	return err
}

func (s *pollReadinessStore) MarkPollComplete(ctx context.Context, orgID, source string, startedAt time.Time) error {
	var startedAtParam any
	if !startedAt.IsZero() {
		startedAtParam = startedAt.UTC()
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO poll_readiness (org_id, source, last_poll_at)
		VALUES (?, ?, ?)
		ON CONFLICT (org_id, source) DO UPDATE SET
			last_poll_at = excluded.last_poll_at
		WHERE ? IS NULL OR poll_readiness.restarted_at IS NULL OR ? >= poll_readiness.restarted_at
	`, orgID, source, time.Now().UTC(), startedAtParam, startedAtParam)
	return err
}

func (s *pollReadinessStore) Ready(ctx context.Context, orgID, source string) (bool, error) {
	var restartedAt, lastPollAt sql.NullTime
	err := s.q.QueryRowContext(ctx, `
		SELECT restarted_at, last_poll_at FROM poll_readiness WHERE org_id = ? AND source = ?
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
	res, err := s.q.ExecContext(ctx, `
		UPDATE poll_readiness SET announce_pending = 0
		WHERE org_id = ? AND source = ? AND announce_pending = 1
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
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO poll_readiness (org_id, source, announce_pending)
		VALUES (?, ?, 1)
		ON CONFLICT (org_id, source) DO UPDATE SET announce_pending = 1
	`, orgID, source)
	return err
}
