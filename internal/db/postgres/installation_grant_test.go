package postgres_test

import (
	"fmt"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestInstallationGrant_Postgres runs the shared grant-mirror conformance suite
// against the Postgres impl. AdminDB serves both pool slots: the mirror is
// admin-pool-only by RLS (tf_app is denied every write to it), exactly like the
// installation rows it hangs off.
//
// Tracked repositories are inserted by raw SQL rather than through
// ReplaceForTeam, which is an app-pool write needing JWT claims and which would
// also reconcile repositories — a table the drift queries deliberately do not
// read, since get-or-create mints rows there for repositories no team tracks.
func TestInstallationGrant_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	var n int // per-subtest uniqueness for slugs
	dbtest.RunInstallationGrantConformance(t, func(t *testing.T) dbtest.InstallationGrantBackend {
		t.Helper()
		h.Reset(t)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

		n++
		owner := pgtest.SeedUser(t, h, fmt.Sprintf("grant-u%d", n))
		orgID := pgtest.SeedOrg(t, h, fmt.Sprintf("grant-org%d", n), owner)
		teamID := pgtest.SeedTeam(t, h, orgID, "default")

		return dbtest.InstallationGrantBackend{
			Apps:   stores.GitHubApps,
			Mirror: stores.InstallationRepos,
			OrgID:  orgID,
			TrackRepo: func(t *testing.T, repoOwner, repo string) {
				t.Helper()
				// The registry row first: a tracking row references it.
				pgtest.MustExec(t, h.AdminDB,
					`INSERT INTO repositories (org_id, owner, repo) VALUES ($1, $2, $3)
					 ON CONFLICT DO NOTHING`,
					orgID, repoOwner, repo)
				var repositoryID string
				if err := h.AdminDB.QueryRow(
					`SELECT id::text FROM repositories
					  WHERE org_id = $1 AND lower(owner) = lower($2) AND lower(repo) = lower($3)`,
					orgID, repoOwner, repo,
				).Scan(&repositoryID); err != nil {
					t.Fatalf("resolve repository id: %v", err)
				}
				pgtest.MustExec(t, h.AdminDB,
					`INSERT INTO team_github_repos (team_id, repository_id) VALUES ($1, $2)
					 ON CONFLICT DO NOTHING`,
					teamID, repositoryID)
			},
		}
	})
}
