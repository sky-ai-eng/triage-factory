package postgres_test

import (
	"fmt"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestIdentityRouting_Postgres runs the shared claims-free identity →
// routing-target conformance suite against the Postgres impl. AdminDB
// serves both pool slots so the suite exercises the `...System`
// admin-pool reads directly (BYPASSRLS) — that's the production seam for
// these methods (the router's claims-free eventbus subscriber). The
// seeder fixtures go through h.AdminDB via the pgtest helpers; GitHub
// identity bindings go through the real UsersStore.UpsertGitHubIdentity
// path inside the conformance body.
func TestIdentityRouting_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunIdentityRoutingConformance(t, func(t *testing.T) (dbtest.IdentityRoutingStores, dbtest.IdentityRoutingSeeder) {
		t.Helper()
		h.Reset(t)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

		var n int // per-subtest uniqueness for emails / slugs
		seed := dbtest.IdentityRoutingSeeder{
			User: func(t *testing.T) string {
				t.Helper()
				n++
				return pgtest.SeedUser(t, h, fmt.Sprintf("idroute-u%d", n))
			},
			Org: func(t *testing.T, ownerID string) string {
				t.Helper()
				n++
				return pgtest.SeedOrg(t, h, fmt.Sprintf("idroute-org%d", n), ownerID)
			},
			Team: func(t *testing.T, orgID string) string {
				t.Helper()
				n++
				return pgtest.SeedTeam(t, h, orgID, fmt.Sprintf("idroute-team%d", n))
			},
			Membership: func(t *testing.T, userID, teamID string) {
				t.Helper()
				pgtest.MustExec(t, h.AdminDB,
					`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'admin')`,
					userID, teamID)
			},
		}
		return dbtest.IdentityRoutingStores{Users: stores.Users, Teams: stores.Teams}, seed
	})
}
