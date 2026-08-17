package sqlite_test

import (
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestConversationSignalStore_SQLite_EveryMethodReturnsErrNotApplicableInLocal pins
// the Postgres-only stub (TFAC-585): conversation_signals lives in the Postgres
// baseline only — local mode is always its own run's owner, so no code
// path may ever reach this store. Every method must fail loudly rather
// than silently no-op against a table that doesn't exist locally.
func TestConversationSignalStore_SQLite_EveryMethodReturnsErrNotApplicableInLocal(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := t.Context()
	rs := stores.ConversationSignals

	const orgID = runmode.LocalDefaultOrgID
	const conversationID = "some-run"
	const target = "some-executor"

	if _, err := rs.Insert(ctx, orgID, conversationID, domain.ConversationSignalCancel, "", target); !errors.Is(err, db.ErrNotApplicableInLocal) {
		t.Errorf("Insert = %v, want ErrNotApplicableInLocal", err)
	}
	if _, err := rs.ListUnackedForTarget(ctx, target); !errors.Is(err, db.ErrNotApplicableInLocal) {
		t.Errorf("ListUnackedForTarget = %v, want ErrNotApplicableInLocal", err)
	}
	if err := rs.Ack(ctx, 1, domain.ConversationSignalAckOK); !errors.Is(err, db.ErrNotApplicableInLocal) {
		t.Errorf("Ack = %v, want ErrNotApplicableInLocal", err)
	}
	if _, _, err := rs.AckStatus(ctx, 1); !errors.Is(err, db.ErrNotApplicableInLocal) {
		t.Errorf("AckStatus = %v, want ErrNotApplicableInLocal", err)
	}
	if _, err := rs.PurgeAcked(ctx, 0); !errors.Is(err, db.ErrNotApplicableInLocal) {
		t.Errorf("PurgeAcked = %v, want ErrNotApplicableInLocal", err)
	}
}
