package postgres_test

import (
	"fmt"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestRepoRecipients_Postgres runs the shared RepoUpdateRecipientsSystem
// conformance suite against the Postgres impl. AdminDB serves both pool
// slots so the suite exercises the admin-pool read directly (BYPASSRLS)
// — that's the production seam: the emitter (repoprofile's profiler) is
// a claims-free background job, and the recipient union deliberately
// crosses team boundaries the membership RLS policies would hide.
func TestRepoRecipients_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunRepoRecipientsConformance(t, func(t *testing.T) (db.TeamGitHubReposStore, dbtest.RepoRecipientsSeeder) {
		t.Helper()
		h.Reset(t)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

		var n int // per-subtest uniqueness for emails / slugs
		seed := dbtest.RepoRecipientsSeeder{
			User: func(t *testing.T) string {
				t.Helper()
				n++
				return pgtest.SeedUser(t, h, fmt.Sprintf("reporecip-u%d", n))
			},
			Org: func(t *testing.T, ownerID string) string {
				t.Helper()
				n++
				return pgtest.SeedOrg(t, h, fmt.Sprintf("reporecip-org%d", n), ownerID)
			},
			Team: func(t *testing.T, orgID string) string {
				t.Helper()
				n++
				return pgtest.SeedTeam(t, h, orgID, fmt.Sprintf("reporecip-team%d", n))
			},
			TeamMembership: func(t *testing.T, userID, teamID string) {
				t.Helper()
				pgtest.MustExec(t, h.AdminDB,
					`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')`,
					userID, teamID)
			},
			OrgMembership: func(t *testing.T, userID, orgID, role string) {
				t.Helper()
				pgtest.MustExec(t, h.AdminDB,
					`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, $3::org_role)`,
					userID, orgID, role)
			},
			TrackRepo: func(t *testing.T, teamID, host, owner, repo string) string {
				t.Helper()
				// A tracking row references the registry row, so the registry
				// row has to exist first. The org is the team's — a tracking
				// row whose repository lived in another org would be a
				// cross-tenant row, which is the thing the FK plus
				// ReplaceForTeam's team-in-org check exist to make
				// unrepresentable.
				var orgID string
				if err := h.AdminDB.QueryRow(`SELECT org_id::text FROM teams WHERE id = $1`, teamID).Scan(&orgID); err != nil {
					t.Fatalf("read team %s org: %v", teamID, err)
				}
				repositoryID := dbseed.GitHubRepository(t, h.AdminDB, orgID, host, owner, repo)
				pgtest.MustExec(t, h.AdminDB, `
					INSERT INTO team_github_repos (team_id, repository_id, org_id)
					VALUES ($1, $2, $3)
					ON CONFLICT DO NOTHING
				`, teamID, repositoryID, orgID)
				return repositoryID
			},
			RenameRepo: func(t *testing.T, repositoryID, owner, repo string) {
				t.Helper()
				dbseed.RenameRepositoryOnly(t, h.AdminDB, repositoryID, owner, repo)
			},
			RetireRepo: func(t *testing.T, repositoryID string) {
				t.Helper()
				pgtest.MustExec(t, h.AdminDB, `UPDATE repositories SET retired_at = now() WHERE id = $1`, repositoryID)
			},
			ArchiveTeam: func(t *testing.T, teamID string) {
				t.Helper()
				pgtest.MustExec(t, h.AdminDB,
					`UPDATE teams SET deleted_at = now() WHERE id = $1`, teamID)
			},
		}
		return stores.TeamGitHubRepos, seed
	})
}
