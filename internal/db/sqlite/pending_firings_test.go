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
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestPendingFiringsStore_SQLite runs the shared conformance suite against
// the SQLite PendingFiringsStore impl. Each subtest gets a fresh in-memory
// DB; the seeder closure builds entity/task/trigger/event/run fixtures
// inline with raw SQL since the schema's NOT NULL columns all carry
// DEFAULTs that the local sentinel constants satisfy.
func TestPendingFiringsStore_SQLite(t *testing.T) {
	dbtest.RunPendingFiringsStoreConformance(t, func(t *testing.T) (db.PendingFiringsStore, string, dbtest.PendingFiringsSeeder) {
		t.Helper()
		conn := newSQLiteForPendingFiringsTest(t)
		stores := sqlitestore.New(conn)
		return stores.PendingFirings, runmode.LocalDefaultOrgID, newSQLitePendingFiringsSeeder(conn, stores)
	})
}

// TestPendingFiringsStore_SQLite_RejectsNonLocalOrg pins the assertLocalOrg
// guard on every org-scoped method — important because these queries would
// otherwise return an empty result for any orgID rather than refusing it,
// which reads as "nothing queued" instead of "wrong tenant".
func TestPendingFiringsStore_SQLite_RejectsNonLocalOrg(t *testing.T) {
	conn := newSQLiteForPendingFiringsTest(t)
	stores := sqlitestore.New(conn)

	const bogusOrg = "11111111-1111-1111-1111-111111111111"
	ctx := t.Context()
	bogusReceipt := workitem.Receipt{ItemID: 1, OrgID: bogusOrg, LeaseGeneration: 1}

	if _, _, err := stores.PendingFirings.Enqueue(ctx, bogusOrg, "e", "t", "tr", "ev", db.AgentClaimStamp{}); err == nil {
		t.Errorf("Enqueue with non-local orgID should error")
	}
	if _, err := stores.PendingFirings.RenewLease(ctx, bogusReceipt); err == nil {
		t.Errorf("RenewLease with non-local orgID should error")
	}
	if err := stores.PendingFirings.MarkFired(ctx, bogusReceipt, "r"); err == nil {
		t.Errorf("MarkFired with non-local orgID should error")
	}
	if err := stores.PendingFirings.MarkSkipped(ctx, bogusReceipt, "reason"); err == nil {
		t.Errorf("MarkSkipped with non-local orgID should error")
	}
	if _, err := stores.PendingFirings.Requeue(ctx, bogusReceipt, workitem.OutcomeTransient, nil); err == nil {
		t.Errorf("Requeue with non-local orgID should error")
	}
	if err := stores.PendingFirings.DeferWhileTaskBusy(ctx, bogusReceipt); err == nil {
		t.Errorf("DeferWhileTaskBusy with non-local orgID should error")
	}
	if _, err := stores.PendingFirings.HasUnsettledForTask(ctx, bogusOrg, "t"); err == nil {
		t.Errorf("HasUnsettledForTask with non-local orgID should error")
	}
	if _, err := stores.PendingFirings.ListForEntity(ctx, bogusOrg, "e"); err == nil {
		t.Errorf("ListForEntity with non-local orgID should error")
	}
	if _, err := stores.PendingFirings.(db.WorkKindHandle).Describe(ctx, bogusOrg, []int64{1}); err == nil {
		t.Errorf("Describe with non-local orgID should error")
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

// newSQLitePendingFiringsSeeder returns a closure-bound seeder bag. Every
// Tuple call inserts a fresh (entity, task, event_handler, event) chain so
// dedup keys stay distinct across subtests.
func newSQLitePendingFiringsSeeder(conn *sql.DB, stores db.Stores) dbtest.PendingFiringsSeeder {
	tuple := func(t *testing.T) dbtest.PendingFiringsTuple {
		t.Helper()
		suf := uuid.New().String()[:8]
		entityID := "e-" + suf
		eventID := "ev-" + suf
		taskID := "t-" + suf
		triggerID := "tr-" + suf
		promptID := "p-" + suf
		blueprintID := "bp-" + suf

		// entity: synthetic id keeps the (source, source_id) UNIQUE happy
		// across subtests.
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
		if _, err := stores.Blueprints.Create(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Blueprint{
			ID: blueprintID, Name: "Test BP", Source: "user", TeamID: runmode.LocalDefaultTeamID,
		}); err != nil {
			t.Fatalf("seed blueprint: %v", err)
		}
		if _, err := stores.Blueprints.ReplaceSteps(context.Background(), runmode.LocalDefaultOrgID, blueprintID, []string{promptID}, nil); err != nil {
			t.Fatalf("seed blueprint step: %v", err)
		}

		// event: pending_firings.triggering_event_id FKs to events(id).
		// event_type uses a real catalog entry so the REFERENCES
		// events_catalog(id) FK is satisfied.
		if _, err := conn.Exec(`
			INSERT INTO events (id, entity_id, event_type, dedup_key)
			VALUES (?, ?, ?, '')
		`, eventID, entityID, domain.EventGitHubPRCICheckFailed); err != nil {
			t.Fatalf("seed event: %v", err)
		}

		// task: pending_firings.task_id FKs to tasks(id). Defaults cover
		// org_id/team_id/creator_user_id/visibility.
		if _, err := conn.Exec(`
			INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id, status, scoring_status)
			VALUES (?, ?, ?, '', ?, 'queued', 'pending')
		`, taskID, entityID, domain.EventGitHubPRCICheckFailed, eventID); err != nil {
			t.Fatalf("seed task: %v", err)
		}

		// event_handler (trigger kind): FK target of pending_firings
		// .trigger_id. The kind-specific CHECK requires triggers to set
		// blueprint_id + breaker_threshold + min_autonomy_suitability and to
		// leave the rule-only columns (name, default_priority, sort_order)
		// NULL.
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
			PromptID:  promptID,
		}
	}

	// runForTask inserts a blueprint + blueprint_run row so MarkFired's
	// fired_run_id FK to blueprint_runs(id) is satisfied — the firing unit
	// is the blueprint_run.
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

	// A claim lands the row in progress, the way every claim door does —
	// the tasks_queue_unclaimed CHECK refuses the held-queued row otherwise.
	claimTaskForUser := func(t *testing.T, taskID string) {
		t.Helper()
		if _, err := conn.Exec(`
			UPDATE tasks
			   SET claimed_by_user_id = ?, claimed_by_agent_id = NULL,
			       status = CASE WHEN status IN ('queued', 'snoozed') THEN 'in_progress' ELSE status END
			 WHERE id = ?
		`, runmode.LocalDefaultUserID, taskID); err != nil {
			t.Fatalf("claim task for user: %v", err)
		}
	}

	return dbtest.PendingFiringsSeeder{
		Tuple:      tuple,
		RunForTask: runForTask,
		SettleRuns: func(t *testing.T, taskID string) {
			t.Helper()
			if _, err := conn.Exec(`UPDATE blueprint_runs SET status = 'completed' WHERE task_id = ? AND status = 'running'`, taskID); err != nil {
				t.Fatalf("settle blueprint_runs: %v", err)
			}
		},
		AgentID:          agentID,
		TaskClaim:        taskClaim,
		ClaimTaskForUser: claimTaskForUser,
		LiveConversation: func(t *testing.T, taskID, promptID string) string {
			t.Helper()
			id := uuid.New().String()
			dbtest.SeedConversation(t, conn, domain.Conversation{ID: id, TaskID: taskID, PromptID: promptID, Model: "m", TriggerType: "event"})
			return id
		},
		EndConversation: func(t *testing.T, conversationID string) {
			t.Helper()
			execOne(t, conn, "end conversation",
				`UPDATE conversations SET status = 'completed', ended_at = CURRENT_TIMESTAMP, ended_reason = ? WHERE id = ?`,
				string(domain.EndedStepAdvanced), conversationID)
		},
		ExpireLease: func(t *testing.T, firingID int64) {
			t.Helper()
			execOne(t, conn, "expire lease",
				`UPDATE pending_firings SET lease_expires_at = strftime('%Y-%m-%d %H:%M:%f','now','-1 hours') WHERE id = ? AND status = 'leased'`, firingID)
		},
		Ripen: func(t *testing.T, firingID int64) {
			t.Helper()
			execOne(t, conn, "ripen", `UPDATE pending_firings SET next_attempt_at = NULL WHERE id = ? AND status = 'ready'`, firingID)
		},
		InTx: func(t *testing.T, fn func(s db.PendingFiringsStore) error) error {
			t.Helper()
			return stores.Tx.WithTx(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(tx db.TxStores) error {
				return fn(tx.PendingFirings)
			})
		},
	}
}
