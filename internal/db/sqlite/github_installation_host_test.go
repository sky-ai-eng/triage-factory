package sqlite_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
)

// TestGitHubInstallationHost_SQLite runs the shared installation-host
// conformance suite against the SQLite impl. Each subtest opens a fresh
// in-memory DB; the seeder inserts user / org rows via raw SQL (no store method
// creates them). No raw probe is needed for the column itself —
// ListInstallationsForOrgSystem surfaces it.
func TestGitHubInstallationHost_SQLite(t *testing.T) {
	dbtest.RunGitHubInstallationHostConformance(t, func(t *testing.T) (db.GitHubAppsStore, dbtest.GitHubInstallationHostSeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)

		seed := dbtest.GitHubInstallationHostSeeder{
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
		}
		return stores.GitHubApps, seed
	})
}
