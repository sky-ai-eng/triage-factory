package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// runCredentialsStore is the SQLite impl of db.RunCredentialsStore — the
// sealed per-run credential bundle channel (TFAC-614). Local mode never
// calls Put (forced role=all, the bundle path is executor-role-only); this
// exists for store-interface + conformance-test symmetry with Postgres.
type runCredentialsStore struct{ q queryer }

func newRunCredentialsStore(q queryer) db.RunCredentialsStore {
	return &runCredentialsStore{q: q}
}

var _ db.RunCredentialsStore = (*runCredentialsStore)(nil)

func (s *runCredentialsStore) Put(ctx context.Context, orgID, runID, executorID string, bootEpoch int64, sealed []byte) error {
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO run_credentials (run_id, org_id, executor_id, boot_epoch, sealed, created_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(run_id) DO UPDATE SET
			org_id      = excluded.org_id,
			executor_id = excluded.executor_id,
			boot_epoch  = excluded.boot_epoch,
			sealed      = excluded.sealed,
			created_at  = excluded.created_at
	`, runID, orgID, executorID, bootEpoch, sealed)
	return err
}

func (s *runCredentialsStore) Get(ctx context.Context, orgID, runID string) (string, int64, []byte, bool, error) {
	var executorID string
	var bootEpoch int64
	var sealed []byte
	err := s.q.QueryRowContext(ctx, `
		SELECT executor_id, boot_epoch, sealed FROM run_credentials
		WHERE org_id = ? AND run_id = ?
	`, orgID, runID).Scan(&executorID, &bootEpoch, &sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil, false, nil
	}
	if err != nil {
		return "", 0, nil, false, err
	}
	return executorID, bootEpoch, sealed, true, nil
}

func (s *runCredentialsStore) Delete(ctx context.Context, orgID, runID string) (bool, error) {
	res, err := s.q.ExecContext(ctx, `DELETE FROM run_credentials WHERE org_id = ? AND run_id = ?`, orgID, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
