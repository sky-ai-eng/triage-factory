package postgres_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestGitHubAppUnusable_Postgres runs the shared unusable-App conformance suite
// against the Postgres impl. AdminDB serves both pool slots: the backfill is a
// system path that reads and writes on the admin pool, and the fixtures that
// stage the registration have no request claims to satisfy RLS with.
func TestGitHubAppUnusable_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunGitHubAppUnusableConformance(t, func(t *testing.T) (db.GitHubAppsStore, dbtest.GitHubAppUnusableSeeder) {
		t.Helper()
		h.Reset(t)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

		var n int // per-subtest uniqueness for display names / slugs
		seed := dbtest.GitHubAppUnusableSeeder{
			Org: func(t *testing.T) string {
				t.Helper()
				n++
				owner := pgtest.SeedUser(t, h, fmt.Sprintf("ghunusable-u%d", n))
				return pgtest.SeedOrg(t, h, fmt.Sprintf("ghunusable-org%d", n), owner)
			},
			App: func(t *testing.T, orgID, appID, pemPEM, baseURL string) {
				t.Helper()
				pemRef := "github_app_" + appID + "_pem"
				if _, err := stores.GitHubApps.CreateForOrg(t.Context(), domain.OrgGitHubApp{
					OrgID: orgID, AppID: appID, Slug: "tf-test", ClientID: "Iv1.x",
					ClientSecretRef: "cs_ref", PEMRef: pemRef, WebhookSecretRef: "wh_ref",
					Active: true,
				}); err != nil {
					t.Fatalf("seed app registration: %v", err)
				}
				if err := stores.Secrets.Put(t.Context(), orgID, pemRef, pemPEM, "test app pem"); err != nil {
					t.Fatalf("seed pem secret: %v", err)
				}
				pgtest.MustExec(t, h.AdminDB, `
					INSERT INTO org_event_sources (org_id, kind, base_url) VALUES ($1, 'github', $2)
					ON CONFLICT (org_id, kind) DO UPDATE SET base_url = EXCLUDED.base_url
				`, orgID, baseURL)
			},
			SetUnusableSince: func(t *testing.T, orgID string, at time.Time) {
				t.Helper()
				pgtest.MustExec(t, h.AdminDB, `UPDATE org_github_apps SET unusable_since = $1 WHERE org_id = $2`, at, orgID)
			},
		}
		return stores.GitHubApps, seed
	})
}
