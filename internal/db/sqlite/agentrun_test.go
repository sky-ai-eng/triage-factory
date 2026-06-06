package sqlite_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestAgentRunStore_SQLite runs the shared conformance suite against
// the SQLite AgentRunStore impl. Each subtest gets a fresh in-memory
// DB so the run lifecycle assertions don't bleed across.
func TestAgentRunStore_SQLite(t *testing.T) {
	dbtest.RunAgentRunStoreConformance(t, func(t *testing.T) (db.AgentRunStore, string, string, dbtest.AgentRunSeeder) {
		t.Helper()
		conn := newSQLiteForAgentRunTest(t)
		seed := newSQLiteAgentRunSeeder(conn)
		stores := sqlitestore.New(conn)
		return stores.AgentRuns, runmode.LocalDefaultOrg, runmode.LocalDefaultUserID, seed
	})
}

// newSQLiteForAgentRunTest opens an in-memory DB, bootstraps the
// schema, and seeds the local default agent + the conformance
// prompt. Returned connection is t.Cleanup-closed.
func newSQLiteForAgentRunTest(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	// agents row backs runs.actor_agent_id and task claim stamps —
	// migration seeds the sentinel user/team but not the agent row
	// itself (production does that via BootstrapLocalAgent).
	if _, err := conn.Exec(
		`INSERT OR IGNORE INTO agents (id, org_id, display_name) VALUES (?, ?, 'Test Bot')`,
		runmode.LocalDefaultAgentID, runmode.LocalDefaultOrgID,
	); err != nil {
		t.Fatalf("seed local agent: %v", err)
	}
	// Conformance suite's run.PromptID points at this stable ID.
	if _, err := conn.Exec(
		`INSERT INTO prompts (id, name, body, creator_user_id, team_id) VALUES ('p_agentrun_test', 'Test', 'body', ?, ?)`,
		runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID,
	); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	return conn
}

// newSQLiteAgentRunSeeder returns the FactorySeeder-style bag of
// callbacks the conformance suite drives. Raw SQL keeps the seeder
// independent of the store under test.
func newSQLiteAgentRunSeeder(conn *sql.DB) dbtest.AgentRunSeeder {
	return dbtest.AgentRunSeeder{
		Entity: func(t *testing.T, suffix string) string {
			t.Helper()
			id := uuid.New().String()
			sourceID := fmt.Sprintf("agentrun-%s-%s", suffix, id[:8])
			if _, err := conn.Exec(`
				INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, created_at)
				VALUES (?, 'github', ?, 'pr', ?, ?, '{}', ?)
			`, id, sourceID, "Conformance "+suffix, "https://example/"+sourceID, time.Now().UTC()); err != nil {
				t.Fatalf("seed entity %s: %v", suffix, err)
			}
			return id
		},
		Event: func(t *testing.T, entityID, eventType string) string {
			t.Helper()
			id := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO events (id, entity_id, event_type, dedup_key, metadata_json, created_at)
				VALUES (?, ?, ?, '', '{}', ?)
			`, id, entityID, eventType, time.Now().UTC()); err != nil {
				t.Fatalf("seed event: %v", err)
			}
			return id
		},
		Task: func(t *testing.T, entityID, eventType, primaryEventID string) string {
			t.Helper()
			id := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id,
				                   status, priority_score, scoring_status, created_at,
				                   team_id, visibility)
				VALUES (?, ?, ?, '', ?, 'queued', 0.5, 'pending', ?, ?, 'team')
			`, id, entityID, eventType, primaryEventID, time.Now().UTC(), runmode.LocalDefaultTeamID); err != nil {
				t.Fatalf("seed task: %v", err)
			}
			return id
		},
		StampAgentClaim: func(t *testing.T, taskID, agentID string) {
			t.Helper()
			if _, err := conn.Exec(
				`UPDATE tasks SET claimed_by_agent_id = ?, claimed_by_user_id = NULL WHERE id = ?`,
				agentID, taskID,
			); err != nil {
				t.Fatalf("stamp claim: %v", err)
			}
		},
		EventHandler: func(t *testing.T, eventType string) string {
			t.Helper()
			id := uuid.New().String()
			// Minimal rule shape: name/default_priority/sort_order non-NULL,
			// trigger-only cols NULL (the event_handlers rule CHECK). org_id
			// takes its local-sentinel DEFAULT.
			if _, err := conn.Exec(`
				INSERT INTO event_handlers
					(id, creator_user_id, team_id, kind, event_type, enabled, source,
					 name, default_priority, sort_order)
				VALUES (?, ?, ?, 'rule', ?, 1, 'user', 'fence-fk', 0.5, 100)
			`, id, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID, eventType); err != nil {
				t.Fatalf("seed event_handler: %v", err)
			}
			return id
		},
		BlueprintRun: func(t *testing.T, taskID string) string {
			t.Helper()
			// runs.blueprint_run_id is NOT NULL — a single prompt is a
			// 1-step blueprint, so every run needs a parent blueprint_run.
			// Mint a fresh blueprint + blueprint_run per call. SQLite
			// blueprint_runs has no org_id/creator_user_id columns; org_id
			// on blueprints takes its local-sentinel DEFAULT.
			bpID := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO blueprints (id, name, source, team_id, creator_user_id)
				VALUES (?, 'Conformance BP', 'user', ?, ?)
			`, bpID, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID); err != nil {
				t.Fatalf("seed blueprint: %v", err)
			}
			brID := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, status, worktree_path, step_plan)
				VALUES (?, ?, ?, 'manual', 'running', '/tmp/wt', '[]')
			`, brID, bpID, taskID); err != nil {
				t.Fatalf("seed blueprint_run: %v", err)
			}
			return brID
		},
		SetRunMemory: func(t *testing.T, runID, entityID, content string) {
			t.Helper()
			memID := uuid.New().String()
			if content == dbtest.NullMemorySentinel {
				if _, err := conn.Exec(`
					INSERT INTO run_memory (id, run_id, entity_id, agent_content) VALUES (?, ?, ?, NULL)
				`, memID, runID, entityID); err != nil {
					t.Fatalf("seed null memory: %v", err)
				}
				return
			}
			if _, err := conn.Exec(`
				INSERT INTO run_memory (id, run_id, entity_id, agent_content) VALUES (?, ?, ?, ?)
			`, memID, runID, entityID, content); err != nil {
				t.Fatalf("seed memory: %v", err)
			}
		},
		AgentID: runmode.LocalDefaultAgentID,
	}
}

// TestAgentRunStore_SQLite_MarkTakenOver_SavepointRollbackInsideTx
// pins the SKY-285 review-bot finding: when AgentRunStore is composed
// inside an outer WithTx and MarkTakenOver hits a claim-race, the
// run UPDATE must NOT survive the outer commit. Pre-fix the run
// flip leaked through to the parent tx; the savepoint inside
// runScoped rolls it back cleanly.
//
// Scenario: seed a running run on a task with NO agent claim, call
// MarkTakenOver with a non-empty claimUserID from inside WithTx.
// The run UPDATE matches (status='running'), the task UPDATE
// affects 0 rows (no agent claim to vacate), savepoint rolls back.
// Outer tx commits. Assertion: run is still 'running' after
// commit.
func TestAgentRunStore_SQLite_MarkTakenOver_SavepointRollbackInsideTx(t *testing.T) {
	conn := newSQLiteForAgentRunTest(t)
	stores := sqlitestore.New(conn)
	seed := newSQLiteAgentRunSeeder(conn)
	ctx := t.Context()

	ent := seed.Entity(t, "savepoint")
	ev := seed.Event(t, ent, "github:pr:opened")
	taskID := seed.Task(t, ent, "github:pr:opened", ev)
	// Deliberately do NOT stamp a bot claim on the task — the claim
	// UPDATE inside MarkTakenOver will affect 0 rows and trigger
	// errScopedRollback. Run starts in 'running'.
	runID := "test-run-savepoint"
	if err := stores.AgentRuns.Create(ctx, runmode.LocalDefaultOrg, domain.AgentRun{
		ID: runID, TaskID: taskID, PromptID: "p_agentrun_test", Status: "running", Model: "m",
		BlueprintRunID: seed.BlueprintRun(t, taskID),
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := stores.Tx.WithTx(ctx, runmode.LocalDefaultOrg, runmode.LocalDefaultUserID, func(tx db.TxStores) error {
		ok, err := tx.AgentRuns.MarkTakenOver(ctx, runmode.LocalDefaultOrg, runID, "/tmp/savepoint-test", runmode.LocalDefaultUserID)
		if err != nil {
			return err
		}
		if ok {
			t.Errorf("MarkTakenOver returned true; want false (claim race-loss)")
		}
		// Return nil so the outer tx COMMITS. The savepoint should
		// have rolled back the partial run UPDATE; the commit only
		// persists nothing for AgentRuns.
		return nil
	}); err != nil {
		t.Fatalf("WithTx: %v", err)
	}

	// Read the run back. Status must still be 'running' — if the
	// savepoint didn't roll back the inner UPDATE, the outer commit
	// would have persisted status='taken_over'.
	got, err := stores.AgentRuns.Get(ctx, runmode.LocalDefaultOrg, runID)
	if err != nil || got == nil {
		t.Fatalf("Get: err=%v got=%v", err, got)
	}
	if got.Status != "running" {
		t.Errorf("run.Status = %q after composed-tx race-loss; want 'running' (savepoint rollback failed)", got.Status)
	}
}

// TestAgentRunStore_SQLite_AssertLocalOrg pins the local-only invariant:
// the orgID guard at every method entry refuses non-LocalDefaultOrg.
// The conformance suite exercises the happy path; this test pins the
// SQLite-specific rejection.
func TestAgentRunStore_SQLite_AssertLocalOrg(t *testing.T) {
	conn := newSQLiteForAgentRunTest(t)
	store := sqlitestore.New(conn).AgentRuns
	if _, err := store.HasActiveForTask(t.Context(), "some-other-org", uuid.New().String()); err == nil {
		t.Error("HasActiveForTask accepted non-LocalDefaultOrg without error")
	}
}
