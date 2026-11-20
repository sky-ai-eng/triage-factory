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
	seedTrigger := func(t *testing.T, blueprintID string, threshold int) string {
		t.Helper()
		triggerID := uuid.New().String()
		exec(t, "seed trigger", `
			INSERT INTO event_handlers (id, creator_user_id, team_id, mode, event_type, source, name,
			                            blueprint_id, breaker_threshold, min_autonomy_suitability)
			VALUES (?, ?, ?, 'automate', ?, 'user', 'Breaker', ?, ?, 0)
		`, triggerID, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID, domain.EventGitHubPRCICheckFailed, blueprintID, threshold)
		return triggerID
	}
	// seedRun stages a finished or running blueprint run with one step
	// conversation; triggerID and creator are each nil or a value, as the
	// run's trigger_type requires.
	seedRun := func(t *testing.T, taskID string, triggerID any, blueprintID, triggerType string, creator any, status string, startedAt time.Time) string {
		t.Helper()
		runID := uuid.New().String()
		exec(t, "seed blueprint run", `
			INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, trigger_id, creator_user_id,
			                            status, worktree_path, step_plan, started_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, '[]', ?)
		`, runID, blueprintID, taskID, triggerType, triggerID, creator, status, "/tmp/wt-"+runID, startedAt)
		conversationID := uuid.New().String()
		step := 0
		insertConversationForTest(t, conn, domain.Conversation{
			ID: conversationID, TaskID: taskID, PromptID: seedSQLiteStepPrompt(t, conn),
			TriggerType: triggerType, BlueprintRunID: runID, BlueprintStepIndex: &step,
		})
		return conversationID
	}
	return dbtest.BreakerSeeder{
		Trigger: func(t *testing.T, threshold int) (string, string) {
			t.Helper()
			promptID := "p_" + uuid.New().String()
			blueprintID := "bp_" + uuid.New().String()
			exec(t, "seed prompt", `INSERT INTO prompts (id, name, body, creator_user_id, team_id) VALUES (?, 'Breaker', 'x', ?, ?)`,
				promptID, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID)
			exec(t, "seed blueprint", `INSERT INTO blueprints (id, name, source, creator_user_id, team_id) VALUES (?, 'Breaker', 'user', ?, ?)`,
				blueprintID, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID)
			exec(t, "seed blueprint step", `INSERT INTO blueprint_steps (blueprint_id, step_index, step_prompt_id, team_id) VALUES (?, 0, ?, ?)`,
				blueprintID, promptID, runmode.LocalDefaultTeamID)
			return seedTrigger(t, blueprintID, threshold), blueprintID
		},
		TriggerOn: seedTrigger,
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
		EventRun: func(t *testing.T, taskID, triggerID, blueprintID, status string, startedAt time.Time) string {
			t.Helper()
			return seedRun(t, taskID, triggerID, blueprintID, "event", nil, status, startedAt)
		},
		ManualRun: func(t *testing.T, taskID, blueprintID, status string, startedAt time.Time) string {
			t.Helper()
			return seedRun(t, taskID, nil, blueprintID, "manual", runmode.LocalDefaultUserID, status, startedAt)
		},
		SetTrigger: func(t *testing.T, triggerID, mode string, threshold int) {
			t.Helper()
			exec(t, "set trigger", `UPDATE event_handlers SET mode = ?, breaker_threshold = ? WHERE id = ?`, mode, threshold, triggerID)
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
