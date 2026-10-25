package sqlite_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestTeamActivityStore_SQLite runs the shared conformance suite against the
// SQLite impl, reusing the factory seeder — the two stores share their
// definition of a team's flow. teamID is the local default and ignored by
// the impl: local is N=1 and unscoped, the same posture as the belt.
func TestTeamActivityStore_SQLite(t *testing.T) {
	dbtest.RunTeamActivityConformance(t, func(t *testing.T) (db.TeamActivityStore, string, string, dbtest.FactorySeeder) {
		t.Helper()
		conn := openTeamActivityTestDB(t)
		stores := sqlitestore.New(conn)
		return stores.TeamActivity, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, newSQLiteTeamActivitySeeder(conn)
	})

	// Local's by-source cut is unscoped, so an event on a repository the team
	// does not track counts there; the tracked-set cut still has no row for
	// that repository, because its rows are the units the team watches.
	t.Run("an_untracked_unit_has_no_row", func(t *testing.T) {
		conn := openTeamActivityTestDB(t)
		stores := sqlitestore.New(conn)
		seed := newSQLiteTeamActivitySeeder(conn)
		now := time.Now().UTC()

		tracked, repo := seed.TrackedEntity(t, "github")
		seed.Event(t, tracked, domain.EventGitHubPROpened, "", now.Add(-time.Hour), time.Time{})
		untracked := seed.Entity(t, "untracked")
		seed.Event(t, untracked, domain.EventGitHubPROpened, "", now.Add(-time.Hour), time.Time{})

		got, err := stores.TeamActivity.TeamActivity(context.Background(),
			runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, now.Add(-2*time.Hour), now)
		if err != nil {
			t.Fatalf("TeamActivity: %v", err)
		}
		if len(got.ByTracked) != 1 || got.ByTracked[0].UnitID != repo || got.ByTracked[0].Events != 1 {
			t.Errorf("by_tracked = %+v, want the one tracked repository with its one event", got.ByTracked)
		}
		for _, row := range got.BySource {
			if row.Source == "github" && (row.Events == nil || *row.Events != 2) {
				t.Errorf("github by_source events = %v, want 2 — local counts every repository", row.Events)
			}
		}
	})

	// Local defines every source's event count, linear's included.
	t.Run("linear_events_are_defined", func(t *testing.T) {
		stores := sqlitestore.New(openTeamActivityTestDB(t))
		now := time.Now().UTC()
		got, err := stores.TeamActivity.TeamActivity(context.Background(),
			runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, now.Add(-time.Hour), now)
		if err != nil {
			t.Fatalf("TeamActivity: %v", err)
		}
		for _, row := range got.BySource {
			if row.Source == "linear" {
				if row.Events == nil {
					t.Error("linear events undefined, want a defined zero")
				}
				return
			}
		}
		t.Error("by_source has no linear row")
	})
}

func openTeamActivityTestDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	if _, err := conn.Exec(
		`INSERT INTO prompts (id, name, body, creator_user_id, team_id) VALUES (?, 'Factory Test', 'body', ?, ?)`,
		factoryTestPromptID, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID,
	); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	return conn
}

// newSQLiteTeamActivitySeeder is the factory seeder plus TrackedEntity, which
// writes the local team's tracking row for each new unit — the one table the
// tracked-set cut reads that the factory seeder never needs.
func newSQLiteTeamActivitySeeder(conn *sql.DB) dbtest.FactorySeeder {
	seed := newSQLiteFactorySeeder(conn)
	const workspace = "ws-test"
	seed.TrackedEntity = func(t *testing.T, source string) (string, string) {
		t.Helper()
		org, team := runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID
		id := uuid.New().String()
		key := "K" + strings.ToUpper(id[:6])
		var unit string
		var err error
		switch source {
		case "github":
			// A repository of its own: the factory seeder's Entity puts every
			// pull request in one.
			repo := strings.ToLower(key)
			sourceID := "octo/" + repo + "#1"
			unit = dbseed.GitHubRepository(t, conn, org, dbtest.TestGitHubHost, "octo", repo)
			if _, err = conn.Exec(`INSERT INTO team_github_repos (team_id, repository_id, org_id) VALUES (?, ?, ?)`, team, unit, org); err == nil {
				_, err = conn.Exec(`
					INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, created_at, scope, repository_id)
					VALUES (?, 'github', ?, 'pr', ?, '', '{}', ?, ?, ?)`,
					id, sourceID, sourceID, time.Now().UTC(), dbtest.TestGitHubHost, unit)
			}
		case "jira":
			const site = "https://jira.example.com"
			unit = dbseed.JiraProject(t, conn, org, site, key)
			sourceID := key + "-1"
			if _, err = conn.Exec(`INSERT INTO jira_project_status_rules (team_id, jira_project_id, org_id) VALUES (?, ?, ?)`, team, unit, org); err == nil {
				_, err = conn.Exec(`
					INSERT INTO entities (id, source, source_id, external_id, kind, title, url, snapshot_json, created_at, scope, jira_project_id)
					VALUES (?, 'jira', ?, ?, 'issue', ?, '', '{}', ?, ?, ?)`,
					id, sourceID, dbseed.JiraIssueID(sourceID), sourceID, time.Now().UTC(), site, unit)
			}
		case "linear":
			unit = "lt-" + id[:8]
			if _, err = conn.Exec(`
				INSERT INTO org_settings (org_id, linear_workspace_id) VALUES (?, ?)
				ON CONFLICT (org_id) DO UPDATE SET linear_workspace_id = excluded.linear_workspace_id`, org, workspace); err != nil {
				break
			}
			if _, err = conn.Exec(`
				INSERT INTO linear_team_rules (team_id, linear_workspace_id, linear_team_id, linear_team_key, linear_team_name)
				VALUES (?, ?, ?, ?, ?)`, team, workspace, unit, key, key); err != nil {
				break
			}
			_, err = conn.Exec(`
				INSERT INTO entities (id, source, source_id, external_id, kind, title, url, snapshot_json, created_at, scope, linear_team_id)
				VALUES (?, 'linear', ?, ?, 'issue', ?, '', '{}', ?, ?, ?)`,
				id, key+"-1", "uuid-"+id, key+"-1", time.Now().UTC(), workspace, unit)
		default:
			t.Fatalf("TrackedEntity: no tracked set for source %q", source)
		}
		if err != nil {
			t.Fatalf("seed tracked %s entity: %v", source, err)
		}
		return id, unit
	}
	return seed
}
