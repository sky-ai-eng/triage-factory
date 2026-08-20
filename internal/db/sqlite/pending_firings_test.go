package sqlite_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestPendingFiringsStore_SQLite runs the shared conformance suite
// against the SQLite PendingFiringsStore impl. Each subtest gets a
// fresh in-memory DB; the seeder closure builds entity/task/trigger/
// event/run fixtures inline with raw SQL since the schema's NOT NULL
// columns all carry DEFAULTs that the local sentinel constants
// satisfy.
func TestPendingFiringsStore_SQLite(t *testing.T) {
	dbtest.RunPendingFiringsStoreConformance(t, func(t *testing.T) (db.PendingFiringsStore, string, dbtest.PendingFiringsSeeder) {
		t.Helper()
		conn := newSQLiteForPendingFiringsTest(t)
		stores := sqlitestore.New(conn)
		return stores.PendingFirings, runmode.LocalDefaultOrgID, newSQLitePendingFiringsSeeder(conn)
	})
}

// TestPendingFiringsStore_SQLite_RejectsNonLocalOrg pins the
// assertLocalOrg guard on every method — important because these queries
// would otherwise return an empty result for any orgID rather than
// refusing it, which reads as "nothing queued" instead of "wrong tenant".
func TestPendingFiringsStore_SQLite_RejectsNonLocalOrg(t *testing.T) {
	conn := newSQLiteForPendingFiringsTest(t)
	stores := sqlitestore.New(conn)

	const bogusOrg = "11111111-1111-1111-1111-111111111111"
	ctx := t.Context()

	if _, _, err := stores.PendingFirings.Enqueue(ctx, bogusOrg, runmode.LocalDefaultUserID, "e", "t", "tr", "ev", db.AgentClaimStamp{}); err == nil {
		t.Errorf("Enqueue with non-local orgID should error")
	}
	if _, err := stores.PendingFirings.PopForTask(ctx, bogusOrg, "t"); err == nil {
		t.Errorf("PopForTask with non-local orgID should error")
	}
	if err := stores.PendingFirings.MarkFired(ctx, bogusOrg, 1, "r"); err == nil {
		t.Errorf("MarkFired with non-local orgID should error")
	}
	if err := stores.PendingFirings.MarkSkipped(ctx, bogusOrg, 1, "reason"); err == nil {
		t.Errorf("MarkSkipped with non-local orgID should error")
	}
	if _, err := stores.PendingFirings.HasPendingForTask(ctx, bogusOrg, "t"); err == nil {
		t.Errorf("HasPendingForTask with non-local orgID should error")
	}
	if _, err := stores.PendingFirings.ListTasksWithPending(ctx, bogusOrg); err == nil {
		t.Errorf("ListTasksWithPending with non-local orgID should error")
	}
	if _, err := stores.PendingFirings.ListForEntity(ctx, bogusOrg, "e"); err == nil {
		t.Errorf("ListForEntity with non-local orgID should error")
	}
}

func newSQLiteForPendingFiringsTest(t *testing.T) *sql.DB {
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
	return conn
}

// newSQLitePendingFiringsSeeder returns a closure-bound seeder bag.
// Every Tuple call inserts a fresh (entity, task, event_handler, event)
// chain so dedup keys stay distinct across subtests.
func newSQLitePendingFiringsSeeder(conn *sql.DB) dbtest.PendingFiringsSeeder {
	tuple := func(t *testing.T) dbtest.PendingFiringsTuple {
		t.Helper()
		suf := uuid.New().String()[:8]
		entityID := "e-" + suf
		eventID := "ev-" + suf
		taskID := "t-" + suf
		triggerID := "tr-" + suf
		promptID := "p-" + suf
		blueprintID := "bp-" + suf

		// entity: synthetic id keeps the (source, source_id)
		// UNIQUE happy across subtests.
		if _, err := conn.Exec(`
			INSERT INTO entities (id, source, source_id, kind, title, url)
			VALUES (?, 'github', ?, 'pr', 'Test PR', '')
		`, entityID, "owner/repo#"+suf); err != nil {
			t.Fatalf("seed entity: %v", err)
		}

		// prompt: triggers + conversations both FK to prompts(id).
		// source='user' requires creator_user_id non-null per the
		// prompts_system_has_no_creator CHECK.
		if _, err := conn.Exec(`
			INSERT INTO prompts (id, name, body, source, creator_user_id, team_id)
			VALUES (?, 'Test', 'x', 'user', ?, ?)
		`, promptID, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID); err != nil {
			t.Fatalf("seed prompt: %v", err)
		}

		// blueprint (+ step): the trigger's blueprint_id FKs to
		// blueprints(id, org_id) AND the same-team blueprints(id, team_id),
		// so it needs a real team-owned blueprint wrapping the prompt above.
		if _, err := sqlitestore.New(conn).Blueprints.Create(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Blueprint{
			ID: blueprintID, Name: "Test BP", Source: "user", TeamID: runmode.LocalDefaultTeamID,
		}); err != nil {
			t.Fatalf("seed blueprint: %v", err)
		}
		if _, err := sqlitestore.New(conn).Blueprints.ReplaceSteps(context.Background(), runmode.LocalDefaultOrgID, blueprintID, []string{promptID}, nil); err != nil {
			t.Fatalf("seed blueprint step: %v", err)
		}

		// event: pending_firings.triggering_event_id FKs to events(id).
		// event_type uses a real catalog entry so the
		// REFERENCES events_catalog(id) FK is satisfied.
		if _, err := conn.Exec(`
			INSERT INTO events (id, entity_id, event_type, dedup_key)
			VALUES (?, ?, ?, '')
		`, eventID, entityID, domain.EventGitHubPRCICheckFailed); err != nil {
			t.Fatalf("seed event: %v", err)
		}

		// task: pending_firings.task_id FKs to tasks(id). Defaults
		// cover org_id/team_id/creator_user_id/visibility.
		if _, err := conn.Exec(`
			INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id, status, scoring_status)
			VALUES (?, ?, ?, '', ?, 'queued', 'pending')
		`, taskID, entityID, domain.EventGitHubPRCICheckFailed, eventID); err != nil {
			t.Fatalf("seed task: %v", err)
		}

		// event_handler (trigger kind): FK target of
		// pending_firings.trigger_id. The kind-specific CHECK requires
		// triggers to set blueprint_id + breaker_threshold +
		// min_autonomy_suitability and to leave the rule-only columns
		// (name, default_priority, sort_order) NULL.
		if _, err := conn.Exec(`
			INSERT INTO event_handlers (id, kind, event_type, blueprint_id, breaker_threshold, min_autonomy_suitability, enabled, source, creator_user_id, team_id)
			VALUES (?, 'trigger', ?, ?, 4, 0, 1, 'user', ?, ?)
		`, triggerID, domain.EventGitHubPRCICheckFailed, blueprintID, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID); err != nil {
			t.Fatalf("seed trigger: %v", err)
		}

		return dbtest.PendingFiringsTuple{
			EntityID:  entityID,
			TaskID:    taskID,
			TriggerID: triggerID,
			EventID:   eventID,
			UserID:    runmode.LocalDefaultUserID,
		}
	}

	// runForTask inserts a blueprint + blueprint_run row so MarkFired's
	// fired_run_id FK to blueprint_runs(id) is satisfied — the firing unit is
	// the blueprint_run now. The conformance suite doesn't probe gate semantics
	// here.
	runForTask := func(t *testing.T, taskID string) string {
		t.Helper()
		bpID := "bp-pf-" + uuid.New().String()[:8]
		if _, err := conn.Exec(`INSERT INTO blueprints (id, name, source, team_id, creator_user_id) VALUES (?, 'bp', 'user', ?, ?)`,
			bpID, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID); err != nil {
			t.Fatalf("seed blueprint: %v", err)
		}
		brID := "bpr-pf-" + uuid.New().String()[:8]
		if _, err := conn.Exec(`
			INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, status, worktree_path, step_plan)
			VALUES (?, ?, ?, 'manual', 'running', '/tmp/wt', '[]')
		`, brID, bpID, taskID); err != nil {
			t.Fatalf("seed blueprint_run: %v", err)
		}
		return brID
	}

	// The claim-coupling subtests stamp a real agents row — tasks
	// .claimed_by_agent_id FKs agents(id), so a synthetic id wouldn't insert.
	// agents is UNIQUE (org_id), and local bootstrap already mints the org's
	// one bot, so adopt that row rather than racing its uniqueness.
	agentID := "agent-pf-" + uuid.New().String()[:8]
	if err := conn.QueryRow(`SELECT id FROM agents WHERE org_id = ?`, runmode.LocalDefaultOrgID).Scan(&agentID); err == sql.ErrNoRows {
		if _, err := conn.Exec(
			`INSERT INTO agents (id, org_id, display_name) VALUES (?, ?, 'PendingFirings Bot')`,
			agentID, runmode.LocalDefaultOrgID,
		); err != nil {
			panic(fmt.Sprintf("newSQLitePendingFiringsSeeder: seed agent: %v", err))
		}
	} else if err != nil {
		panic(fmt.Sprintf("newSQLitePendingFiringsSeeder: lookup agent: %v", err))
	}

	taskClaim := func(t *testing.T, taskID string) (string, string) {
		t.Helper()
		var agent, user sql.NullString
		if err := conn.QueryRow(
			`SELECT claimed_by_agent_id, claimed_by_user_id FROM tasks WHERE id = ?`, taskID,
		).Scan(&agent, &user); err != nil {
			t.Fatalf("read task claim: %v", err)
		}
		return agent.String, user.String
	}

	claimTaskForUser := func(t *testing.T, taskID string) {
		t.Helper()
		if _, err := conn.Exec(`
			UPDATE tasks SET claimed_by_user_id = ?, claimed_by_agent_id = NULL WHERE id = ?
		`, runmode.LocalDefaultUserID, taskID); err != nil {
			t.Fatalf("claim task for user: %v", err)
		}
	}

	return dbtest.PendingFiringsSeeder{
		Tuple:            tuple,
		RunForTask:       runForTask,
		AgentID:          agentID,
		TaskClaim:        taskClaim,
		ClaimTaskForUser: claimTaskForUser,
	}
}
