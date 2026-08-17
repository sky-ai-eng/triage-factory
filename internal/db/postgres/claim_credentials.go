package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// runCredentialsStore is the Postgres impl of db.ClaimCredentialsStore — the
// sealed per-claim credential bundle channel. Admin-pool only:
// claim_credentials carries no app-pool grant at all (see the table's RLS
// comment in the baseline migration), so both the brain's write and the
// executor's read route through the superuser pool. Rows key on the run's
// ACTIVE claim, resolved here from the conversation id the caller holds.
type runCredentialsStore struct{ admin queryer }

func newClaimCredentialsStore(admin queryer) db.ClaimCredentialsStore {
	return &runCredentialsStore{admin: admin}
}

var _ db.ClaimCredentialsStore = (*runCredentialsStore)(nil)

// Put is guarded on boot_epoch so a slow provision can never clobber a
// fresher one: if run X times out under executor A (boot_epoch 1) and is
// reclaimed by executor B (boot_epoch 2) before A's in-flight resolve
// finishes, A's write must not overwrite B's once it lands. The WHERE
// clause makes the UPDATE a no-op whenever the row already carries a
// STRICTLY NEWER boot_epoch than this write's — <=, not <, so a same-epoch
// refresh (the brain's periodic GitHub-token re-mint for the SAME still-
// live claim) still applies. The active-claim resolution adds a second
// layer: a reclaimed run has a NEW active claim, so the stale write keys a
// different row entirely and the fresh claim's bundle is untouched either
// way. A run with no active claim inserts nothing (silent no-op).
func (s *runCredentialsStore) Put(ctx context.Context, orgID, conversationID, executorID string, bootEpoch int64, sealed []byte) error {
	_, err := s.admin.ExecContext(ctx, `
		INSERT INTO claim_credentials (claim_id, org_id, executor_id, boot_epoch, sealed, created_at)
		SELECT cl.id, $2::uuid, $3, $4, $5, now()
		FROM claims cl
		WHERE cl.org_id = $2::uuid AND cl.conversation_id = $1::uuid AND cl.released_at IS NULL
		ON CONFLICT (claim_id) DO UPDATE SET
			org_id      = EXCLUDED.org_id,
			executor_id = EXCLUDED.executor_id,
			boot_epoch  = EXCLUDED.boot_epoch,
			sealed      = EXCLUDED.sealed,
			created_at  = EXCLUDED.created_at
		WHERE claim_credentials.boot_epoch <= EXCLUDED.boot_epoch
	`, conversationID, orgID, executorID, bootEpoch, sealed)
	return err
}

func (s *runCredentialsStore) Get(ctx context.Context, orgID, conversationID string) (db.SealedBundle, bool, error) {
	var b db.SealedBundle
	err := s.admin.QueryRowContext(ctx, `
		SELECT cc.executor_id, cc.boot_epoch, cc.sealed, cc.created_at
		FROM claim_credentials cc
		JOIN claims cl ON cl.id = cc.claim_id
		WHERE cl.org_id = $1::uuid AND cl.conversation_id = $2::uuid AND cl.released_at IS NULL
	`, orgID, conversationID).Scan(&b.ExecutorID, &b.BootEpoch, &b.Sealed, &b.SealedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return db.SealedBundle{}, false, nil
	}
	if err != nil {
		return db.SealedBundle{}, false, err
	}
	return b, true, nil
}
