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
		}
		return stores.GitHubApps, seed
	})
}
