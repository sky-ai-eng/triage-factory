package sqlite

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// claimCredentialsStore is the SQLite impl of db.ClaimCredentialsStore. The
// sealed-bundle channel is Postgres-only in substance — local mode (forced
// role=all) reads the live secret store directly and never parks a claim in
// awaiting_credentials — so the SQLite schema carries no claim_credentials
// table and this impl refuses every call.
type claimCredentialsStore struct{}

func newClaimCredentialsStore(_ queryer) db.ClaimCredentialsStore {
	return claimCredentialsStore{}
}

var _ db.ClaimCredentialsStore = claimCredentialsStore{}

func (claimCredentialsStore) Put(ctx context.Context, orgID, conversationID, executorID string, bootEpoch int64, sealed []byte) error {
	return db.ErrNotApplicableInLocal
}

func (claimCredentialsStore) Get(ctx context.Context, orgID, conversationID string) (db.SealedBundle, bool, error) {
	return db.SealedBundle{}, false, db.ErrNotApplicableInLocal
}
