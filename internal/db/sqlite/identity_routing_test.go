package sqlite_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
)

// TestIdentityRouting_SQLite runs the shared claims-free identity →
// routing-target conformance suite against the SQLite impl. Each subtest
// opens a fresh in-memory DB; the seeder inserts tenancy + membership
// rows via raw SQL (no store method creates users/orgs/teams), while the
// GitHub identity bindings the reverse-lookup reads go through the real
// UsersStore.UpsertGitHubIdentity path inside the conformance body.
func TestIdentityRouting_SQLite(t *testing.T) {
	dbtest.RunIdentityRoutingConformance(t, func(t *testing.T) (dbtest.IdentityRoutingStores, dbtest.IdentityRoutingSeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)

		seed := dbtest.IdentityRoutingSeeder{
			User: func(t *testing.T) string {
				t.Helper()
				id := uuid.NewString()
				if _, err := conn.Exec(
					`INSERT INTO users (id, display_name) VALUES (?, ?)`, id, "user-"+id[:8],
				); err != nil {
					t.Fatalf("seed user: %v", err)
				}
				return id
			},
			Org: func(t *testing.T, _ string) string {
				t.Helper()
				// SQLite orgs has no owner column, so ownerID is unused;
				// the uuid doubles as the globally-unique slug.
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
			Membership: func(t *testing.T, userID, teamID string) {
				t.Helper()
				if _, err := conn.Exec(
					`INSERT INTO memberships (user_id, team_id, role) VALUES (?, ?, 'admin')`,
					userID, teamID,
				); err != nil {
					t.Fatalf("seed membership: %v", err)
				}
			},
		}
		return dbtest.IdentityRoutingStores{Users: stores.Users, Teams: stores.Teams}, seed
	})
}
