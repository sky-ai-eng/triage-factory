package sqlite_test

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
)

// TestGitHubNumericIDs_SQLite runs the shared numeric-id conformance suite
// against the SQLite impl. Each subtest opens a fresh in-memory DB; the seeder
// inserts user / org rows via raw SQL (no store method creates them) and reads
// user_github_identities.github_user_id the same way, since no store method
// surfaces that column.
func TestGitHubNumericIDs_SQLite(t *testing.T) {
	dbtest.RunGitHubNumericIDConformance(t, func(t *testing.T) (dbtest.GitHubNumericIDStores, dbtest.GitHubNumericIDSeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)

		seed := dbtest.GitHubNumericIDSeeder{
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
			GitHubUserID: func(t *testing.T, userID, githubBaseURL string) string {
				t.Helper()
				var id sql.NullString
				err := conn.QueryRow(
					`SELECT github_user_id FROM user_github_identities WHERE user_id = ? AND github_base_url = ?`,
					userID, db.NormalizeGitHubHost(githubBaseURL),
				).Scan(&id)
				if errors.Is(err, sql.ErrNoRows) {
					return ""
				}
				if err != nil {
					t.Fatalf("read github_user_id: %v", err)
				}
				return id.String
			},
		}
		return dbtest.GitHubNumericIDStores{Users: stores.Users, GitHubApps: stores.GitHubApps}, seed
	})
}
