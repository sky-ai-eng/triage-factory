package postgres_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestGitHubRefreshMarks_Postgres runs the shared refresh-mark conformance
// suite against the Postgres impl, both pools wired against AdminDB like the
// other system stores' suites, with a second org for the cross-org cases.
func TestGitHubRefreshMarks_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	dbtest.RunGitHubRefreshMarkConformance(t, func(t *testing.T) dbtest.GitHubRefreshMarkFixture {
		t.Helper()
		h.Reset(t)
		orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "refresh-marks")
		otherOrgID, _, _ := pgtest.SeedOrgWithUser(t, h, "refresh-marks-other")
		return dbtest.GitHubRefreshMarkFixture{Stores: stores, OrgID: orgID, OtherOrgID: otherOrgID}
	})
}
