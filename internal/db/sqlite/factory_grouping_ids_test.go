package sqlite_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestFactoryReadStore_SQLite_EntitiesCarryTheirGroupingIDs is the local twin
// of the Postgres test: a Jira row carries its project's registry id and a
// Linear row its Linear team, each from the entity's own column, and neither
// carries the others'. The GitHub arm is in the conformance suite.
func TestFactoryReadStore_SQLite_EntitiesCarryTheirGroupingIDs(t *testing.T) {
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	const site = "https://jira.example.com"
	project := dbseed.JiraProject(t, conn, runmode.LocalDefaultOrgID, site, "SKY")
	issue := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO entities (id, source, source_id, external_id, kind, title, url, snapshot_json, created_at, scope, jira_project_id)
		VALUES (?, 'jira', 'SKY-3', ?, 'issue', 'SKY-3', '', '{}', ?, ?, ?)
	`, issue, dbseed.JiraIssueID("SKY-3"), time.Now().UTC(), site, project); err != nil {
		t.Fatalf("seed jira entity: %v", err)
	}
	linear := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO entities (id, source, source_id, external_id, kind, title, url, snapshot_json, created_at, scope, linear_team_id)
		VALUES (?, 'linear', 'ENG-1', 'uuid-ENG-1', 'issue', 'ENG-1', '', '{}', ?, 'ws-test', 'lt-eng')
	`, linear, time.Now().UTC()); err != nil {
		t.Fatalf("seed linear entity: %v", err)
	}

	rows, err := sqlitestore.New(conn).Factory.Entities(t.Context(), runmode.LocalDefaultOrgID, 100, nil)
	if err != nil {
		t.Fatalf("Entities: %v", err)
	}
	type ids struct{ repository, jiraProject, linearTeam string }
	want := map[string]ids{
		issue:  {jiraProject: project},
		linear: {linearTeam: "lt-eng"},
	}
	seen := 0
	for _, r := range rows {
		w, ok := want[r.Entity.ID]
		if !ok {
			continue
		}
		seen++
		if got := (ids{r.Entity.RepositoryID, r.Entity.JiraProjectID, r.Entity.LinearTeamID}); got != w {
			t.Errorf("%s %s: grouping ids = %+v, want %+v", r.Entity.Source, r.Entity.SourceID, got, w)
		}
	}
	if seen != len(want) {
		t.Fatalf("belt carried %d of the %d seeded entities", seen, len(want))
	}
}
