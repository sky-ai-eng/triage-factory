package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// claimCredentialsStore is the Postgres impl of db.ClaimCredentialsStore — the
// sealed per-claim credential bundle channel. Admin-pool only:
// claim_credentials carries no app-pool grant at all (see the table's RLS
// comment in the baseline migration), so both the brain's write and the
// executor's read route through the superuser pool. Rows key on a claim: the
// write names the claim it sealed for, and the read resolves the
// conversation's active one.
type claimCredentialsStore struct{ admin queryer }

func newClaimCredentialsStore(admin queryer) db.ClaimCredentialsStore {
	return &claimCredentialsStore{admin: admin}
}

var _ db.ClaimCredentialsStore = (*claimCredentialsStore)(nil)

// Put addresses the claim the caller read, never the conversation's active
// claim at write time. The caller sealed to the key that claim published, so
// the bundle is that claim's or nobody's: resolving the active claim here
// would let a seal for a claim released mid-provision land under the next
// claim, sealed to a key its sidecar never minted. The guard is the claim's
// row as the caller read it: unreleased, with the executor and boot epoch the
// seal was made for. Any arm that fails inserts nothing.
//
// The source manifest lands on the claims row in the same statement, joined to
// what the upsert RETURNED: a write the guard declined returns no row, so the
// manifest is replaced exactly when the ciphertext it describes is.
func (s *claimCredentialsStore) Put(ctx context.Context, orgID, claimID, executorID string, bootEpoch int64, sealed []byte, sources domain.SourceManifest) error {
	manifest, err := db.EncodeSourceManifest(sources)
	if err != nil {
		return err
	}
	_, err = s.admin.ExecContext(ctx, `
		WITH put AS (
			INSERT INTO claim_credentials (claim_id, org_id, executor_id, boot_epoch, sealed, created_at)
			SELECT cl.id, cl.org_id, $3, $4, $5, now()
			FROM claims cl
			WHERE cl.org_id = $1::uuid AND cl.id = $2::uuid AND cl.released_at IS NULL
			  AND cl.executor_id = $3 AND cl.boot_epoch = $4
			ON CONFLICT (claim_id) DO UPDATE SET
				executor_id = EXCLUDED.executor_id,
				boot_epoch  = EXCLUDED.boot_epoch,
				sealed      = EXCLUDED.sealed,
				created_at  = EXCLUDED.created_at
			RETURNING claim_id
		)
		UPDATE claims SET source_manifest = $6::jsonb
		FROM put
		WHERE claims.id = put.claim_id
	`, orgID, claimID, executorID, bootEpoch, sealed, manifest)
	return err
}

func (s *claimCredentialsStore) Get(ctx context.Context, orgID, conversationID string) (db.SealedBundle, bool, error) {
	var (
		b       db.SealedBundle
		sources sql.NullString
	)
	err := s.admin.QueryRowContext(ctx, `
		SELECT cc.executor_id, cc.boot_epoch, cc.sealed, cl.source_manifest::text, cc.created_at
		FROM claim_credentials cc
		JOIN claims cl ON cl.id = cc.claim_id
		WHERE cl.org_id = $1::uuid AND cl.conversation_id = $2::uuid AND cl.released_at IS NULL
	`, orgID, conversationID).Scan(&b.ExecutorID, &b.BootEpoch, &b.Sealed, &sources, &b.SealedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return db.SealedBundle{}, false, nil
	}
	if err != nil {
		return db.SealedBundle{}, false, err
	}
	if b.Sources, err = db.DecodeSourceManifest(sources); err != nil {
		return db.SealedBundle{}, false, err
	}
	return b, true, nil
}
