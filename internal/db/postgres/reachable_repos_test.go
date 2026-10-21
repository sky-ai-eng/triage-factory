package postgres_test

import (
	"fmt"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestReachableRepos_Postgres runs the shared reachable-repo conformance suite
// against the Postgres impl. AdminDB serves both pool slots: the mirror is
// admin-pool-only by RLS (tf_app is denied every write to it), exactly like the
// installation rows it hangs off.
//
// Tracked repositories are inserted by raw SQL rather than through
// ReplaceForTeam, which is an app-pool write needing JWT claims and which would
// also reconcile repositories — a table the drift queries deliberately do not
// read, since get-or-create mints rows there for repositories no team tracks.
func TestReachableRepos_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	var n int // per-subtest uniqueness for slugs
	dbtest.RunReachableReposConformance(t, func(t *testing.T) dbtest.ReachableReposBackend {
		t.Helper()
		h.Reset(t)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

		n++
		owner := pgtest.SeedUser(t, h, fmt.Sprintf("grant-u%d", n))
		orgID := pgtest.SeedOrg(t, h, fmt.Sprintf("grant-org%d", n), owner)
		teamID := pgtest.SeedTeam(t, h, orgID, "default")

		return dbtest.ReachableReposBackend{
			Apps:   stores.GitHubApps,
			Mirror: stores.ReachableRepos,
			OrgID:  orgID,
			TrackRepo: func(t *testing.T, repoOwner, repo string) {
				t.Helper()
				// The registry row first: a tracking row references it.
				pgtest.SeedTrackedRepoOnHost(t, h, orgID, teamID, dbtest.TestGitHubHost, repoOwner, repo)
			},
			RegisterRepo: func(t *testing.T, repoOwner, repo, externalID string) string {
				t.Helper()
				id := pgtest.SeedRepositoryOnHost(t, h, orgID, dbtest.TestGitHubHost, repoOwner, repo)
				if externalID != "" {
					pgtest.MustExec(t, h.AdminDB, `UPDATE repositories SET external_id = $1 WHERE id = $2`, externalID, id)
				}
				return id
			},
			SetOwnerExternalID: func(t *testing.T, repositoryID, ownerExternalID string) {
				t.Helper()
				pgtest.MustExec(t, h.AdminDB, `UPDATE repositories SET owner_external_id = $1 WHERE id = $2`, ownerExternalID, repositoryID)
			},
			TrackRepository: func(t *testing.T, repositoryID string) {
				t.Helper()
				pgtest.MustExec(t, h.AdminDB, `
					INSERT INTO team_github_repos (team_id, repository_id, org_id) VALUES ($1, $2, $3)
					ON CONFLICT DO NOTHING
				`, teamID, repositoryID, orgID)
			},
		}
	})
}
