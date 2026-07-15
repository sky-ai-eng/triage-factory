package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// curatorTurnCredentialsStore is the Postgres impl of
// db.CuratorTurnCredentialsStore — the sealed per-turn credential bundle
// channel, the curator-turn analog of runCredentialsStore.
// Admin-pool only: curator_turn_credentials carries no app-pool grant at all
// (see the table's REVOKE in the baseline migration), so both the brain's
// write and the executor's read/delete route through the superuser pool.
type curatorTurnCredentialsStore struct{ admin queryer }

func newCuratorTurnCredentialsStore(admin queryer) db.CuratorTurnCredentialsStore {
	return &curatorTurnCredentialsStore{admin: admin}
}

var _ db.CuratorTurnCredentialsStore = (*curatorTurnCredentialsStore)(nil)

// Put is guarded on boot_epoch so a slow provision can never clobber a fresher
// one, identical to runCredentialsStore.Put: the WHERE clause makes the UPDATE
// a no-op whenever the row already carries a STRICTLY NEWER boot_epoch than
// this write's — <=, not <, so a same-epoch refresh (the brain's periodic
// re-mint for the SAME still-live turn) still applies.
func (s *curatorTurnCredentialsStore) Put(ctx context.Context, orgID, requestID, executorID string, bootEpoch int64, sealed []byte) error {
	_, err := s.admin.ExecContext(ctx, `
		INSERT INTO curator_turn_credentials (request_id, org_id, executor_id, boot_epoch, sealed, created_at)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, now())
		ON CONFLICT (request_id) DO UPDATE SET
			org_id      = EXCLUDED.org_id,
			executor_id = EXCLUDED.executor_id,
			boot_epoch  = EXCLUDED.boot_epoch,
			sealed      = EXCLUDED.sealed,
			created_at  = EXCLUDED.created_at
		WHERE curator_turn_credentials.boot_epoch <= EXCLUDED.boot_epoch
	`, requestID, orgID, executorID, bootEpoch, sealed)
	return err
}

func (s *curatorTurnCredentialsStore) Get(ctx context.Context, orgID, requestID string) (string, int64, []byte, bool, error) {
	var executorID string
	var bootEpoch int64
	var sealed []byte
	err := s.admin.QueryRowContext(ctx, `
		SELECT executor_id, boot_epoch, sealed FROM curator_turn_credentials
		WHERE org_id = $1::uuid AND request_id = $2::uuid
	`, orgID, requestID).Scan(&executorID, &bootEpoch, &sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil, false, nil
	}
	if err != nil {
		return "", 0, nil, false, err
	}
	return executorID, bootEpoch, sealed, true, nil
}

func (s *curatorTurnCredentialsStore) Delete(ctx context.Context, orgID, requestID string) (bool, error) {
	res, err := s.admin.ExecContext(ctx, `
		DELETE FROM curator_turn_credentials WHERE org_id = $1::uuid AND request_id = $2::uuid
	`, orgID, requestID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
