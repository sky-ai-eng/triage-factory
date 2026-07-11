package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// runCredentialsStore is the Postgres impl of db.RunCredentialsStore — the
// sealed per-run credential bundle channel (TFAC-614). Admin-pool only:
// run_credentials carries no app-pool grant at all (see the table's RLS
// comment in the baseline migration), so both the brain's write and the
// executor's read/delete route through the superuser pool.
type runCredentialsStore struct{ admin queryer }

func newRunCredentialsStore(admin queryer) db.RunCredentialsStore {
	return &runCredentialsStore{admin: admin}
}

var _ db.RunCredentialsStore = (*runCredentialsStore)(nil)

// Put is guarded on boot_epoch so a slow provision can never clobber a
// fresher one: if run X times out under executor A (boot_epoch 1) and is
// reclaimed by executor B (boot_epoch 2) before A's in-flight resolve
// finishes, A's write must not overwrite B's once it lands. The WHERE
// clause makes the UPDATE a no-op whenever the row already carries a
// STRICTLY NEWER boot_epoch than this write's — <=, not <, so a same-epoch
// refresh (the brain's periodic GitHub-token re-mint for the SAME still-
// live claim) still applies. Without this a stale write landing after a
// fresh one would leave run_credentials pointing at a bundle sealed to a
// keypair the current claimant doesn't hold the private half of;
// tryUnsealBundle's epoch check on the READ side means this is self-
// healing either way (the executor just keeps polling), but closing the
// window here means it never happens at all rather than relying on the
// backstop sweep's retry cadence.
func (s *runCredentialsStore) Put(ctx context.Context, orgID, runID, executorID string, bootEpoch int64, sealed []byte) error {
	_, err := s.admin.ExecContext(ctx, `
		INSERT INTO run_credentials (run_id, org_id, executor_id, boot_epoch, sealed, created_at)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, now())
		ON CONFLICT (run_id) DO UPDATE SET
			org_id      = EXCLUDED.org_id,
			executor_id = EXCLUDED.executor_id,
			boot_epoch  = EXCLUDED.boot_epoch,
			sealed      = EXCLUDED.sealed,
			created_at  = EXCLUDED.created_at
		WHERE run_credentials.boot_epoch <= EXCLUDED.boot_epoch
	`, runID, orgID, executorID, bootEpoch, sealed)
	return err
}

func (s *runCredentialsStore) Get(ctx context.Context, orgID, runID string) (string, int64, []byte, bool, error) {
	var executorID string
	var bootEpoch int64
	var sealed []byte
	err := s.admin.QueryRowContext(ctx, `
		SELECT executor_id, boot_epoch, sealed FROM run_credentials
		WHERE org_id = $1::uuid AND run_id = $2::uuid
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
	res, err := s.admin.ExecContext(ctx, `
		DELETE FROM run_credentials WHERE org_id = $1::uuid AND run_id = $2::uuid
	`, orgID, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
