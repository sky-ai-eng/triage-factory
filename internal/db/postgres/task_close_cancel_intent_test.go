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

// TestTaskStore_CloseWithConversationCancelIntent_Postgres runs the shared close-
// carries-stop-intent suite against the Postgres impl. Fixtures are seeded
// through the harness's admin connection (BYPASSRLS) and the store is wired
// on the same pool — the method is admin-pool only, so that is the pool it
// runs on in production too. Skips cleanly when Docker isn't available.
func TestTaskStore_CloseWithConversationCancelIntent_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	dbtest.RunTaskCloseCancelIntentConformance(t, func(t *testing.T) (db.TaskStore, string, dbtest.TaskCloseCancelIntentSeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID, _ := seedPgOrgUserAgent(t, h)
		conn := h.AdminDB

		// entityForTask lets Event() find the entity a seeded task hangs off,
		// so the closing event lands on the same entity production would put
		// it on. Keyed per factory call, which is per subtest.
		entityForTask := map[string]string{}

		seeder := dbtest.TaskCloseCancelIntentSeeder{
			Task: func(t *testing.T) string {
				t.Helper()
				entityID, _, taskID := seedPgTaskChain(t, conn, orgID, userID, "close-intent-"+uuid.New().String()[:8])
				entityForTask[taskID] = entityID
				return taskID
			},
			Event: func(t *testing.T, taskID string) string {
				t.Helper()
				eventID := uuid.New().String()
				if _, err := conn.Exec(`
					INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
					VALUES ($1, $2, $3, $4, '', '{}'::jsonb, $5)
				`, eventID, orgID, entityForTask[taskID], domain.EventGitHubPRMerged, time.Now().UTC()); err != nil {
					t.Fatalf("seed closing event: %v", err)
				}
				return eventID
			},
			BlueprintAndConversation: func(t *testing.T, taskID, blueprintStatus, convStatus string) (string, string) {
				t.Helper()
				brID := seedPgBlueprintRunForClose(t, conn, orgID, userID, taskID, blueprintStatus)
				promptID := seedPgStepPromptForClose(t, conn, orgID, userID)
				convID := uuid.New().String()
				if _, err := conn.Exec(`
					INSERT INTO conversations (id, org_id, creator_user_id, team_id, visibility, task_id,
					                           prompt_id, trigger_type, origin, status,
					                           blueprint_run_id, blueprint_step_index)
					VALUES ($1, $2, NULL,
					        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
					        'team', $3, $4, 'event', 'blueprint', NULLIF($5, ''), $6, 0)
				`, convID, orgID, taskID, promptID, convStatus, brID); err != nil {
					t.Fatalf("seed conversation: %v", err)
				}
				return brID, convID
			},
			BareRun: func(t *testing.T, taskID, convStatus string) string {
				t.Helper()
				convID := uuid.New().String()
				// origin='interactive': the origin CHECK demands the blueprint
				// parents only for 'blueprint', so this is how a conversation
				// with no sequence behind it is spelled.
				if _, err := conn.Exec(`
					INSERT INTO conversations (id, org_id, creator_user_id, team_id, visibility, task_id,
					                           trigger_type, origin, status)
					VALUES ($1, $2, $3,
					        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
					        'team', $4, 'manual', 'interactive', NULLIF($5, ''))
				`, convID, orgID, userID, taskID, convStatus); err != nil {
					t.Fatalf("seed blueprintless conversation: %v", err)
				}
				return convID
			},
			CancelRequested: func(t *testing.T, blueprintRunID string) bool {
				t.Helper()
				var flag bool
				if err := conn.QueryRow(`SELECT cancel_requested FROM blueprint_runs WHERE id = $1`, blueprintRunID).Scan(&flag); err != nil {
					t.Fatalf("read cancel_requested: %v", err)
				}
				return flag
			},
			TaskStatus: func(t *testing.T, taskID string) string {
				t.Helper()
				var status string
				if err := conn.QueryRow(`SELECT status FROM tasks WHERE id = $1`, taskID).Scan(&status); err != nil {
					t.Fatalf("read task status: %v", err)
				}
				return status
			},
			CloseAuditCount: func(t *testing.T, taskID string) int {
				t.Helper()
				var n int
				if err := conn.QueryRow(`SELECT COUNT(*) FROM task_events WHERE task_id = $1 AND kind = 'closed'`, taskID).Scan(&n); err != nil {
					t.Fatalf("count close audit rows: %v", err)
				}
				return n
			},
		}
		return stores.Tasks, orgID, seeder
	})
}

// seedPgBlueprintRunForClose mints a blueprint + blueprint_run on the task in
// the named status. The suite needs a finished one as well as a running one,
// since "a blueprint that already ended is never stamped" is half of what it
// asserts.
func seedPgBlueprintRunForClose(t *testing.T, conn *sql.DB, orgID, userID, taskID, status string) string {
	t.Helper()
	bpID := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO blueprints (id, org_id, creator_user_id, team_id, name, source)
		VALUES ($1, $2, $3,
		        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
		        'Close Intent BP', 'user')
	`, bpID, orgID, userID); err != nil {
		t.Fatalf("seed blueprint: %v", err)
	}
	brID := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO blueprint_runs (id, org_id, creator_user_id, blueprint_id, task_id,
		                            trigger_type, status, worktree_path, step_plan)
		VALUES ($1, $2, $3, $4, $5, 'manual', $6, '/tmp/wt', '[]')
	`, brID, orgID, userID, bpID, taskID, status); err != nil {
		t.Fatalf("seed blueprint_run: %v", err)
	}
	return brID
}

// seedPgStepPromptForClose mints one prompt so a blueprint-origin conversation
// can satisfy the origin CHECK's prompt_id requirement.
func seedPgStepPromptForClose(t *testing.T, conn *sql.DB, orgID, userID string) string {
	t.Helper()
	promptID := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO prompts (id, org_id, creator_user_id, team_id, name, body, source, allowed_tools, created_at, updated_at)
		VALUES ($1, $2, $3,
		        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
		        'Close Intent', 'body', 'user', '', now(), now())
	`, promptID, orgID, userID); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	return promptID
}
