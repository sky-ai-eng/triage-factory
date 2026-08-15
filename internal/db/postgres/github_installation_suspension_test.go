package postgres_test

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestGitHubInstallationSuspension_Postgres runs the shared suspension
// conformance suite against the Postgres impl. AdminDB serves both pool slots:
// the installation mirror is admin-pool-only by RLS (tf_app is denied every
// write to it). The seeder fixtures go through h.AdminDB via the pgtest
// helpers, and read the installation's lifecycle columns there too — every
// store read filters removed rows out, and one arrow of the lifecycle is about
// what a removed row kept.
func TestGitHubInstallationSuspension_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunGitHubInstallationSuspensionConformance(t, func(t *testing.T) (db.GitHubAppsStore, dbtest.GitHubSuspensionSeeder) {
		t.Helper()
		h.Reset(t)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

		var n int // per-subtest uniqueness for display names / slugs
		seed := dbtest.GitHubSuspensionSeeder{
			User: func(t *testing.T) string {
				t.Helper()
				n++
				return pgtest.SeedUser(t, h, fmt.Sprintf("ghsusp-u%d", n))
			},
			Org: func(t *testing.T, ownerID string) string {
				t.Helper()
				n++
				return pgtest.SeedOrg(t, h, fmt.Sprintf("ghsusp-org%d", n), ownerID)
			},
			Row: func(t *testing.T, orgID, installationID string) dbtest.GitHubInstallationRow {
				t.Helper()
				var removedAt, suspendedAt sql.NullTime
				var suspendedBy sql.NullString
				err := h.AdminDB.QueryRow(`
					SELECT removed_at, suspended_at, suspended_by
					  FROM org_github_app_installations
					 WHERE org_id = $1 AND installation_id = $2
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
