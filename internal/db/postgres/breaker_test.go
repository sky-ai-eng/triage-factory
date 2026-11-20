package postgres_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestBreaker_Postgres runs the shared breaker suite against the Postgres
// impl. Both reads are admin-pool only, so the store is wired on the
// harness's admin connection, where production runs them. Skips cleanly when
// Docker isn't available.
func TestBreaker_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	dbtest.RunBreakerConformance(t, func(t *testing.T) (db.TaskStore, string, dbtest.BreakerSeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID, _ := seedPgOrgUserAgent(t, h)
		return stores.Tasks, orgID, newPgBreakerSeeder(h.AdminDB, orgID, userID)
	})
}

func newPgBreakerSeeder(conn *sql.DB, orgID, userID string) dbtest.BreakerSeeder {
	exec := func(t *testing.T, what, query string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(query, args...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	const team = `(SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1)`
	eventOf := map[string]string{}
	seedTrigger := func(t *testing.T, blueprintID string, threshold int) string {
		t.Helper()
		triggerID := uuid.New().String()
		exec(t, "seed trigger", `
			INSERT INTO event_handlers (id, org_id, creator_user_id, team_id, mode, event_type, source, name,
			                            blueprint_id, breaker_threshold, min_autonomy_suitability)
			VALUES ($1, $2, $3, `+team+`, 'automate', $4, 'user', 'Breaker', $5, $6, 0)
		`, triggerID, orgID, userID, domain.EventGitHubPRCICheckFailed, blueprintID, threshold)
		return triggerID
	}
	// seedRun stages a finished or running blueprint run with one step
	// conversation; triggerID and creator are each nil or a value, as the
	// run's trigger_type requires.
	seedRun := func(t *testing.T, taskID string, triggerID any, blueprintID, triggerType string, creator any, status string, startedAt time.Time) string {
		t.Helper()
		runID := uuid.New().String()
		exec(t, "seed blueprint run", `
			INSERT INTO blueprint_runs (id, org_id, team_id, creator_user_id, blueprint_id, task_id,
			                            trigger_type, trigger_id, status, worktree_path, step_plan, started_at)
			VALUES ($1, $2, (SELECT team_id FROM blueprints WHERE id = $4 AND org_id = $2), $3, $4, $5, $6, $7, $8, '/tmp/wt', '[]', $9)
		`, runID, orgID, creator, blueprintID, taskID, triggerType, triggerID, status, startedAt)
		promptID := seedPgStepPromptForClose(t, conn, orgID, userID)
		conversationID := uuid.New().String()
		exec(t, "seed step conversation", `
			INSERT INTO conversations (id, org_id, creator_user_id, team_id, visibility, task_id,
			                           prompt_id, trigger_type, origin, blueprint_run_id, blueprint_step_index, started_at)
			VALUES ($1, $2, $3, `+team+`, 'team', $4, $5, $6, 'blueprint', $7, 0, $8)
		`, conversationID, orgID, creator, taskID, promptID, triggerType, runID, startedAt)
		return conversationID
	}
	return dbtest.BreakerSeeder{
		Trigger: func(t *testing.T, threshold int) (string, string) {
			t.Helper()
			promptID := "p_" + uuid.New().String()
			blueprintID := "bp_" + uuid.New().String()
			exec(t, "seed prompt", `
				INSERT INTO prompts (id, org_id, creator_user_id, team_id, name, body, source)
				VALUES ($1, $2, $3, `+team+`, 'Breaker', 'x', 'user')
			`, promptID, orgID, userID)
			exec(t, "seed blueprint", `
				INSERT INTO blueprints (id, org_id, creator_user_id, team_id, name, source)
				VALUES ($1, $2, $3, `+team+`, 'Breaker', 'user')
			`, blueprintID, orgID, userID)
			exec(t, "seed blueprint step", `
				INSERT INTO blueprint_steps (org_id, team_id, blueprint_id, step_index, step_prompt_id)
				VALUES ($2, `+team+`, $1, 0, $3)
			`, blueprintID, orgID, promptID)
			return seedTrigger(t, blueprintID, threshold), blueprintID
		},
		TriggerOn: seedTrigger,
		Task: func(t *testing.T) (string, string) {
			t.Helper()
			entityID, eventID, taskID := seedPgTaskChain(t, conn, orgID, userID, "breaker-"+uuid.New().String()[:8])
			eventOf[entityID] = eventID
			return entityID, taskID
		},
		TaskOnEntity: func(t *testing.T, entityID string) string {
			t.Helper()
			taskID := uuid.New().String()
			exec(t, "seed task on entity", `
				INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key,
				                   primary_event_id, status, scoring_status, priority_score)
				VALUES ($1, $2, $3, `+team+`, 'team', $4, $5, $6, $7, 'queued', 'pending', 0.5)
			`, taskID, orgID, userID, entityID, domain.EventGitHubPRCICheckFailed, "breaker-"+taskID, eventOf[entityID])
			return taskID
		},
		EventRun: func(t *testing.T, taskID, triggerID, blueprintID, status string, startedAt time.Time) string {
			t.Helper()
			return seedRun(t, taskID, triggerID, blueprintID, "event", nil, status, startedAt)
		},
		ManualRun: func(t *testing.T, taskID, blueprintID, status string, startedAt time.Time) string {
			t.Helper()
			return seedRun(t, taskID, nil, blueprintID, "manual", userID, status, startedAt)
		},
		SetTrigger: func(t *testing.T, triggerID, mode string, threshold int) {
			t.Helper()
			exec(t, "set trigger", `UPDATE event_handlers SET mode = $1, breaker_threshold = $2 WHERE id = $3`, mode, threshold, triggerID)
		},
		DeleteTrigger: func(t *testing.T, triggerID string) {
			t.Helper()
			exec(t, "delete trigger", `UPDATE event_handlers SET deleted_at = now() WHERE id = $1`, triggerID)
		},
		CloseEntity: func(t *testing.T, entityID string) {
			t.Helper()
			exec(t, "close entity", `UPDATE entities SET state = 'closed' WHERE id = $1`, entityID)
		},
	}
}
