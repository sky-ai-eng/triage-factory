package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// runPendingInputStore is the Postgres impl of db.RunPendingInputStore —
// the durable half of resume-by-enqueue (TFAC-585). Reachable two ways: the
// resume flip binds it to the claims tx so Store commits atomically with the
// status flip under the resuming user's claims (the RLS policy admits the
// write via the run's own visibility); Consume runs off the admin pool from
// the dispatcher's claim path, a goroutine with no request context.
type runPendingInputStore struct{ admin queryer }

func newRunPendingInputStore(admin queryer) db.RunPendingInputStore {
	return &runPendingInputStore{admin: admin}
}

var _ db.RunPendingInputStore = (*runPendingInputStore)(nil)

func (s *runPendingInputStore) Store(ctx context.Context, orgID, runID, userID, message string) error {
	_, err := s.admin.ExecContext(ctx, `
		INSERT INTO run_pending_input (run_id, org_id, user_id, message, created_at)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, now())
		ON CONFLICT (run_id) DO UPDATE SET
			org_id = EXCLUDED.org_id,
			user_id = EXCLUDED.user_id,
			message = EXCLUDED.message,
			created_at = EXCLUDED.created_at
	`, runID, orgID, userID, message)
	return err
}

func (s *runPendingInputStore) Peek(ctx context.Context, orgID, runID string) (string, string, bool, error) {
	var message, userID string
	err := s.admin.QueryRowContext(ctx, `
		SELECT message, user_id::text FROM run_pending_input
		WHERE org_id = $1::uuid AND run_id = $2::uuid
	`, orgID, runID).Scan(&message, &userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return message, userID, true, nil
}

func (s *runPendingInputStore) Consume(ctx context.Context, orgID, runID string) (string, string, bool, error) {
	var message, userID string
	err := s.admin.QueryRowContext(ctx, `
		DELETE FROM run_pending_input
		WHERE org_id = $1::uuid AND run_id = $2::uuid
		RETURNING message, user_id::text
	`, orgID, runID).Scan(&message, &userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return message, userID, true, nil
}
