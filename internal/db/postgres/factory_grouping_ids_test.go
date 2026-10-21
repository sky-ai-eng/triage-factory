package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestFactoryReadStore_Postgres_EntitiesCarryTheirGroupingIDs: every belt row
// carries the registry id the factory groups its chip by — a pull request's
// repository, a Jira issue's project, a Linear issue's team — read from the
// entity's own column, and only the one for its source.
func TestFactoryReadStore_Postgres_EntitiesCarryTheirGroupingIDs(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _ := seedPgFactoryOrg(t, h)
	teamID := firstTeamForOrg(t, h, orgID)

	trackPgRepo(t, h, orgID, teamID, "acme", "widget")
	pr := seedPgGitHubEntityRaw(t, h, orgID, "acme", "widget", 7)
	seedPgJiraRule(t, h, teamID, "SKY")
	issue := seedPgJiraEntityRaw(t, h, orgID, "SKY", 3)

	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO org_settings (org_id, linear_workspace_id) VALUES ($1, 'ws-test')
		ON CONFLICT (org_id) DO UPDATE SET linear_workspace_id = EXCLUDED.linear_workspace_id
	`, orgID)
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO linear_team_rules (team_id, linear_workspace_id, linear_team_id, linear_team_key, linear_team_name)
		VALUES ($1, 'ws-test', 'lt-eng', 'ENG', 'Engineering')
	`, teamID)
	linear := uuid.New().String()
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO entities (id, org_id, source, source_id, external_id, kind, title, url, snapshot_json, created_at, scope, linear_team_id)
		VALUES ($1, $2, 'linear', 'ENG-1', 'uuid-ENG-1', 'issue', 'ENG-1', '', '{}'::jsonb, $3, 'ws-test', 'lt-eng')
	`, linear, orgID, time.Now().UTC())

	column := func(id, col string) string {
		t.Helper()
		var v string
		if err := h.AdminDB.QueryRow(`SELECT COALESCE(`+col+`::text, '') FROM entities WHERE id = $1`, id).Scan(&v); err != nil {
			t.Fatalf("read %s of %s: %v", col, id, err)
		}
		return v
	}
	type ids struct{ repository, jiraProject, linearTeam string }
	want := map[string]ids{
		pr:     {repository: column(pr, "repository_id")},
		issue:  {jiraProject: column(issue, "jira_project_id")},
		linear: {linearTeam: "lt-eng"},
	}

	rows, err := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey).Factory.Entities(context.Background(), orgID, 100, nil)
	if err != nil {
		t.Fatalf("Entities: %v", err)
	}
	seen := 0
	for _, r := range rows {
		w, ok := want[r.Entity.ID]
		if !ok {
			continue
		}
		seen++
		got := ids{r.Entity.RepositoryID, r.Entity.JiraProjectID, r.Entity.LinearTeamID}
		if got != w || (w.repository == "" && w.jiraProject == "" && w.linearTeam == "") {
			t.Errorf("%s %s: grouping ids = %+v, want %+v", r.Entity.Source, r.Entity.SourceID, got, w)
		}
	}
	if seen != len(want) {
		t.Fatalf("belt carried %d of the %d seeded entities", seen, len(want))
	}
}
