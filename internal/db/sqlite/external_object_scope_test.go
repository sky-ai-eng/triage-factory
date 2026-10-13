package sqlite_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestExternalObjectScope_SQLite runs the shared scope conformance suite for
// artifacts and the audit ledger against the SQLite backend, a fresh in-memory
// DB per subtest.
func TestExternalObjectScope_SQLite(t *testing.T) {
	dbtest.RunExternalObjectScopeConformance(t, func(t *testing.T) (db.Stores, string, string) {
		t.Helper()
		return sqlitestore.New(openSQLiteForTest(t)), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID
	})
}

// TestCommentArtifacts_SQLite runs the shared comment-artifact conformance
// suite against the SQLite backend, a fresh in-memory DB per subtest.
func TestCommentArtifacts_SQLite(t *testing.T) {
	dbtest.RunCommentArtifactConformance(t, func(t *testing.T) (db.Stores, string, string) {
		t.Helper()
		return sqlitestore.New(openSQLiteForTest(t)), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID
	})
}
