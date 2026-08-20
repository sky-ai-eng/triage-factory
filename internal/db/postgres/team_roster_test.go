package postgres_test

import (
	"fmt"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestTeamRoster_Postgres runs the shared team-roster conformance suite against
// the Postgres impl. AdminDB serves both pool slots so the suite pins the
// query's own semantics — ordering, paging, host scoping — independent of the
// auth path; the RLS arm that gates who may read a roster is exercised by the
// handler tests, which run under real claims.
//
// The seeder fixtures go through h.AdminDB via the pgtest helpers; identity
// bindings go through the real UsersStore writers inside the conformance body.
func TestTeamRoster_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunTeamRosterConformance(t, func(t *testing.T) (dbtest.TeamRosterStores, dbtest.TeamRosterSeeder) {
		t.Helper()
		h.Reset(t)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

		var n int // per-subtest uniqueness for emails / slugs
		seed := dbtest.TeamRosterSeeder{
			User: func(t *testing.T, displayName string) string {
				t.Helper()
				n++
				// SeedUser derives the auth email from the display name, and
				// the paging case deliberately reuses one name — so the
				// counter, not the name, is what keeps the email unique.
				id := pgtest.SeedUser(t, h, fmt.Sprintf("roster-u%d", n))
				pgtest.MustExec(t, h.AdminDB,
					`UPDATE users SET display_name = $2 WHERE id = $1`, id, displayName)
				return id
			},
			Org: func(t *testing.T, ownerID string) string {
				t.Helper()
				n++
				return pgtest.SeedOrg(t, h, fmt.Sprintf("roster-org%d", n), ownerID)
			},
			Team: func(t *testing.T, orgID string) string {
				t.Helper()
				n++
				return pgtest.SeedTeam(t, h, orgID, fmt.Sprintf("roster-team%d", n))
			},
			Membership: func(t *testing.T, userID, teamID, role string) {
				t.Helper()
				pgtest.MustExec(t, h.AdminDB,
					`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, $3::membership_role)`,
					userID, teamID, role)
			},
		}
		return dbtest.TeamRosterStores{Teams: stores.Teams, Users: stores.Users}, seed
	})
}
