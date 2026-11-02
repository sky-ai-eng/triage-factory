package db

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The migration under test, and the last migration v1.17.0 ships, whose
// schema every test here seeds.
const (
	notificationsDeliverablesMergeability       int64 = 202610300001
	beforeNotificationsDeliverablesMergeability int64 = 202610290001
)

// openBeforeNotificationsDeliverablesMergeability returns a database at the
// v1.17.0 schema holding the local org, team and user, the conversation c-1,
// and the GitHub pull request entity e1 in repository r1.
func openBeforeNotificationsDeliverablesMergeability(t *testing.T) *sql.DB {
	t.Helper()
	database := openMigrationsTestDBAt(t, TestDSNMemory, beforeNotificationsDeliverablesMergeability)
	execAll(t, database,
		`INSERT INTO orgs (id, slug, name) VALUES ('`+runmode.LocalDefaultOrgID+`', 'local', 'Local')`,
		`INSERT INTO teams (id, org_id, slug, name) VALUES ('`+runmode.LocalDefaultTeamID+`', '`+runmode.LocalDefaultOrgID+`', 'default', 'Default')`,
		`INSERT INTO users (id, display_name) VALUES ('`+runmode.LocalDefaultUserID+`', 'Local')`,
		`INSERT INTO conversations (id, org_id, team_id, origin, status)
			VALUES ('c-1', '`+runmode.LocalDefaultOrgID+`', '`+runmode.LocalDefaultTeamID+`', 'interactive', 'open')`,
		`INSERT INTO repositories (id, org_id, owner, repo, source) VALUES ('r1', '`+runmode.LocalDefaultOrgID+`', 'octo', 'api', 'github')`,
		`INSERT INTO entities (id, org_id, source, scope, source_id, kind, repository_id)
			VALUES ('e1', '`+runmode.LocalDefaultOrgID+`', 'github', 'https://github.com', 'octo/api#1', 'pr', 'r1')`,
	)
	return database
}

func migrateNotificationsDeliverablesMergeability(t *testing.T, database *sql.DB) {
	t.Helper()
	migrateThrough(t, database, notificationsDeliverablesMergeability)
}

func execAll(t *testing.T, database *sql.DB, stmts ...string) {
	t.Helper()
	for _, stmt := range stmts {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", strings.Join(strings.Fields(stmt), " "), err)
		}
	}
}

func queryInt(t *testing.T, database *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := database.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// nullable binds an empty string as NULL.
func nullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// TestMigrate_LedgerPerformerAndObject pins the two ledger backfills. A row
// with no conversation is TF's own (system) without an actor and a person's
// with one, and every row with a conversation stays the agent's. A pull
// request action with an entity names the pull request, and a review submit
// or dismissal with a positive integer id names the review; every other row
// names no object. The performed_by CHECK refuses any other value.
func TestMigrate_LedgerPerformerAndObject(t *testing.T) {
	database := openBeforeNotificationsDeliverablesMergeability(t)

	type object struct{ kind, key string }
	rows := []struct {
		id, provider, action, conversationID, actorUserID, entityID, externalID string

		performedBy string
		object      object
	}{
		{id: "mirror", provider: "jira", action: "status_changed",
			performedBy: "system"},
		{id: "board-drag", provider: "jira", action: "status_changed", actorUserID: runmode.LocalDefaultUserID,
			performedBy: "person"},
		{id: "created", provider: "github", action: "pr_created", conversationID: "c-1", entityID: "e1",
			performedBy: "agent", object: object{"pull_request", ""}},
		{id: "merged", provider: "github", action: "pr_merged", conversationID: "c-1", actorUserID: runmode.LocalDefaultUserID, entityID: "e1",
			performedBy: "agent", object: object{"pull_request", ""}},
		{id: "unlinked", provider: "github", action: "pr_created", conversationID: "c-1",
			performedBy: "agent"},
		{id: "label", provider: "github", action: "label_added", conversationID: "c-1", entityID: "e1",
			performedBy: "agent"},
		{id: "submitted", provider: "github", action: "review_submitted", conversationID: "c-1", entityID: "e1", externalID: "4242",
			performedBy: "agent", object: object{"review", "github:review:4242"}},
		{id: "dismissed", provider: "github", action: "review_dismissed", actorUserID: runmode.LocalDefaultUserID, externalID: "77",
			performedBy: "person", object: object{"review", "github:review:77"}},
		{id: "no-id", provider: "github", action: "review_submitted", conversationID: "c-1", entityID: "e1", externalID: "0",
			performedBy: "agent"},
		{id: "not-numeric", provider: "github", action: "review_submitted", conversationID: "c-1", entityID: "e1", externalID: "12a",
			performedBy: "agent"},
	}
	for _, r := range rows {
		if _, err := database.Exec(`
			INSERT INTO external_actions (id, org_id, provider, scope, action, target, credential, dedup_key,
			                              conversation_id, actor_user_id, entity_id, external_id)
			VALUES (?, ?, ?, 'https://github.com', ?, 'octo/api#1', 'github_app', ?, ?, ?, ?, ?)`,
			r.id, runmode.LocalDefaultOrgID, r.provider, r.action, r.id,
			nullable(r.conversationID), nullable(r.actorUserID), nullable(r.entityID), nullable(r.externalID),
		); err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}

	migrateNotificationsDeliverablesMergeability(t, database)

	for _, r := range rows {
		var performedBy string
		var kind, key sql.NullString
		if err := database.QueryRow(`SELECT performed_by, object_kind, object_key FROM external_actions WHERE id = ?`, r.id).
			Scan(&performedBy, &kind, &key); err != nil {
			t.Fatalf("read %s: %v", r.id, err)
		}
		if performedBy != r.performedBy {
			t.Errorf("%s performed_by = %q, want %q", r.id, performedBy, r.performedBy)
		}
		if got := (object{kind.String, key.String}); got != r.object {
			t.Errorf("%s names %+v, want %+v", r.id, got, r.object)
		}
	}

	if _, err := database.Exec(`UPDATE external_actions SET performed_by = 'bot' WHERE id = 'mirror'`); err == nil {
		t.Error("performed_by accepted 'bot'; want the CHECK to refuse it")
	}
}

// TestMigrate_ArtifactsBecomeDeliverables pins the narrowing and the rename:
// the branch, issue, comment and message rows are deleted, and the pull
// request and review survive in deliverables, verified.
func TestMigrate_ArtifactsBecomeDeliverables(t *testing.T) {
	database := openBeforeNotificationsDeliverablesMergeability(t)
	for _, a := range []struct{ id, provider, kind, state string }{
		{"pr", "github", "pull_request", "draft"},
		{"review", "github", "review", "pending"},
		{"branch", "git", "branch", "pushed"},
		{"issue", "jira", "issue", "created"},
		{"comment", "github", "comment", "posted"},
		{"message", "slack", "message", "posted"},
	} {
		if _, err := database.Exec(`
			INSERT INTO artifacts (id, conversation_id, org_id, team_id, provider, kind, scope, target, state, dedup_key)
			VALUES (?, 'c-1', ?, ?, ?, ?, 'https://github.com', 'octo/api#1', ?, ?)`,
			a.id, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, a.provider, a.kind, a.state, a.id,
		); err != nil {
			t.Fatalf("seed %s: %v", a.id, err)
		}
	}

	migrateNotificationsDeliverablesMergeability(t, database)

	rows, err := database.Query(`SELECT id, verification FROM deliverables ORDER BY id`)
	if err != nil {
		t.Fatalf("read deliverables: %v", err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var id, verification string
		if err := rows.Scan(&id, &verification); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[id] = verification
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read deliverables: %v", err)
	}
	if len(got) != 2 || got["pr"] != "verified" || got["review"] != "verified" {
		t.Errorf("deliverables after the migration = %v, want pr and review, both verified", got)
	}
	if n := queryInt(t, database, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'artifacts'`); n != 0 {
		t.Error("the artifacts table survived the rename")
	}
}

// TestMigrate_NotifyRulesKeepHandlers pins the event_handlers rebuild: every
// rule and trigger survives with its fields, a conversation and a pending
// firing that name a trigger still name it, and the rebuilt table takes a
// notify rule.
func TestMigrate_NotifyRulesKeepHandlers(t *testing.T) {
	database := openBeforeNotificationsDeliverablesMergeability(t)
	if err := SeedEventTypes(database, "sqlite3"); err != nil {
		t.Fatalf("seed event types: %v", err)
	}
	execAll(t, database,
		`INSERT INTO blueprints (id, name, creator_user_id) VALUES ('bp1', 'Fix CI', '`+runmode.LocalDefaultUserID+`')`,
		`INSERT INTO event_handlers (id, creator_user_id, kind, event_type, scope_predicate_json, name, default_priority, sort_order)
			VALUES ('rule', '`+runmode.LocalDefaultUserID+`', 'rule', '`+domain.EventGitHubPRCICheckFailed+`', '{"repo":"octo/api"}', 'CI failed', 0.7, 3)`,
		`INSERT INTO event_handlers (id, creator_user_id, kind, event_type, blueprint_id, breaker_threshold, min_autonomy_suitability)
			VALUES ('trigger', '`+runmode.LocalDefaultUserID+`', 'trigger', '`+domain.EventGitHubPRCICheckFailed+`', 'bp1', 4, 0.6)`,
		`INSERT INTO events (id, entity_id, event_type) VALUES ('ev1', 'e1', '`+domain.EventGitHubPRCICheckFailed+`')`,
		`INSERT INTO tasks (id, entity_id, event_type, primary_event_id) VALUES ('t1', 'e1', '`+domain.EventGitHubPRCICheckFailed+`', 'ev1')`,
		`INSERT INTO conversations (id, origin, status, trigger_type, creator_user_id, trigger_id)
			VALUES ('c-trigger', 'interactive', 'open', 'event', NULL, 'trigger')`,
		`INSERT INTO pending_firings (status, max_attempts, first_enqueued_at, created_at, entity_id, task_id, trigger_id, triggering_event_id)
			VALUES ('ready', 3, '2026-10-01 00:00:00.000', '2026-10-01 00:00:00.000', 'e1', 't1', 'trigger', 'ev1')`,
	)

	migrateNotificationsDeliverablesMergeability(t, database)

	var predicate, name string
	var priority float64
	if err := database.QueryRow(`SELECT scope_predicate_json, name, default_priority FROM event_handlers WHERE id = 'rule' AND kind = 'rule'`).
		Scan(&predicate, &name, &priority); err != nil {
		t.Fatalf("read rule: %v", err)
	}
	if predicate != `{"repo":"octo/api"}` || name != "CI failed" || priority != 0.7 {
		t.Errorf("rule after the rebuild = (%s, %s, %v), want its fields kept", predicate, name, priority)
	}
	if n := queryInt(t, database, `SELECT COUNT(*) FROM event_handlers WHERE id = 'trigger' AND blueprint_id = 'bp1' AND breaker_threshold = 4`); n != 1 {
		t.Error("the trigger did not survive the rebuild with its fields")
	}
	if n := queryInt(t, database, `SELECT COUNT(*) FROM conversations WHERE trigger_id = 'trigger'`); n != 1 {
		t.Error("the conversation lost its trigger in the rebuild")
	}
	if n := queryInt(t, database, `SELECT COUNT(*) FROM pending_firings WHERE trigger_id = 'trigger'`); n != 1 {
		t.Error("the rebuild cascaded into the trigger's pending firing")
	}
	if n := queryInt(t, database, `SELECT COUNT(*) FROM pragma_foreign_key_check`); n != 0 {
		t.Errorf("%d foreign key violations after the migration", n)
	}
	if n := queryInt(t, database, `PRAGMA foreign_keys`); n != 1 {
		t.Error("foreign keys are off after the migration")
	}

	execAll(t, database, `
		INSERT INTO event_handlers (id, creator_user_id, kind, event_type, name, notify_audience)
		VALUES ('notify', '`+runmode.LocalDefaultUserID+`', 'notify', '`+domain.EventGitHubPRCICheckFailed+`', 'Tell me', 'creator')`)
}

// TestMigrate_HeldCloseIsPaired pins the held-close columns: a task the
// migration finds holds no close, and a write that sets one column of the
// pair without the other is refused while one that sets or clears both lands.
func TestMigrate_HeldCloseIsPaired(t *testing.T) {
	database := openBeforeNotificationsDeliverablesMergeability(t)
	if err := SeedEventTypes(database, "sqlite3"); err != nil {
		t.Fatalf("seed event types: %v", err)
	}
	execAll(t, database,
		`INSERT INTO events (id, entity_id, event_type) VALUES ('ev1', 'e1', '`+domain.EventGitHubPRCICheckFailed+`'),
		                                                       ('ev2', 'e1', '`+domain.EventGitHubPRCICheckFailed+`')`,
		`INSERT INTO tasks (id, entity_id, event_type, primary_event_id) VALUES ('t1', 'e1', '`+domain.EventGitHubPRCICheckFailed+`', 'ev1')`,
	)

	migrateNotificationsDeliverablesMergeability(t, database)

	if n := queryInt(t, database, `SELECT COUNT(*) FROM tasks WHERE close_deferred_event_id IS NULL AND close_deferred_at IS NULL`); n != 1 {
		t.Error("the task the migration found holds a close")
	}
	for _, half := range []string{
		`UPDATE tasks SET close_deferred_event_id = 'ev2' WHERE id = 't1'`,
		`UPDATE tasks SET close_deferred_at = '2026-11-01 00:00:00.000' WHERE id = 't1'`,
		`INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id, close_deferred_event_id)
		 VALUES ('t2', 'e1', '` + domain.EventGitHubPRCICheckFailed + `', 'x', 'ev1', 'ev2')`,
	} {
		_, err := database.Exec(half)
		if err == nil || !strings.Contains(err.Error(), "tasks_close_hold_paired") {
			t.Errorf("%s: err = %v, want the pairing refusal", strings.Join(strings.Fields(half), " "), err)
		}
	}
	execAll(t, database,
		`UPDATE tasks SET close_deferred_event_id = 'ev2', close_deferred_at = '2026-11-01 00:00:00.000' WHERE id = 't1'`,
		`UPDATE tasks SET close_deferred_event_id = NULL, close_deferred_at = NULL WHERE id = 't1'`,
	)
}

// TestMigrate_NotificationsDeliverablesMergeabilityFailureCommitsNothing
// forces a failure near the end of the file and checks that nothing before it
// committed, and that a retry runs the whole file.
func TestMigrate_NotificationsDeliverablesMergeabilityFailureCommitsNothing(t *testing.T) {
	database := openBeforeNotificationsDeliverablesMergeability(t)
	execAll(t, database,
		`INSERT INTO artifacts (id, conversation_id, org_id, team_id, provider, kind, scope, target, state, dedup_key)
			VALUES ('branch', 'c-1', '`+runmode.LocalDefaultOrgID+`', '`+runmode.LocalDefaultTeamID+`', 'git', 'branch', 'https://github.com', 'octo/api:b', 'pushed', 'branch')`,
		`CREATE TABLE branch_tips (id TEXT)`,
	)

	gooseMu.Lock()
	goose.SetBaseFS(migrationsSQLiteFS)
	upErr := goose.SetDialect("sqlite3")
	if upErr == nil {
		upErr = goose.UpTo(database, "migrations-sqlite", notificationsDeliverablesMergeability)
	}
	gooseMu.Unlock()
	if upErr == nil || !strings.Contains(upErr.Error(), "branch_tips") {
		t.Fatalf("goose.UpTo = %v, want the forced failure on branch_tips", upErr)
	}

	// What closing the connection does.
	if _, err := database.Exec(`ROLLBACK`); err != nil {
		t.Fatalf("ROLLBACK: %v — the failure left no transaction open, so the statements before it committed", err)
	}
	for _, name := range []string{"notifications", "deliverables", "pull_request_mergeability"} {
		if n := queryInt(t, database, `SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, name); n != 0 {
			t.Errorf("%s exists after the failed migration", name)
		}
	}
	if n := queryInt(t, database, `SELECT COUNT(*) FROM artifacts WHERE id = 'branch'`); n != 1 {
		t.Error("the branch artifact was deleted by the failed migration")
	}
	if n := queryInt(t, database, `SELECT COUNT(*) FROM goose_db_version WHERE version_id = ?`, notificationsDeliverablesMergeability); n != 0 {
		t.Error("the failed migration was recorded as applied")
	}

	execAll(t, database, `DROP TABLE branch_tips`)
	migrateNotificationsDeliverablesMergeability(t, database)
	if n := queryInt(t, database, `SELECT COUNT(*) FROM deliverables`); n != 0 {
		t.Errorf("deliverables holds %d rows after the retry, want the branch row gone", n)
	}
	if n := queryInt(t, database, `PRAGMA foreign_keys`); n != 1 {
		t.Error("foreign keys are off after the retry")
	}
}
