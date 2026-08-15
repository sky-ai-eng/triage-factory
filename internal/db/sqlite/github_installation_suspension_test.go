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

// TestGitHubInstallationSuspension_SQLite runs the shared suspension
// conformance suite against the SQLite impl. Each subtest opens a fresh
// in-memory DB; the seeder inserts user / org rows via raw SQL (no store
// method creates them) and reads the installation's lifecycle columns the same
// way, since every store read filters removed rows out and one arrow of the
// lifecycle is about what a removed row kept.
func TestGitHubInstallationSuspension_SQLite(t *testing.T) {
	dbtest.RunGitHubInstallationSuspensionConformance(t, func(t *testing.T) (db.GitHubAppsStore, dbtest.GitHubSuspensionSeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)

		seed := dbtest.GitHubSuspensionSeeder{
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
			Row: func(t *testing.T, orgID, installationID string) dbtest.GitHubInstallationRow {
				t.Helper()
				var removedAt, suspendedAt sql.NullTime
				var suspendedBy sql.NullString
				err := conn.QueryRow(`
					SELECT removed_at, suspended_at, suspended_by
					  FROM org_github_app_installations
					 WHERE org_id = ? AND installation_id = ?
				`, orgID, installationID).Scan(&removedAt, &suspendedAt, &suspendedBy)
				if errors.Is(err, sql.ErrNoRows) {
					return dbtest.GitHubInstallationRow{}
				}
				if err != nil {
					t.Fatalf("read installation row: %v", err)
				}
				return dbtest.GitHubInstallationRow{
					Exists:      true,
					Removed:     removedAt.Valid,
					Suspended:   suspendedAt.Valid,
					SuspendedBy: suspendedBy.String,
				}
			},
		}
		return stores.GitHubApps, seed
	})
}
