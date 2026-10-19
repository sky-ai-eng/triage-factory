package db

import (
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	_ "modernc.org/sqlite"
)

const (
	beforeEntityLinearTeam = 202610050001
	entityLinearTeam       = 202610190001
)

// A Linear row takes its team from its stored snapshot, and every row the
// snapshot cannot answer for — another source, no snapshot, a snapshot with
// no team, one that is not JSON — is left NULL.
func TestMigrate_EntityLinearTeamBackfillsFromSnapshot(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemory, beforeEntityLinearTeam)
	treeFS, dir, err := migrationsFor("sqlite3")
	if err != nil {
		t.Fatalf("migrationsFor: %v", err)
	}
	mustExec(t, database, `INSERT INTO orgs (id, slug, name) VALUES (?, 'local', 'Local')`, runmode.LocalDefaultOrgID)

	rows := []struct {
		id, source, sourceID string
		snapshot             any
	}{
		{"e-linear", "linear", "ENG-1", `{"id":"u1","identifier":"ENG-1","team_id":"team-eng","team_key":"ENG"}`},
		{"e-linear-cleared", "linear", "ENG-2", nil},
		{"e-linear-empty", "linear", "ENG-3", ""},
		{"e-linear-no-team", "linear", "ENG-4", `{"id":"u4","identifier":"ENG-4"}`},
		{"e-linear-corrupt", "linear", "ENG-5", `not json`},
		{"e-jira", "jira", "PROJ-1", `{"key":"PROJ-1","team_id":"looks-like-a-team"}`},
	}
	for _, r := range rows {
		mustExec(t, database, `INSERT INTO entities (id, org_id, source, scope, source_id, kind, snapshot_json)
			VALUES (?, ?, ?, 'ws-1', ?, 'issue', ?)`, r.id, runmode.LocalDefaultOrgID, r.source, r.sourceID, r.snapshot)
	}

	gooseMu.Lock()
	goose.SetBaseFS(treeFS)
	upErr := goose.SetDialect("sqlite3")
	if upErr == nil {
		upErr = goose.UpTo(database, dir, entityLinearTeam)
	}
	gooseMu.Unlock()
	if upErr != nil {
		t.Fatalf("goose.UpTo entity linear team: %v", upErr)
	}

	want := map[string]string{"e-linear": "team-eng"}
	for _, r := range rows {
		got := queryString(t, database, `SELECT COALESCE(linear_team_id, '<null>') FROM entities WHERE id = ?`, r.id)
		w, ok := want[r.id]
		if !ok {
			w = "<null>"
		}
		if got != w {
			t.Errorf("%s: linear_team_id = %q, want %q", r.id, got, w)
		}
	}
}
