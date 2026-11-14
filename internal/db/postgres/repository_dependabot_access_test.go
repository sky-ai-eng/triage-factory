package postgres_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestRepositoryDependabotAccess_Postgres runs the shared Dependabot-access
// conformance suite against the Postgres impl, both pools wired against
// AdminDB like the other system stores' suites, with a second org for the
// cross-org case.
func TestRepositoryDependabotAccess_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	dbtest.RunRepositoryDependabotAccessConformance(t, func(t *testing.T) dbtest.RepositoryDependabotAccessFixture {
		t.Helper()
		h.Reset(t)
		orgID, _, teamID := pgtest.SeedOrgWithUser(t, h, "dependabot-access")
		otherOrgID, _, _ := pgtest.SeedOrgWithUser(t, h, "dependabot-access-other")
		return dbtest.RepositoryDependabotAccessFixture{Stores: stores, OrgID: orgID, TeamID: teamID, OtherOrgID: otherOrgID}
	})
}
