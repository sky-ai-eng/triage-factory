package db

import (
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The migration under test, and the migration before it, whose schema the
// test seeds.
const (
	interactiveConversationBackfill       int64 = 202611150001
	beforeInteractiveConversationBackfill int64 = 202611140002
)

// TestMigrate_InteractiveConversationBackfill: a person's claim mints the
// conversation they work the task in, and tasks a person held before that
// write existed have none. The migration mints one for every open task a
// person holds with no un-ended top-level conversation, shaped as the claim
// mints it, and leaves every other task as it is.
func TestMigrate_InteractiveConversationBackfill(t *testing.T) {
	database := openMigrationsTestDBAt(t, beforeInteractiveConversationBackfill)
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

	migrateThrough(t, database, interactiveConversationBackfill)

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
