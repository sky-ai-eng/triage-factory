package sqlite_test

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func TestSourceStatus_SQLite(t *testing.T) {
	dbtest.RunSourceStatusConformance(t, func(t *testing.T) dbtest.SourceStatusHarness {
		t.Helper()
		conn := openSQLiteForTest(t)
		if _, err := conn.Exec(
			`INSERT OR IGNORE INTO agents (id, org_id, display_name) VALUES (?, ?, 'Test Bot')`,
			runmode.LocalDefaultAgentID, runmode.LocalDefaultOrgID,
		); err != nil {
			t.Fatalf("seed local agent: %v", err)
		}
		stores := sqlitestore.New(conn)
		return dbtest.SourceStatusHarness{
			Tasks:   stores.Tasks,
			Swipes:  stores.Swipes,
			OrgID:   runmode.LocalDefaultOrgID,
			UserID:  runmode.LocalDefaultUserID,
			AgentID: runmode.LocalDefaultAgentID,
			SeedTask: func(t *testing.T, source, snapshot string) string {
				t.Helper()
				return seedSQLiteSourceTask(t, conn, source, snapshot)
			},
		}
	})
}

// seedSQLiteSourceTask inserts a queued task on a fresh entity of source
// carrying snapshot, owned by the local team.
func seedSQLiteSourceTask(t *testing.T, conn *sql.DB, source, snapshot string) string {
	t.Helper()
	entityID, eventID, taskID := uuid.New().String(), uuid.New().String(), uuid.New().String()
	kind, scope, eventType := sourceStatusFixture(source)
	sourceID := "src-" + entityID
	var repositoryID, jiraProjectID any
	switch source {
	case "github":
		sourceID = "octo/src#" + entityID
		repositoryID = dbseed.GitHubEntityRepository(t, conn, runmode.LocalDefaultOrgID, scope, sourceID)
	case "jira":
		jiraProjectID = dbseed.JiraIssueProjectID(t, conn, runmode.LocalDefaultOrgID, scope, sourceID)
	}
	if _, err := conn.Exec(
		`INSERT INTO entities (id, source, source_id, kind, snapshot_json, scope, repository_id, jira_project_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		entityID, source, sourceID, kind, snapshot, scope, repositoryID, jiraProjectID,
	); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := conn.Exec(
		`INSERT INTO events (id, entity_id, event_type, dedup_key, metadata_json) VALUES (?, ?, ?, '', '{}')`,
		eventID, entityID, eventType,
	); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := conn.Exec(
		`INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id, status, team_id, visibility)
		 VALUES (?, ?, ?, '', ?, 'queued', ?, 'team')`,
		taskID, entityID, eventType, eventID, runmode.LocalDefaultTeamID,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return taskID
}

// sourceStatusFixture is the entity kind, scope and a catalogued event type
// for a task of source.
func sourceStatusFixture(source string) (kind, scope, eventType string) {
	switch source {
	case "jira":
		return "issue", "https://jira.example.com", "jira:issue:assigned"
	case "linear":
		return "issue", "ws-test", "linear:issue:available"
	default:
		return "pr", "https://github.com", "github:pr:opened"
	}
}
