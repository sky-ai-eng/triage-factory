package postgres_test

import (
	"fmt"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestGitHubInstallationHost_Postgres runs the shared installation-host
// conformance suite against the Postgres impl. AdminDB serves both pool slots:
// the installation mirror is admin-pool-only by RLS (tf_app is denied every
// write to it). The seeder fixtures go through h.AdminDB via the pgtest
// helpers; the column itself needs no raw probe, since
// ListInstallationsForOrgSystem surfaces it.
func TestGitHubInstallationHost_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunGitHubInstallationHostConformance(t, func(t *testing.T) (db.GitHubAppsStore, dbtest.GitHubInstallationHostSeeder) {
		t.Helper()
		h.Reset(t)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

		var n int // per-subtest uniqueness for display names / slugs
		seed := dbtest.GitHubInstallationHostSeeder{
			User: func(t *testing.T) string {
				t.Helper()
				n++
				return pgtest.SeedUser(t, h, fmt.Sprintf("ghhost-u%d", n))
			},
			Org: func(t *testing.T, ownerID string) string {
				t.Helper()
				n++
				return pgtest.SeedOrg(t, h, fmt.Sprintf("ghhost-org%d", n), ownerID)
			},
		}
		return stores.GitHubApps, seed
	})
}
