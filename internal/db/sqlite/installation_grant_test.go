package sqlite_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestInstallationGrant_SQLite runs the shared grant-mirror conformance suite
// against the SQLite impl. Each subtest opens a fresh in-memory DB.
//
// The org is the local sentinel rather than a fresh uuid: local mode is N=1 and
// every store method here asserts it, which is the same discipline the rest of
// this package's `...System` methods hold. Tracked repositories are inserted by
// raw SQL — ReplaceForTeam would also reconcile repo_profiles, and the drift
// queries deliberately read team_github_repos rather than that derived cache.
func TestInstallationGrant_SQLite(t *testing.T) {
	dbtest.RunInstallationGrantConformance(t, func(t *testing.T) dbtest.InstallationGrantBackend {
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

		return dbtest.InstallationGrantBackend{
			Apps:   stores.GitHubApps,
			Mirror: stores.InstallationRepos,
			OrgID:  orgID,
			TrackRepo: func(t *testing.T, owner, repo string) {
				t.Helper()
				if _, err := conn.Exec(
					`INSERT INTO team_github_repos (team_id, owner, repo) VALUES (?, ?, ?)`,
					teamID, owner, repo,
				); err != nil {
					t.Fatalf("seed team_github_repos: %v", err)
				}
			},
		}
	})
}
