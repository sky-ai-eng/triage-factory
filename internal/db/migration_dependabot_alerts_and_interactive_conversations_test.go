package db

import (
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The migration under test, and the last migration v1.18.2 ships, whose
// schema every test here seeds.
const (
	dependabotAlertsInteractiveConversations       int64 = 202611120001
	beforeDependabotAlertsInteractiveConversations int64 = 202611090001
)

// TestMigrate_InteractiveConversationBackfill: a person's claim mints the
// conversation they work the task in, and tasks a person held before that
// write existed have none. The migration mints one for every open task a
// person holds with no un-ended top-level conversation, shaped as the claim
// mints it, and leaves every other task as it is.
func TestMigrate_InteractiveConversationBackfill(t *testing.T) {
	database := openMigrationsTestDBAt(t, beforeDependabotAlertsInteractiveConversations)
	org, team, user := runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID
	const agent = "00000000-0000-0000-0000-0000000000a1"
	execAll(t, database,
		`INSERT INTO orgs (id, slug, name) VALUES ('`+org+`', 'local', 'Local')`,
		`INSERT INTO teams (id, org_id, slug, name) VALUES ('`+team+`', '`+org+`', 'default', 'Default')`,
		`INSERT INTO users (id, display_name) VALUES ('`+user+`', 'Local')`,
		`INSERT INTO agents (id, org_id, display_name) VALUES ('`+agent+`', '`+org+`', 'Agent')`,
		`INSERT INTO repositories (id, org_id, owner, repo, source, host) VALUES ('r1', '`+org+`', 'octo', 'api', 'github', 'https://github.com')`,
	)

	// events.event_type references the catalog, which Migrate seeds after the
	// last migration and this database has not reached.
	if err := SeedEventTypes(database, "sqlite3"); err != nil {
		t.Fatalf("seed the events catalog: %v", err)
	}

	// Each task gets an entity and event of its own: one active task per
	// (entity, event type, dedup key).
	type seeded struct {
		id, status, claimedUser, claimedAgent string
		// conversations names the task's conversations to seed before the
		// migration, each "ended" or "unended".
		conversations []string
		// wantMinted says the migration mints this task a conversation.
		wantMinted bool
	}
	tasks := []seeded{
		{id: "held-none", status: "in_progress", claimedUser: user, wantMinted: true},
		{id: "held-ended-only", status: "in_progress", claimedUser: user, conversations: []string{"ended"}, wantMinted: true},
		{id: "held-has-one", status: "in_progress", claimedUser: user, conversations: []string{"ended", "unended"}},
		{id: "closed-done", status: "done", claimedUser: user},
		{id: "closed-dismissed", status: "dismissed", claimedUser: user},
		{id: "unclaimed", status: "queued"},
		{id: "agent-held", status: "in_progress", claimedAgent: agent},
	}
	for i, task := range tasks {
		entity, event := "e-"+task.id, "ev-"+task.id
		if _, err := database.Exec(`
			INSERT INTO entities (id, org_id, source, scope, source_id, kind, repository_id)
			VALUES (?, ?, 'github', 'https://github.com', ?, 'pr', 'r1')`,
			entity, org, "octo/api#"+string(rune('1'+i))); err != nil {
			t.Fatalf("seed entity for %s: %v", task.id, err)
		}
		if _, err := database.Exec(`INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES (?, ?, 'github:pr:opened', '')`,
			event, entity); err != nil {
			t.Fatalf("seed event for %s: %v", task.id, err)
		}
		if _, err := database.Exec(`
			INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status, claimed_by_user_id, claimed_by_agent_id)
			VALUES (?, ?, 'github:pr:opened', ?, ?, ?, ?)`,
			task.id, entity, event, task.status, nullable(task.claimedUser), nullable(task.claimedAgent)); err != nil {
			t.Fatalf("seed task %s: %v", task.id, err)
		}
		for j, shape := range task.conversations {
			var endedAt, endedReason any
			if shape == "ended" {
				endedAt, endedReason = "2026-11-01 00:00:00", "requeued"
			}
			if _, err := database.Exec(`
				INSERT INTO conversations (id, org_id, team_id, task_id, type, origin, status, ended_at, ended_reason)
				VALUES (?, ?, ?, ?, 'delegation', 'interactive', 'open', ?, ?)`,
				task.id+"-c"+string(rune('0'+j)), org, team, task.id, endedAt, endedReason); err != nil {
				t.Fatalf("seed conversation on %s: %v", task.id, err)
			}
		}
	}
	// A subagent row is not a conversation of the task's own, so it does not
	// stand in for one: its task is still minted a conversation.
	execAll(t, database,
		`INSERT INTO conversations (id, org_id, team_id, task_id, type, origin, status, ended_at, ended_reason)
			VALUES ('held-none-parent', '`+org+`', '`+team+`', 'held-none', 'delegation', 'interactive', 'open', '2026-11-01 00:00:00', 'requeued')`,
		`INSERT INTO conversations (id, org_id, team_id, task_id, type, origin, status, parent_conversation_id)
			VALUES ('held-none-sub', '`+org+`', '`+team+`', 'held-none', 'subagent:test', 'interactive', 'open', 'held-none-parent')`,
	)

	before := map[string]int{}
	for _, task := range tasks {
		before[task.id] = queryInt(t, database, `SELECT COUNT(*) FROM conversations WHERE task_id = ?`, task.id)
	}

	migrateThrough(t, database, dependabotAlertsInteractiveConversations)

	for _, task := range tasks {
		after := queryInt(t, database, `SELECT COUNT(*) FROM conversations WHERE task_id = ?`, task.id)
		minted := after - before[task.id]
		want := 0
		if task.wantMinted {
			want = 1
		}
		if minted != want {
			t.Errorf("task %s: the migration minted %d conversations, want %d", task.id, minted, want)
			continue
		}
		if !task.wantMinted {
			continue
		}
		// Exactly one un-ended top-level conversation, shaped as a claim
		// mints it.
		if n := queryInt(t, database, `
			SELECT COUNT(*) FROM conversations
			WHERE task_id = ? AND ended_at IS NULL AND parent_conversation_id IS NULL`, task.id); n != 1 {
			t.Errorf("task %s: %d un-ended top-level conversations after the migration, want one", task.id, n)
			continue
		}
		var id, orgID, convType, runtime, triggerType, origin, teamID, visibility, creator, status string
		var parkedAt, startedAt sql.NullString
		var nonNull int
		if err := database.QueryRow(`
			SELECT id, org_id, type, runtime, trigger_type, origin, team_id, visibility, creator_user_id, status,
			       parked_at, started_at,
			       (prompt_id IS NOT NULL) + (model IS NOT NULL) + (worktree_path IS NOT NULL) + (trigger_id IS NOT NULL)
			       + (actor_agent_id IS NOT NULL) + (blueprint_run_id IS NOT NULL) + (blueprint_step_index IS NOT NULL)
			       + (preferred_executor_id IS NOT NULL) + (park_reason IS NOT NULL) + (completed_at IS NOT NULL)
			       + (outcome IS NOT NULL) + (outcome_reason IS NOT NULL) + (queued_at IS NOT NULL)
			       + (sdk_session_id IS NOT NULL)
			FROM conversations
			WHERE task_id = ? AND ended_at IS NULL AND parent_conversation_id IS NULL`, task.id,
		).Scan(&id, &orgID, &convType, &runtime, &triggerType, &origin, &teamID, &visibility, &creator, &status,
			&parkedAt, &startedAt, &nonNull); err != nil {
			t.Fatalf("task %s: read the minted conversation: %v", task.id, err)
		}
		got := [...]string{orgID, convType, runtime, triggerType, origin, teamID, visibility, creator, status}
		wantRow := [...]string{org, "interactive", "sdk", "manual", "interactive", team, "team", task.claimedUser, "open"}
		if got != wantRow {
			t.Errorf("task %s: minted conversation = %v, want %v", task.id, got, wantRow)
		}
		if !parkedAt.Valid || !startedAt.Valid {
			t.Errorf("task %s: parked_at set = %v, started_at set = %v; want both stamped", task.id, parkedAt.Valid, startedAt.Valid)
		}
		if nonNull != 0 {
			t.Errorf("task %s: %d of the columns a claim's mint leaves NULL hold values", task.id, nonNull)
		}
		// A v4 UUID, as every conversation id the store mints is.
		if len(id) != 36 || id[14] != '4' {
			t.Errorf("task %s: minted id %q is not a v4 UUID", task.id, id)
		}
	}
}

// TestMigrate_RenameIdleParkReason: every stored `idle` park reads
// `turn_incomplete`, and no other reason, or a row with none, changes.
func TestMigrate_RenameIdleParkReason(t *testing.T) {
	database := openMigrationsTestDBAt(t, beforeDependabotAlertsInteractiveConversations)
	org, team := runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID
	execAll(t, database,
		`INSERT INTO orgs (id, slug, name) VALUES ('`+org+`', 'local', 'Local')`,
		`INSERT INTO teams (id, org_id, slug, name) VALUES ('`+team+`', '`+org+`', 'default', 'Default')`,
		`INSERT INTO users (id, display_name) VALUES ('`+runmode.LocalDefaultUserID+`', 'Local')`,
	)

	rows := []struct{ id, reason, want string }{
		{id: "idle", reason: "idle", want: "turn_incomplete"},
		{id: "stopped", reason: "user_cancelled", want: "user_cancelled"},
		{id: "stalled", reason: "stalled", want: "stalled"},
		{id: "no-reason", reason: "", want: ""},
	}
	for _, r := range rows {
		if _, err := database.Exec(`
			INSERT INTO conversations (id, org_id, team_id, origin, status, park_reason)
			VALUES (?, ?, ?, 'interactive', 'open', ?)`,
			r.id, org, team, nullable(r.reason)); err != nil {
			t.Fatalf("seed conversation %s: %v", r.id, err)
		}
	}

	migrateThrough(t, database, dependabotAlertsInteractiveConversations)

	for _, r := range rows {
		var got sql.NullString
		if err := database.QueryRow(`SELECT park_reason FROM conversations WHERE id = ?`, r.id).Scan(&got); err != nil {
			t.Fatalf("read conversation %s: %v", r.id, err)
		}
		if got.String != r.want || got.Valid != (r.want != "") {
			t.Errorf("conversation %s: park_reason = %v, want %q", r.id, got, r.want)
		}
	}
}

// TestMigrate_BlueprintRunTeamBackfill: an existing run takes the team of its
// lowest-index step conversation, whatever team a later step is on, and a run
// with no conversation keeps no team.
func TestMigrate_BlueprintRunTeamBackfill(t *testing.T) {
	database := openMigrationsTestDBAt(t, beforeDependabotAlertsInteractiveConversations)
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

	migrateThrough(t, database, dependabotAlertsInteractiveConversations)

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

// TestMigrate_SDKSessionCostBackfill: an un-ended conversation with a session
// starts from its newest cost row, whatever rows with no cost follow it, and
// an ended conversation, one with no cost row, and one with no session start
// from zero.
func TestMigrate_SDKSessionCostBackfill(t *testing.T) {
	database := openMigrationsTestDBAt(t, beforeDependabotAlertsInteractiveConversations)
	org, team, user := runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID
	execAll(t, database,
		`INSERT INTO orgs (id, slug, name) VALUES ('`+org+`', 'local', 'Local')`,
		`INSERT INTO teams (id, org_id, slug, name) VALUES ('`+team+`', '`+org+`', 'default', 'Default')`,
		`INSERT INTO users (id, display_name) VALUES ('`+user+`', 'Local')`,
		`INSERT INTO conversations (id, org_id, team_id, type, origin, status, sdk_session_id)
			VALUES ('c-live', '`+org+`', '`+team+`', 'interactive', 'interactive', 'open', 's-live')`,
		`INSERT INTO conversations (id, org_id, team_id, type, origin, status, sdk_session_id, ended_at, ended_reason)
			VALUES ('c-ended', '`+org+`', '`+team+`', 'interactive', 'interactive', 'open', 's-ended', '2026-11-01 00:00:00', 'requeued')`,
		`INSERT INTO conversations (id, org_id, team_id, type, origin, status, sdk_session_id)
			VALUES ('c-no-cost', '`+org+`', '`+team+`', 'interactive', 'interactive', 'open', 's-no-cost')`,
		`INSERT INTO conversations (id, org_id, team_id, type, origin, status)
			VALUES ('c-no-session', '`+org+`', '`+team+`', 'interactive', 'interactive', 'open')`,
		// The live conversation's newest cost row is followed by a person's
		// next message, which carries none.
		`INSERT INTO messages (conversation_id, role, cost_usd) VALUES ('c-live', 'assistant', 0.00697188)`,
		`INSERT INTO messages (conversation_id, role, cost_usd) VALUES ('c-live', 'user', NULL)`,
		`INSERT INTO messages (conversation_id, role, cost_usd) VALUES ('c-live', 'assistant', 0.00825322)`,
		`INSERT INTO messages (conversation_id, role, cost_usd) VALUES ('c-live', 'user', NULL)`,
		`INSERT INTO messages (conversation_id, role, cost_usd) VALUES ('c-ended', 'assistant', 0.5)`,
		`INSERT INTO messages (conversation_id, role, cost_usd) VALUES ('c-no-cost', 'assistant', NULL)`,
		`INSERT INTO messages (conversation_id, role, cost_usd) VALUES ('c-no-session', 'assistant', 0.25)`,
	)

	migrateThrough(t, database, dependabotAlertsInteractiveConversations)

	for _, tc := range []struct {
		conversation string
		want         float64
	}{
		{"c-live", 0.00825322},
		{"c-ended", 0},
		{"c-no-cost", 0},
		{"c-no-session", 0},
	} {
		var got float64
		if err := database.QueryRow(`SELECT sdk_session_cost_usd FROM conversations WHERE id = ?`, tc.conversation).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", tc.conversation, err)
		}
		if got != tc.want {
			t.Errorf("%s sdk_session_cost_usd = %v, want %v", tc.conversation, got, tc.want)
		}
	}
}
