package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// runPendingInputStore is the SQLite impl of db.RunPendingInputStore — the
// durable half of resume-by-enqueue (TFAC-585). SQLite is N=1, no RLS; org_id
// exists for parity with the Postgres baseline and every caller passes
// LocalDefaultOrgID (asserted at each entry), mirroring
// internal/db/sqlite/staged_injections.go.
type runPendingInputStore struct{ q queryer }

func newRunPendingInputStore(q queryer) db.RunPendingInputStore {
	return &runPendingInputStore{q: q}
}

var _ db.RunPendingInputStore = (*runPendingInputStore)(nil)

func (s *runPendingInputStore) Store(ctx context.Context, orgID, runID, userID, message string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO run_pending_input (run_id, org_id, user_id, message, created_at)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT (run_id) DO UPDATE SET
			org_id = excluded.org_id,
			user_id = excluded.user_id,
			message = excluded.message,
			created_at = excluded.created_at
	`, runID, orgID, userID, message)
	return err
}

func (s *runPendingInputStore) Consume(ctx context.Context, orgID, runID string) (string, string, bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", "", false, err
	}
	var message, userID string
	err := s.q.QueryRowContext(ctx, `
		DELETE FROM run_pending_input WHERE org_id = ? AND run_id = ?
		RETURNING message, user_id
	`, orgID, runID).Scan(&message, &userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return message, userID, true, nil
}
