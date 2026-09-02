package postgres_test

import (
	"fmt"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestGitHubManagedRefresh_Postgres runs the shared managed-refresh
// conformance suite against the Postgres impl — the dialect the managed class
// actually runs on, since a shared App key exists only in multi mode. AdminDB
// serves both pool slots: the installation mirror is admin-pool-only by RLS
// (tf_app is denied every write to it), and the class writer is an app-pool
// write whose RLS predicate is a request context this suite has no claims for.
func TestGitHubManagedRefresh_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunGitHubManagedRefreshConformance(t, func(t *testing.T) (db.GitHubAppsStore, dbtest.GitHubManagedRefreshSeeder) {
		t.Helper()
		stores, seed := newManagedRefreshFixture(t, h)
		return stores.GitHubApps, seed
	})
}

// TestGitHubManagedCadence_Postgres runs the cadence-pass conformance suite
// against the Postgres impl — the dialect the pass actually runs on. Same pool
// wiring as the suite above, for the same reasons.
func TestGitHubManagedCadence_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunGitHubManagedCadenceConformance(t, func(t *testing.T) (db.GitHubAppsStore, dbtest.GitHubManagedRefreshSeeder) {
		t.Helper()
		stores, seed := newManagedRefreshFixture(t, h)
		return stores.GitHubApps, seed
	})
}

// newManagedRefreshFixture resets the shared harness, opens a store bundle
// over its admin connection, and builds the seeder both managed suites stage
// rows through.
func newManagedRefreshFixture(t *testing.T, h *pgtest.Harness) (db.Stores, dbtest.GitHubManagedRefreshSeeder) {
	t.Helper()
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	var n int // per-subtest uniqueness for display names / slugs
	seed := dbtest.GitHubManagedRefreshSeeder{
		User: func(t *testing.T) string {
			t.Helper()
			n++
			return pgtest.SeedUser(t, h, fmt.Sprintf("ghmanaged-u%d", n))
		},
		Org: func(t *testing.T, ownerID string) string {
			t.Helper()
			n++
			return pgtest.SeedOrg(t, h, fmt.Sprintf("ghmanaged-org%d", n), ownerID)
		},
		Class: func(t *testing.T, orgID string, class domain.GitHubCredentialClass, baseURL string) {
			t.Helper()
			if _, err := stores.Orgs.SetGitHubCredentialClass(t.Context(), orgID, class); err != nil {
				t.Fatalf("set credential class: %v", err)
			}
			pgtest.MustExec(t, h.AdminDB, `
				INSERT INTO org_event_sources (org_id, kind, base_url) VALUES ($1, 'github', $2)
				ON CONFLICT (org_id, kind) DO UPDATE SET base_url = EXCLUDED.base_url
			`, orgID, baseURL)
		},
		AllInstallationIDs: func(t *testing.T, orgID string) []string {
			t.Helper()
			rows, err := h.AdminDB.Query(`
				SELECT installation_id FROM org_github_app_installations
				 WHERE org_id = $1 ORDER BY installation_id
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
			pgtest.MustExec(t, h.AdminDB, `
				INSERT INTO reachable_repositories (org_id, credential_class, installation_id, owner, repo)
				VALUES ($1, 'managed_app', $2, 'reach', 'repo-' || $2)
			`, orgID, installationID)
			pgtest.MustExec(t, h.AdminDB, `
				INSERT INTO reachable_scopes (org_id, credential_class, scope) VALUES ($1, 'managed_app', $2)
			`, orgID, installationID)
		},
		ReachRows: func(t *testing.T, orgID, installationID string) (entries, scopes int) {
			t.Helper()
			if err := h.AdminDB.QueryRow(`
				SELECT COUNT(*) FROM reachable_repositories WHERE org_id = $1 AND installation_id = $2
			`, orgID, installationID).Scan(&entries); err != nil {
				t.Fatalf("count reachable entries: %v", err)
			}
			if err := h.AdminDB.QueryRow(`
				SELECT COUNT(*) FROM reachable_scopes WHERE org_id = $1 AND scope = $2
			`, orgID, installationID).Scan(&scopes); err != nil {
				t.Fatalf("count reachable scopes: %v", err)
			}
			return entries, scopes
		},
	}
	return stores, seed
}
