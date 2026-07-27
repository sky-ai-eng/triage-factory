package sqlite_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestRunCredentialsStore_SQLite_NotApplicable pins the local-mode posture:
// the sealed-bundle channel is Postgres-only (local mode reads the live
// secret store and never parks a run in awaiting_credentials), the SQLite
// schema carries no claim_credentials table, and the stub refuses loudly
// rather than pretending to store ciphertext.
func TestRunCredentialsStore_SQLite_NotApplicable(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	orgID := runmode.LocalDefaultOrgID
	runID := uuid.New().String()

	if err := stores.RunCredentials.Put(ctx, orgID, runID, "executor-1", 1, []byte("sealed")); !errors.Is(err, db.ErrNotApplicableInLocal) {
		t.Fatalf("Put = %v, want ErrNotApplicableInLocal", err)
	}
	if _, ok, err := stores.RunCredentials.Get(ctx, orgID, runID); ok || !errors.Is(err, db.ErrNotApplicableInLocal) {
		t.Fatalf("Get = (ok=%v, err=%v), want (false, ErrNotApplicableInLocal)", ok, err)
	}
}
