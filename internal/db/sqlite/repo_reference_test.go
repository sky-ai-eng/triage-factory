package sqlite_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestRepoReferences_SQLite runs the shared repository-reference conformance
// suite against the SQLite backend. Each subtest gets a fresh in-memory DB, so
// the "untracking keeps everything" assertions read a state only that subtest
// produced.
func TestRepoReferences_SQLite(t *testing.T) {
	dbtest.RunRepoReferenceConformance(t, func(t *testing.T) (db.Stores, string, string, func(*testing.T, string) string) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		conversation := func(t *testing.T, suffix string) string {
			t.Helper()
			id := uuid.New().String()
			dbtest.SeedConversation(t, conn, domain.Conversation{ID: id, Model: "m"})
			return id
		}
		return stores, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, conversation
	})
}
