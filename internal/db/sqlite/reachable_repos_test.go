package sqlite_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestReachableRepos_SQLite runs the shared reachable-repo conformance suite
// against the SQLite impl. Each subtest opens a fresh in-memory DB.
//
// The org is the local sentinel rather than a fresh uuid: local mode is N=1 and
// every store method here asserts it, which is the same discipline the rest of
// this package's `...System` methods hold. Tracked repositories are inserted by
// raw SQL rather than through ReplaceForTeam, which is a full-set replace and
// would drop the previous repository each time the suite tracks another one.
func TestReachableRepos_SQLite(t *testing.T) {
	dbtest.RunReachableReposConformance(t, func(t *testing.T) dbtest.ReachableReposBackend {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)

		// The local tenant row may already exist (BootstrapSchemaForTest runs the
		// same ensure the binary does), so this is the ensure and not a create.
		orgID := runmode.LocalDefaultOrgID
		if _, err := conn.Exec(
			`INSERT OR IGNORE INTO orgs (id, slug, name) VALUES (?, 'local', 'Local')`, orgID,
		); err != nil {
			t.Fatalf("seed org: %v", err)
		}
		// Same for the team: local bootstrap mints one, and the suite only needs
		// SOME team in the org to hang tracked repositories off.
		teamID := uuid.NewString()
		if _, err := conn.Exec(
			`INSERT OR IGNORE INTO teams (id, org_id, slug, name) VALUES (?, ?, 'grant-suite', 'Grant suite')`,
			teamID, orgID,
		); err != nil {
			t.Fatalf("seed team: %v", err)
		}

		return dbtest.ReachableReposBackend{
			Apps:   stores.GitHubApps,
			Mirror: stores.ReachableRepos,
			Repos:  stores.Repos,
			Teams:  stores.Teams,
			OrgID:  orgID,
			TeamID: teamID,
			SeedTeam: func(t *testing.T, slug string) string {
				t.Helper()
				id := uuid.NewString()
				if _, err := conn.Exec(
					`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, ?, ?)`, id, orgID, slug, slug,
				); err != nil {
					t.Fatalf("seed team %s: %v", slug, err)
				}
				return id
			},
			TrackRepository: func(t *testing.T, teamID, repositoryID string) {
				t.Helper()
				if _, err := conn.Exec(
					`INSERT INTO team_github_repos (team_id, repository_id, org_id) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`,
					teamID, repositoryID, runmode.LocalDefaultOrgID,
				); err != nil {
					t.Fatalf("seed team_github_repos: %v", err)
				}
			},
		}
	})
}
