package sqlite_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestJiraProjectStore_SQLite runs the shared registry suite against the SQLite
// impl. Local mode is one org; the second team the suite needs is inserted
// straight into teams beside the seeded default team.
func TestJiraProjectStore_SQLite(t *testing.T) {
	dbtest.RunJiraProjectConformance(t, func(t *testing.T) dbtest.JiraProjectFixture {
		t.Helper()
		conn := openSQLiteForTest(t)
		const otherTeamID = "00000000-0000-4000-8000-0000000000b2"
		if _, err := conn.Exec(
			`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, ?, ?)`,
			otherTeamID, runmode.LocalDefaultOrgID, "other", "Other",
		); err != nil {
			t.Fatalf("seed second team: %v", err)
		}
		stores := sqlitestore.New(conn)
		return dbtest.JiraProjectFixture{
			Projects:    stores.JiraProjects,
			Rules:       stores.JiraStatusRules,
			Teams:       stores.Teams,
			OrgID:       runmode.LocalDefaultOrgID,
			TeamID:      runmode.LocalDefaultTeamID,
			OtherTeamID: otherTeamID,
			SeedUnresolved: func(t *testing.T, site, key string) string {
				t.Helper()
				id := uuid.NewString()
				if _, err := conn.Exec(
					`INSERT INTO jira_projects (id, org_id, site, project_key) VALUES (?, ?, ?, ?)`,
					id, runmode.LocalDefaultOrgID, site, key,
				); err != nil {
					t.Fatalf("seed unresolved jira project: %v", err)
				}
				return id
			},
		}
	})
}
