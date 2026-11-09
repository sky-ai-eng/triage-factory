package sqlite_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestGitHubRefreshMarks_SQLite runs the shared refresh-mark conformance suite
// against the SQLite impl, a fresh in-memory database per subtest. SQLite holds
// one org, so the cross-org cases skip.
func TestGitHubRefreshMarks_SQLite(t *testing.T) {
	dbtest.RunGitHubRefreshMarkConformance(t, func(t *testing.T) dbtest.GitHubRefreshMarkFixture {
		return dbtest.GitHubRefreshMarkFixture{Stores: sqlitestore.New(openSQLiteForTest(t)), OrgID: runmode.LocalDefaultOrgID}
	})
}
