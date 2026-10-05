package sqlite_test

import (
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestGitHubAppUnusable_SQLite runs the shared unusable-App conformance suite
// against the SQLite impl. Each subtest opens a fresh in-memory DB on the
// local sentinel org — local mode is N=1 — with the App's PEM in the mocked
// keychain, which is where the local secret store keeps it.
func TestGitHubAppUnusable_SQLite(t *testing.T) {
	keyring.MockInit()
	dbtest.RunGitHubAppUnusableConformance(t, func(t *testing.T) (db.GitHubAppsStore, dbtest.GitHubAppUnusableSeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)

		seed := dbtest.GitHubAppUnusableSeeder{
			Org: func(t *testing.T) string {
				t.Helper()
				seedSQLiteOrgForApps(t, conn, runmode.LocalDefaultOrgID)
				return runmode.LocalDefaultOrgID
			},
			App: func(t *testing.T, orgID, appID, pemPEM, baseURL string) {
				t.Helper()
				pemRef := "github_app_" + appID + "_pem"
				seedSQLiteApp(t, conn, orgID, appID, pemRef)
				if err := stores.Secrets.Put(t.Context(), orgID, pemRef, pemPEM, "test app pem"); err != nil {
					t.Fatalf("seed pem secret: %v", err)
				}
				if _, err := conn.Exec(`
					INSERT INTO org_event_sources (org_id, kind, base_url) VALUES (?, 'github', ?)
					ON CONFLICT(org_id, kind) DO UPDATE SET base_url = excluded.base_url
				`, orgID, baseURL); err != nil {
					t.Fatalf("seed org_event_sources: %v", err)
				}
			},
			SetUnusableSince: func(t *testing.T, orgID string, at time.Time) {
				t.Helper()
				if _, err := conn.Exec(`UPDATE org_github_apps SET unusable_since = ? WHERE org_id = ?`, at, orgID); err != nil {
					t.Fatalf("set unusable_since: %v", err)
				}
			},
		}
		return stores.GitHubApps, seed
	})
}
