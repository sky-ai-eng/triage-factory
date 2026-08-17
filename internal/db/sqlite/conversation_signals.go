package sqlite

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// runSignalStore is the SQLite stub for db.ConversationSignalStore. conversation_signals is
// Postgres-only (see internal/db/postgres/conversation_signals.go's doc comment):
// local mode is always its own run's owner (TF_ROLE=all is one process),
// so no code path may ever reach this store — internal/delegate only
// constructs the cross-pod RunController in multi mode. Every method
// returns ErrNotApplicableInLocal, mirroring
// internal/db/sqlite/marketplace_store.go. Wired purely to keep db.Stores
// fully populated in both modes.
type runSignalStore struct{}

func newConversationSignalStore() db.ConversationSignalStore { return runSignalStore{} }

var _ db.ConversationSignalStore = runSignalStore{}

func (runSignalStore) Insert(context.Context, string, string, domain.ConversationSignalKind, string, string) (int64, error) {
	return 0, db.ErrNotApplicableInLocal
}

func (runSignalStore) ListUnackedForTarget(context.Context, string) ([]domain.ConversationSignal, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (runSignalStore) Ack(context.Context, int64, string) error {
	return db.ErrNotApplicableInLocal
}

func (runSignalStore) AckStatus(context.Context, int64) (bool, string, error) {
	return false, "", db.ErrNotApplicableInLocal
}

func (runSignalStore) PurgeAcked(context.Context, time.Duration) (int, error) {
	return 0, db.ErrNotApplicableInLocal
}
