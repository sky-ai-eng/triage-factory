package sqlite

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// runCredentialsStore is the SQLite impl of db.RunCredentialsStore. The
// sealed-bundle channel is Postgres-only in substance — local mode (forced
// role=all) reads the live secret store directly and never parks a run in
// awaiting_credentials — so the SQLite schema carries no claim_credentials
// table and this impl refuses every call.
type runCredentialsStore struct{}

func newRunCredentialsStore(_ queryer) db.RunCredentialsStore {
	return runCredentialsStore{}
}

var _ db.RunCredentialsStore = runCredentialsStore{}

func (runCredentialsStore) Put(ctx context.Context, orgID, runID, executorID string, bootEpoch int64, sealed []byte) error {
	return db.ErrNotApplicableInLocal
}

func (runCredentialsStore) Get(ctx context.Context, orgID, runID string) (string, int64, []byte, bool, error) {
	return "", 0, nil, false, db.ErrNotApplicableInLocal
}
