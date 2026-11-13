package sqlite_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestRepositoryDependabotAccess_SQLite runs the shared Dependabot-access
// conformance suite against the SQLite impl, a fresh in-memory database per
// subtest. SQLite holds one org, so the cross-org case skips.
func TestRepositoryDependabotAccess_SQLite(t *testing.T) {
	dbtest.RunRepositoryDependabotAccessConformance(t, func(t *testing.T) dbtest.RepositoryDependabotAccessFixture {
		return dbtest.RepositoryDependabotAccessFixture{
			Stores: sqlitestore.New(openSQLiteForTest(t)),
			OrgID:  runmode.LocalDefaultOrgID,
			TeamID: runmode.LocalDefaultTeamID,
		}
	})
}
