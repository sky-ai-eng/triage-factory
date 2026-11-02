package postgres_test

import (
	"fmt"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestTeamListForOrg_Postgres runs the shared ListForOrg conformance suite
// against the Postgres impl. AdminDB serves both pool slots, so the suite pins
// the query's own semantics; who may read the list under RLS is exercised by
// the handler tests, which run under real claims.
func TestTeamListForOrg_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunTeamListForOrgConformance(t, func(t *testing.T) (db.TeamsStore, dbtest.TeamListSeeder) {
		t.Helper()
		h.Reset(t)
		owner := pgtest.SeedUser(t, h, "team-list-owner")
		var n int // per-call uniqueness for slugs
		seed := dbtest.TeamListSeeder{
			Org: func(t *testing.T) string {
				t.Helper()
				n++
				return pgtest.SeedOrg(t, h, fmt.Sprintf("team-list-org%d", n), owner)
			},
			Team: func(t *testing.T, orgID string) string {
				t.Helper()
				n++
				return pgtest.SeedTeam(t, h, orgID, fmt.Sprintf("team-list-team%d", n))
			},
		}
		return pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey).Teams, seed
	})
}
