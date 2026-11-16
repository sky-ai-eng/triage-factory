package db

import (
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The migration under test, and the one before it, whose schema the test
// seeds.
const (
	blueprintRunTeam       int64 = 202611160001
	beforeBlueprintRunTeam int64 = 202611120001
)

// TestMigrate_BlueprintRunTeamBackfill: an existing run takes the team of its
// lowest-index step conversation, whatever team a later step is on, and a run
// with no conversation keeps no team.
func TestMigrate_BlueprintRunTeamBackfill(t *testing.T) {
	database := openMigrationsTestDBAt(t, beforeBlueprintRunTeam)
	org, team, user := runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID
	const otherTeam = "00000000-0000-0000-0000-0000000000b2"
	execAll(t, database,
		`INSERT INTO orgs (id, slug, name) VALUES ('`+org+`', 'local', 'Local')`,
		`INSERT INTO teams (id, org_id, slug, name) VALUES ('`+team+`', '`+org+`', 'default', 'Default')`,
		`INSERT INTO teams (id, org_id, slug, name) VALUES ('`+otherTeam+`', '`+org+`', 'other', 'Other')`,
		`INSERT INTO users (id, display_name) VALUES ('`+user+`', 'Local')`,
		`INSERT INTO repositories (id, org_id, owner, repo, source, host) VALUES ('r1', '`+org+`', 'octo', 'api', 'github', 'https://github.com')`,
		`INSERT INTO entities (id, org_id, source, scope, source_id, kind, repository_id)
			VALUES ('e1', '`+org+`', 'github', 'https://github.com', 'octo/api#1', 'pr', 'r1')`,
	)
	if err := SeedEventTypes(database, "sqlite3"); err != nil {
		t.Fatalf("seed the events catalog: %v", err)
	}
	execAll(t, database,
		`INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES ('ev1', 'e1', 'github:pr:opened', '')`,
		`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status) VALUES ('t1', 'e1', 'github:pr:opened', 'ev1', 'done')`,
		`INSERT INTO prompts (id, name, body, creator_user_id, team_id) VALUES ('p1', 'P', 'b', '`+user+`', '`+team+`')`,
		`INSERT INTO blueprints (id, name, source, creator_user_id, team_id) VALUES ('bp1', 'BP', 'user', '`+user+`', '`+team+`')`,
		`INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, status, worktree_path, step_plan)
			VALUES ('run-steps', 'bp1', 't1', 'manual', 'completed', '/tmp/wt', '[]')`,
		`INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, status, worktree_path, step_plan)
			VALUES ('run-bare', 'bp1', 't1', 'manual', 'completed', '/tmp/wt', '[]')`,
		// Step 1 is listed first and is on a different team from step 0, so
		// the backfill has to pick by index rather than by row order.
		`INSERT INTO conversations (id, org_id, team_id, task_id, prompt_id, status, blueprint_run_id, blueprint_step_index)
			VALUES ('c-step1', '`+org+`', '`+team+`', 't1', 'p1', 'open', 'run-steps', 1)`,
		`INSERT INTO conversations (id, org_id, team_id, task_id, prompt_id, status, blueprint_run_id, blueprint_step_index)
			VALUES ('c-step0', '`+org+`', '`+otherTeam+`', 't1', 'p1', 'open', 'run-steps', 0)`,
	)

	migrateThrough(t, database, blueprintRunTeam)

	for _, tc := range []struct {
		run, want string
	}{
		{"run-steps", otherTeam},
		{"run-bare", ""},
	} {
		var got sql.NullString
		if err := database.QueryRow(`SELECT team_id FROM blueprint_runs WHERE id = ?`, tc.run).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", tc.run, err)
		}
		if got.String != tc.want || got.Valid != (tc.want != "") {
			t.Errorf("%s team_id = %v, want %q", tc.run, got, tc.want)
		}
	}
}
