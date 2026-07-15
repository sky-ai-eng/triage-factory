package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// curatorTurnCredentialsStore is the SQLite impl of
// db.CuratorTurnCredentialsStore — the sealed per-turn credential bundle
// channel. Local mode never calls Put (forced role=all, the bundle
// path is executor-role-only); this exists for store-interface + conformance-
// test symmetry with Postgres.
type curatorTurnCredentialsStore struct{ q queryer }

func newCuratorTurnCredentialsStore(q queryer) db.CuratorTurnCredentialsStore {
	return &curatorTurnCredentialsStore{q: q}
}

var _ db.CuratorTurnCredentialsStore = (*curatorTurnCredentialsStore)(nil)

// Put is guarded on boot_epoch, mirroring the Postgres impl and
// RunCredentialsStore.Put: a slow provision must never clobber a fresher one
// written for a later claim of the same turn. <=, not <, so a same-epoch
// refresh still applies.
func (s *curatorTurnCredentialsStore) Put(ctx context.Context, orgID, requestID, executorID string, bootEpoch int64, sealed []byte) error {
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO curator_turn_credentials (request_id, org_id, executor_id, boot_epoch, sealed, created_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(request_id) DO UPDATE SET
			org_id      = excluded.org_id,
			executor_id = excluded.executor_id,
			boot_epoch  = excluded.boot_epoch,
			sealed      = excluded.sealed,
			created_at  = excluded.created_at
		WHERE curator_turn_credentials.boot_epoch <= excluded.boot_epoch
	`, requestID, orgID, executorID, bootEpoch, sealed)
	return err
}

func (s *curatorTurnCredentialsStore) Get(ctx context.Context, orgID, requestID string) (string, int64, []byte, bool, error) {
	var executorID string
	var bootEpoch int64
	var sealed []byte
	err := s.q.QueryRowContext(ctx, `
		SELECT executor_id, boot_epoch, sealed FROM curator_turn_credentials
		WHERE org_id = ? AND request_id = ?
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
	res, err := s.q.ExecContext(ctx, `DELETE FROM curator_turn_credentials WHERE org_id = ? AND request_id = ?`, orgID, requestID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
