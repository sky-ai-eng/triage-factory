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

// TestTaskStore_CloseWithRunCancelIntent_SQLite runs the shared close-carries-
// stop-intent suite against the SQLite impl. Each subtest gets a fresh
// in-memory DB, so the exact-count assertions can't pick up a sibling's rows.
func TestTaskStore_CloseWithRunCancelIntent_SQLite(t *testing.T) {
	dbtest.RunTaskCloseCancelIntentConformance(t, func(t *testing.T) (db.TaskStore, string, dbtest.TaskCloseCancelIntentSeeder) {
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

		// entityForTask lets Event() find the entity a seeded task hangs off,
		// so the closing event lands on the same entity production would put
		// it on. Keyed per factory call, which is per subtest.
		entityForTask := map[string]string{}

		seeder := dbtest.TaskCloseCancelIntentSeeder{
			Task: func(t *testing.T) string {
				t.Helper()
				entityID, _, taskID := seedSQLiteTaskChain(t, conn, "close-intent-"+uuid.New().String()[:8])
				entityForTask[taskID] = entityID
				return taskID
			},
			Event: func(t *testing.T, taskID string) string {
				t.Helper()
				eventID := uuid.New().String()
				if _, err := conn.Exec(`
					INSERT INTO events (id, entity_id, event_type, dedup_key, metadata_json, created_at)
					VALUES (?, ?, ?, '', '{}', ?)
				`, eventID, entityForTask[taskID], domain.EventGitHubPRMerged, time.Now().UTC()); err != nil {
					t.Fatalf("seed closing event: %v", err)
				}
				return eventID
			},
			Run: func(t *testing.T, taskID, blueprintStatus, convStatus string) (string, string) {
				t.Helper()
				brID := seedSQLiteBlueprintRun(t, conn, taskID, blueprintStatus)
				convID := uuid.New().String()
				stepIdx := 0
				insertConversationForTest(t, conn, domain.Conversation{
					ID: convID, TaskID: taskID, PromptID: seedSQLiteStepPrompt(t, conn),
					Status: convStatus, TriggerType: "event",
					BlueprintRunID: brID, BlueprintStepIndex: &stepIdx,
				})
				return brID, convID
			},
			BareRun: func(t *testing.T, taskID, convStatus string) string {
				t.Helper()
				convID := uuid.New().String()
				// origin='interactive': the origin CHECK demands the blueprint
				// parents only for 'blueprint', so this is how a conversation
				// with no sequence behind it is spelled.
				if _, err := conn.Exec(`
					INSERT INTO conversations (id, task_id, status, trigger_type, origin,
					                           team_id, visibility, creator_user_id)
					VALUES (?, ?, ?, 'manual', 'interactive', ?, 'team', ?)
				`, convID, taskID, nullIfEmptyString(convStatus), runmode.LocalDefaultTeamID,
					runmode.LocalDefaultUserID); err != nil {
					t.Fatalf("seed blueprintless conversation: %v", err)
				}
				return convID
			},
			CancelRequested: func(t *testing.T, blueprintRunID string) bool {
				t.Helper()
				var flag int
				if err := conn.QueryRow(`SELECT cancel_requested FROM blueprint_runs WHERE id = ?`, blueprintRunID).Scan(&flag); err != nil {
					t.Fatalf("read cancel_requested: %v", err)
				}
				return flag != 0
			},
			TaskStatus: func(t *testing.T, taskID string) string {
				t.Helper()
				var status string
				if err := conn.QueryRow(`SELECT status FROM tasks WHERE id = ?`, taskID).Scan(&status); err != nil {
					t.Fatalf("read task status: %v", err)
				}
				return status
			},
			CloseAuditCount: func(t *testing.T, taskID string) int {
				t.Helper()
				var n int
				if err := conn.QueryRow(`SELECT COUNT(*) FROM task_events WHERE task_id = ? AND kind = 'closed'`, taskID).Scan(&n); err != nil {
					t.Fatalf("count close audit rows: %v", err)
				}
				return n
			},
		}
		return sqlitestore.New(conn).Tasks, runmode.LocalDefaultOrgID, seeder
	})
}

// seedSQLiteBlueprintRun mints a blueprint + blueprint_run on the task in the
// named status. seedBlueprintRunForRun (the shared FK-target fixture) always
// writes 'running'; this suite needs a finished one too, since "a blueprint
// that already ended is never stamped" is half of what it asserts.
func seedSQLiteBlueprintRun(t *testing.T, conn *sql.DB, taskID, status string) string {
	t.Helper()
	blueprintID := "bp_" + uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO blueprints (id, name, source, creator_user_id, team_id)
		VALUES (?, 'bp', 'user', ?, ?)
	`, blueprintID, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID); err != nil {
		t.Fatalf("seed blueprint: %v", err)
	}
	stepPlan, err := domain.MarshalStepPlan([]domain.BlueprintPlanStep{
		{StepIndex: 0, PromptID: "seed-step", PromptName: "seed", PromptBody: "x", Source: "user"},
	})
	if err != nil {
		t.Fatalf("marshal seed step plan: %v", err)
	}
	brID := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, creator_user_id,
		                            status, worktree_path, step_plan)
		VALUES (?, ?, ?, 'manual', ?, ?, ?, ?)
	`, brID, blueprintID, taskID, runmode.LocalDefaultUserID, status, "/tmp/wt-"+brID, stepPlan); err != nil {
		t.Fatalf("seed blueprint_run: %v", err)
	}
	return brID
}

// seedSQLiteStepPrompt mints one prompt so a blueprint-origin conversation can
// satisfy the origin CHECK's prompt_id requirement.
func seedSQLiteStepPrompt(t *testing.T, conn *sql.DB) string {
	t.Helper()
	id := fmt.Sprintf("p_%s", uuid.New().String())
	if _, err := conn.Exec(
		`INSERT INTO prompts (id, name, body, creator_user_id, team_id) VALUES (?, 'Close Intent', 'body', ?, ?)`,
		id, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	return id
}

// nullIfEmptyString binds the mid-flight conversation status — SQL NULL, the
// state a conversation carries while it has reached no outcome.
func nullIfEmptyString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
