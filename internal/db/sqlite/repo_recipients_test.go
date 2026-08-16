package sqlite_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
)

// TestRepoRecipients_SQLite runs the shared RepoUpdateRecipientsSystem
// conformance suite against the SQLite impl. Production local mode never
// calls the method (the repoevent.Notifier broadcasts org-wide at N=1),
// but the impl runs the identical query to Postgres precisely so this
// suite can pin both backends to one result — the
// TeamIDsForUserInOrgSystem precedent. The seeder inserts tenancy, role,
// and tracking rows via raw SQL (no store method creates them).
func TestRepoRecipients_SQLite(t *testing.T) {
	dbtest.RunRepoRecipientsConformance(t, func(t *testing.T) (db.TeamGitHubReposStore, dbtest.RepoRecipientsSeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)

		exec := func(t *testing.T, query string, args ...any) {
			t.Helper()
			if _, err := conn.Exec(query, args...); err != nil {
				t.Fatalf("exec %q: %v", query, err)
			}
		}
		seed := dbtest.RepoRecipientsSeeder{
			User: func(t *testing.T) string {
				id := uuid.NewString()
				exec(t, `INSERT INTO users (id, display_name) VALUES (?, ?)`, id, "user-"+id[:8])
				return id
			},
			Org: func(t *testing.T, _ string) string {
				// SQLite orgs has no owner column, so ownerID is unused;
				// the uuid doubles as the globally-unique slug.
				id := uuid.NewString()
				exec(t, `INSERT INTO orgs (id, slug, name) VALUES (?, ?, ?)`, id, id, "org-"+id[:8])
				return id
			},
			Team: func(t *testing.T, orgID string) string {
				id := uuid.NewString()
				exec(t, `INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, ?, ?)`, id, orgID, id, "team-"+id[:8])
				return id
			},
			TeamMembership: func(t *testing.T, userID, teamID string) {
				exec(t, `INSERT INTO memberships (user_id, team_id, role) VALUES (?, ?, 'member')`, userID, teamID)
			},
			OrgMembership: func(t *testing.T, userID, orgID, role string) {
				exec(t, `INSERT INTO org_memberships (user_id, org_id, role) VALUES (?, ?, ?)`, userID, orgID, role)
			},
			TrackRepo: func(t *testing.T, teamID, owner, repo string) {
				// A tracking row references the registry row, so the registry
				// row has to exist first. The org is the team's — a tracking
				// row whose repository lived in another org would be a
				// cross-tenant row, which is the thing the FK plus
				// ReplaceForTeam's team-in-org check exist to make
				// unrepresentable.
				exec(t, `
					INSERT INTO repositories (id, org_id, owner, repo, source)
					SELECT ?, t.org_id, ?, ?, 'github'
					  FROM teams t
					 WHERE t.id = ?
					   AND NOT EXISTS (
					       SELECT 1 FROM repositories r
					        WHERE r.org_id = t.org_id AND r.source = 'github'
					          AND LOWER(r.owner) = LOWER(?) AND LOWER(r.repo) = LOWER(?))
				`, uuid.NewString(), owner, repo, teamID, owner, repo)
				exec(t, `
					INSERT INTO team_github_repos (team_id, repository_id)
					SELECT t.id, r.id
					  FROM teams t
					  JOIN repositories r
					    ON r.org_id = t.org_id AND r.source = 'github'
					   AND LOWER(r.owner) = LOWER(?) AND LOWER(r.repo) = LOWER(?)
					 WHERE t.id = ?
					ON CONFLICT(team_id, repository_id) DO NOTHING
				`, owner, repo, teamID)
			},
			ArchiveTeam: func(t *testing.T, teamID string) {
				exec(t, `UPDATE teams SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`, teamID)
			},
		}
		return stores.TeamGitHubRepos, seed
	})
}
