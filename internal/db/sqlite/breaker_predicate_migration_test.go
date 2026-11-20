package sqlite_test

import (
	"context"
	"database/sql"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestMigrate_BlueprintRunsRecordPredicate_SQLite pins the upgrade: every
// event-fired run on disk records its trigger's predicate as it stands, so a
// breaker that held before the upgrade holds after it. That includes a shipped
// handler's, whose empty-list predicate the upgrade first rewrites to the NULL
// the API stores for it, on the event types where the list is a field.
func TestMigrate_BlueprintRunsRecordPredicate_SQLite(t *testing.T) {
	ctx := context.Background()
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	// The migration before the one under test.
	const prior = 202611200005
	if err := db.BootstrapSchemaAtForTest(conn, prior); err != nil {
		t.Fatalf("schema at %d: %v", prior, err)
	}
	if err := db.SeedLocalTenantRows(ctx, conn); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	// event_handlers.event_type and events.event_type reference the catalog,
	// which Migrate seeds after the last migration and this database has not
	// reached.
	if err := db.SeedEventTypes(conn, "sqlite3"); err != nil {
		t.Fatalf("seed the events catalog: %v", err)
	}
	stores := sqlitestore.New(conn)
	org, team, user := runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID
	entity, _, err := stores.Entities.FindOrCreateSystem(ctx, org, "github", dbtest.TestGitHubHost,
		"octo/breaker#1", "PR_breaker1", domain.EntityKindPullRequest, "t", "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	exec(`INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES ('ev-breaker', ?, ?, '')`,
		entity.ID, domain.EventGitHubPRCICheckFailed)
	exec(`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status) VALUES ('t-breaker', ?, ?, 'ev-breaker', 'queued')`,
		entity.ID, domain.EventGitHubPRCICheckFailed)
	exec(`INSERT INTO blueprints (id, org_id, team_id, name, source, creator_user_id) VALUES ('bp-breaker', ?, ?, 'Fix CI', 'user', ?)`,
		org, team, user)

	// shipped holds the form the shipped CI handler was seeded with, user a
	// predicate of its own, and vulnerability the shipped form on an event
	// type that has no such field, where it does not mean match-all.
	exec(`INSERT INTO event_handlers (id, org_id, team_id, creator_user_id, mode, event_type, scope_predicate_json,
	                                  source, system_slug, name, blueprint_id, breaker_threshold)
	      VALUES ('shipped', ?, ?, NULL, 'automate', ?, '{"author_id_in":[]}', 'system', 'system-handler-ci-check-failed',
	              'CI check failed on my PR', 'bp-breaker', 2)`,
		org, team, domain.EventGitHubPRCICheckFailed)
	exec(`INSERT INTO event_handlers (id, org_id, team_id, creator_user_id, mode, event_type, scope_predicate_json,
	                                  source, name, blueprint_id, breaker_threshold)
	      VALUES ('user', ?, ?, ?, 'automate', ?, '{"author_id_in":["583231"]}', 'user', 'CI failed on octocat''s PRs', 'bp-breaker', 2)`,
		org, team, user, domain.EventGitHubPRCICheckFailed)
	exec(`INSERT INTO event_handlers (id, org_id, team_id, creator_user_id, mode, event_type, scope_predicate_json,
	                                  source, name, breaker_threshold)
	      VALUES ('vulnerability', ?, ?, ?, 'watch', ?, '{"author_id_in":[]}', 'user', 'Alerts', 2)`,
		org, team, user, domain.EventGitHubVulnerabilityAlertOpened)

	run := func(id string, trigger any, triggerType string, creator any, status, startedAt string) {
		t.Helper()
		exec(`INSERT INTO blueprint_runs (id, team_id, blueprint_id, task_id, trigger_type, trigger_id, creator_user_id,
		                                  status, worktree_path, step_plan, started_at)
		      VALUES (?, ?, 'bp-breaker', 't-breaker', ?, ?, ?, ?, '/tmp/wt', '[]', ?)`,
			id, team, triggerType, trigger, creator, status, startedAt)
	}
	run("shipped-1", "shipped", "event", nil, "failed", "2026-11-01 10:00:00")
	run("shipped-2", "shipped", "event", nil, "aborted", "2026-11-01 10:01:00")
	run("user-1", "user", "event", nil, "failed", "2026-11-01 10:02:00")
	run("user-2", "user", "event", nil, "failed", "2026-11-01 10:03:00")
	run("manual", nil, "manual", user, "failed", "2026-11-01 10:04:00")

	if err := db.Migrate(conn, "sqlite3"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	for _, trigger := range []string{"shipped", "user"} {
		got, err := stores.Tasks.BreakerStreakSystem(ctx, org, entity.ID, trigger)
		if err != nil {
			t.Fatalf("BreakerStreakSystem(%s): %v", trigger, err)
		}
		if got.Failures != 2 {
			t.Errorf("%s streak after the upgrade = %+v, want the 2 failures it had before", trigger, got)
		}
	}
	tripped, err := stores.Tasks.ListTrippedBreakersSystem(ctx, org)
	if err != nil {
		t.Fatalf("ListTrippedBreakersSystem: %v", err)
	}
	want := []domain.TrippedBreaker{{TriggerID: "shipped", EntityID: entity.ID}, {TriggerID: "user", EntityID: entity.ID}}
	if !slices.Equal(tripped, want) {
		t.Errorf("tripped after the upgrade = %+v, want %+v", tripped, want)
	}

	predicateOf := func(table, id string) sql.NullString {
		t.Helper()
		var p sql.NullString
		if err := conn.QueryRow(`SELECT scope_predicate_json FROM `+table+` WHERE id = ?`, id).Scan(&p); err != nil {
			t.Fatalf("read %s %s: %v", table, id, err)
		}
		return p
	}
	for id, want := range map[string]sql.NullString{
		"shipped":       {},
		"user":          {String: `{"author_id_in":["583231"]}`, Valid: true},
		"vulnerability": {String: `{"author_id_in":[]}`, Valid: true},
	} {
		if got := predicateOf("event_handlers", id); got != want {
			t.Errorf("handler %s predicate after the upgrade = %+v, want %+v", id, got, want)
		}
	}
	for id, want := range map[string]string{
		"shipped-1": "", "shipped-2": "",
		"user-1": `{"author_id_in":["583231"]}`, "user-2": `{"author_id_in":["583231"]}`,
		"manual": "",
	} {
		if got := predicateOf("blueprint_runs", id); got.String != want || !got.Valid {
			t.Errorf("run %s recorded predicate after the upgrade = %+v, want %q", id, got, want)
		}
	}
}
