package sqlite_test

import (
	"database/sql"
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

// TestBreaker_SQLite runs the shared breaker suite against the SQLite impl,
// a fresh in-memory DB per subtest.
func TestBreaker_SQLite(t *testing.T) {
	dbtest.RunBreakerConformance(t, func(t *testing.T) (db.TaskStore, string, dbtest.BreakerSeeder) {
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
		return sqlitestore.New(conn).Tasks, runmode.LocalDefaultOrgID, newSQLiteBreakerSeeder(conn)
	})
}

func newSQLiteBreakerSeeder(conn *sql.DB) dbtest.BreakerSeeder {
	exec := func(t *testing.T, what, query string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(query, args...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	eventOf := map[string]string{}
	return dbtest.BreakerSeeder{
		Trigger: func(t *testing.T, threshold int) (string, string, string) {
			t.Helper()
			promptID := "p_" + uuid.New().String()
			blueprintID := "bp_" + uuid.New().String()
			triggerID := uuid.New().String()
			exec(t, "seed prompt", `INSERT INTO prompts (id, name, body, creator_user_id, team_id) VALUES (?, 'Breaker', 'x', ?, ?)`,
				promptID, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID)
			exec(t, "seed blueprint", `INSERT INTO blueprints (id, name, source, creator_user_id, team_id) VALUES (?, 'Breaker', 'user', ?, ?)`,
				blueprintID, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID)
			exec(t, "seed blueprint step", `INSERT INTO blueprint_steps (blueprint_id, step_index, step_prompt_id, team_id) VALUES (?, 0, ?, ?)`,
				blueprintID, promptID, runmode.LocalDefaultTeamID)
			exec(t, "seed trigger", `
				INSERT INTO event_handlers (id, creator_user_id, team_id, kind, event_type, source,
				                            blueprint_id, breaker_threshold, min_autonomy_suitability)
				VALUES (?, ?, ?, 'trigger', ?, 'user', ?, ?, 0)
			`, triggerID, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID, domain.EventGitHubPRCICheckFailed, blueprintID, threshold)
			return triggerID, blueprintID, promptID
		},
		Task: func(t *testing.T) (string, string) {
			t.Helper()
			entityID, eventID, taskID := seedSQLiteTaskChain(t, conn, "breaker-"+uuid.New().String()[:8])
			eventOf[entityID] = eventID
			return entityID, taskID
		},
		TaskOnEntity: func(t *testing.T, entityID string) string {
			t.Helper()
			taskID := uuid.New().String()
			exec(t, "seed task on entity", `
				INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id,
				                   status, priority_score, scoring_status, team_id, visibility)
				VALUES (?, ?, ?, ?, ?, 'queued', 0.5, 'pending', ?, 'team')
			`, taskID, entityID, domain.EventGitHubPRCICheckFailed, taskID, eventOf[entityID], runmode.LocalDefaultTeamID)
			return taskID
		},
		Run: func(t *testing.T, taskID, blueprintID, triggerType, status string, startedAt time.Time) string {
			t.Helper()
			var creator any
			if triggerType == "manual" {
				creator = runmode.LocalDefaultUserID
			}
			runID := uuid.New().String()
			exec(t, "seed blueprint run", `
				INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, creator_user_id,
				                            status, worktree_path, step_plan, started_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, '[]', ?)
			`, runID, blueprintID, taskID, triggerType, creator, status, "/tmp/wt-"+runID, startedAt)
			conversationID := uuid.New().String()
			step := 0
			insertConversationForTest(t, conn, domain.Conversation{
				ID: conversationID, TaskID: taskID, PromptID: seedSQLiteStepPrompt(t, conn),
				TriggerType: triggerType, BlueprintRunID: runID, BlueprintStepIndex: &step,
			})
			return conversationID
		},
		SetTrigger: func(t *testing.T, triggerID string, enabled bool, threshold int) {
			t.Helper()
			exec(t, "set trigger", `UPDATE event_handlers SET enabled = ?, breaker_threshold = ? WHERE id = ?`, enabled, threshold, triggerID)
		},
		DeleteTrigger: func(t *testing.T, triggerID string) {
			t.Helper()
			exec(t, "delete trigger", `UPDATE event_handlers SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`, triggerID)
		},
		CloseEntity: func(t *testing.T, entityID string) {
			t.Helper()
			exec(t, "close entity", `UPDATE entities SET state = 'closed' WHERE id = ?`, entityID)
		},
	}
}
