package sqlite_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
)

// TestTeamRoster_SQLite runs the shared team-roster conformance suite against
// the SQLite impl. Each subtest opens a fresh in-memory DB; the seeder inserts
// tenancy + membership rows via raw SQL (no store method creates users / orgs /
// teams), while the identity bindings the roster enriches with go through the
// real UsersStore writers inside the conformance body.
//
// Local mode is N=1, so in production this store only ever answers with the one
// synthetic member — but it answers with the same query the multi-team roster
// runs, which is what this suite pins.
func TestTeamRoster_SQLite(t *testing.T) {
	dbtest.RunTeamRosterConformance(t, func(t *testing.T) (dbtest.TeamRosterStores, dbtest.TeamRosterSeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)

		seed := dbtest.TeamRosterSeeder{
			User: func(t *testing.T, displayName string) string {
				t.Helper()
				id := uuid.NewString()
				if _, err := conn.Exec(
					`INSERT INTO users (id, display_name) VALUES (?, ?)`, id, displayName,
				); err != nil {
					t.Fatalf("seed user: %v", err)
				}
				return id
			},
			Org: func(t *testing.T, _ string) string {
				t.Helper()
				// SQLite orgs has no owner column, so ownerID is unused; the
				// uuid doubles as the globally-unique slug.
				id := uuid.NewString()
				if _, err := conn.Exec(
					`INSERT INTO orgs (id, slug, name) VALUES (?, ?, ?)`, id, id, "org-"+id[:8],
				); err != nil {
					t.Fatalf("seed org: %v", err)
				}
				return id
			},
			Team: func(t *testing.T, orgID string) string {
				t.Helper()
				id := uuid.NewString()
				if _, err := conn.Exec(
					`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, ?, ?)`,
					id, orgID, id, "team-"+id[:8],
				); err != nil {
					t.Fatalf("seed team: %v", err)
				}
				return id
			},
			Membership: func(t *testing.T, userID, teamID, role string) {
				t.Helper()
				if _, err := conn.Exec(
					`INSERT INTO memberships (user_id, team_id, role) VALUES (?, ?, ?)`,
					userID, teamID, role,
				); err != nil {
					t.Fatalf("seed membership: %v", err)
				}
			},
		}
		return dbtest.TeamRosterStores{Teams: stores.Teams, Users: stores.Users}, seed
	})
}
