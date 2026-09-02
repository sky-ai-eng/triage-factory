package sqlite_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestGitHubManagedRefresh_SQLite runs the shared managed-refresh conformance
// suite against the SQLite impl. Local mode never carries the managed class —
// a distributed binary ships no shared App key — so this run is what pins the
// invariant for the impl rather than for a deployment: the suite is the same
// on both dialects, and a refresh that creates a row has to fail somewhere.
func TestGitHubManagedRefresh_SQLite(t *testing.T) {
	dbtest.RunGitHubManagedRefreshConformance(t, func(t *testing.T) (db.GitHubAppsStore, dbtest.GitHubManagedRefreshSeeder) {
		t.Helper()
		stores, seed := newManagedRefreshFixture(t)
		return stores.GitHubApps, seed
	})
}

// TestGitHubManagedCadence_SQLite runs the cadence-pass conformance suite
// against the SQLite impl, for the same reason as the suite above: the pass is
// a no-op on every local install, and the impl exists so the invariant is
// pinned on both dialects rather than only the one it runs on.
func TestGitHubManagedCadence_SQLite(t *testing.T) {
	dbtest.RunGitHubManagedCadenceConformance(t, func(t *testing.T) (db.GitHubAppsStore, dbtest.GitHubManagedRefreshSeeder) {
		t.Helper()
		stores, seed := newManagedRefreshFixture(t)
		return stores.GitHubApps, seed
	})
}

// newManagedRefreshFixture opens a fresh in-memory store bundle and builds the
// seeder both managed suites stage rows through.
func newManagedRefreshFixture(t *testing.T) (db.Stores, dbtest.GitHubManagedRefreshSeeder) {
	t.Helper()
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)

	seed := dbtest.GitHubManagedRefreshSeeder{
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
		Class: func(t *testing.T, orgID string, class domain.GitHubCredentialClass, baseURL string) {
			t.Helper()
			if _, err := stores.Orgs.SetGitHubCredentialClass(t.Context(), orgID, class); err != nil {
				t.Fatalf("set credential class: %v", err)
			}
			if _, err := conn.Exec(`
				INSERT INTO org_event_sources (org_id, kind, base_url) VALUES (?, 'github', ?)
				ON CONFLICT(org_id, kind) DO UPDATE SET base_url = excluded.base_url
			`, orgID, baseURL); err != nil {
				t.Fatalf("seed github base url: %v", err)
			}
		},
		AllInstallationIDs: func(t *testing.T, orgID string) []string {
			t.Helper()
			rows, err := conn.Query(`
				SELECT installation_id FROM org_github_app_installations
				 WHERE org_id = ? ORDER BY installation_id
			`, orgID)
			if err != nil {
				t.Fatalf("read installation rows: %v", err)
			}
			defer rows.Close()
			var out []string
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					t.Fatalf("scan installation row: %v", err)
				}
				out = append(out, id)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("read installation rows: %v", err)
			}
			return out
		},
		Reach: func(t *testing.T, orgID, installationID string) {
			t.Helper()
			if _, err := conn.Exec(`
				INSERT INTO reachable_repositories (org_id, credential_class, installation_id, owner, repo)
				VALUES (?, 'managed_app', ?, 'reach', 'repo-' || ?)
			`, orgID, installationID, installationID); err != nil {
				t.Fatalf("seed reachable entry: %v", err)
			}
			if _, err := conn.Exec(`
				INSERT INTO reachable_scopes (org_id, credential_class, scope) VALUES (?, 'managed_app', ?)
			`, orgID, installationID); err != nil {
				t.Fatalf("seed reachable scope: %v", err)
			}
		},
		ReachRows: func(t *testing.T, orgID, installationID string) (entries, scopes int) {
			t.Helper()
			if err := conn.QueryRow(`
				SELECT COUNT(*) FROM reachable_repositories WHERE org_id = ? AND installation_id = ?
			`, orgID, installationID).Scan(&entries); err != nil {
				t.Fatalf("count reachable entries: %v", err)
			}
			if err := conn.QueryRow(`
				SELECT COUNT(*) FROM reachable_scopes WHERE org_id = ? AND scope = ?
			`, orgID, installationID).Scan(&scopes); err != nil {
				t.Fatalf("count reachable scopes: %v", err)
			}
			return entries, scopes
		},
	}
	return stores, seed
}
