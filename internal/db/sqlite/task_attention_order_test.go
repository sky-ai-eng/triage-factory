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

// TestTaskStore_AttentionOrder_SQLite runs the shared attention-tier suite
// against the SQLite impl. Each subtest gets a fresh in-memory DB, so a lane's
// exact order can't pick up a sibling's rows.
func TestTaskStore_AttentionOrder_SQLite(t *testing.T) {
	dbtest.RunTaskAttentionOrderConformance(t, func(t *testing.T) (db.TaskStore, string, dbtest.TaskAttentionOrderSeeder) {
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

		seeder := dbtest.TaskAttentionOrderSeeder{
			Task: func(t *testing.T, f dbtest.TaskAttentionFixture) string {
				t.Helper()
				now := time.Now().UTC()
				entityID, eventID, taskID := uuid.New().String(), uuid.New().String(), uuid.New().String()
				sourceID := fmt.Sprintf("attn-%s-%d", f.Suffix, now.UnixNano())
				eventType := domain.EventGitHubPRCICheckFailed
				if _, err := conn.Exec(`
					INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, created_at)
					VALUES (?, 'github', ?, 'pr', ?, ?, '{}', ?)
				`, entityID, sourceID, f.Title, "https://example/"+sourceID, now); err != nil {
					t.Fatalf("seed entity: %v", err)
				}
				if _, err := conn.Exec(`
					INSERT INTO events (id, entity_id, event_type, dedup_key, metadata_json, created_at)
					VALUES (?, ?, ?, '', '{}', ?)
				`, eventID, entityID, eventType, now); err != nil {
					t.Fatalf("seed event: %v", err)
				}
				var closedAt any
				if f.ClosedAt != nil {
					closedAt = *f.ClosedAt
				}
				if _, err := conn.Exec(`
					INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id,
					                   status, priority_score, scoring_status, created_at,
					                   team_id, visibility, closed_at)
					VALUES (?, ?, ?, '', ?, ?, ?, 'pending', ?, ?, 'team', ?)
				`, taskID, entityID, eventType, eventID, f.Status, f.Priority, now,
					runmode.LocalDefaultTeamID, closedAt); err != nil {
					t.Fatalf("seed task: %v", err)
				}
				return taskID
			},
			Conversation: func(t *testing.T, taskID, storedStatus string) string {
				t.Helper()
				convID := uuid.New().String()
				// origin='interactive': the origin CHECK demands the blueprint
				// parents only for 'blueprint', so this is how a conversation
				// with no sequence behind it is spelled.
				if _, err := conn.Exec(`
					INSERT INTO conversations (id, task_id, status, trigger_type, origin,
					                           team_id, visibility, creator_user_id)
					VALUES (?, ?, ?, 'manual', 'interactive', ?, 'team', ?)
				`, convID, taskID, nullIfEmptyString(storedStatus),
					runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID); err != nil {
					t.Fatalf("seed conversation: %v", err)
				}
				return convID
			},
			ActiveClaim: func(t *testing.T, conversationID string) string {
				t.Helper()
				return dbtest.SeedActiveClaim(t, conn, conversationID, "exec-attn", 1)
			},
			PendingPermission: func(t *testing.T, conversationID, claimID string) {
				t.Helper()
				if _, err := conn.Exec(`
					INSERT INTO conversation_permissions (id, conversation_id, claim_id, tool_call_id,
					                                     tool_name, state, requested_at)
					VALUES (?, ?, ?, ?, 'Bash', 'pending', ?)
				`, uuid.New().String(), conversationID, claimID,
					"toolu_"+uuid.New().String()[:8], time.Now().UTC()); err != nil {
					t.Fatalf("seed permission: %v", err)
				}
			},
			Artifact: func(t *testing.T, conversationID, kind, state, detailsJSON string) {
				t.Helper()
				id := uuid.New().String()
				if _, err := conn.Exec(`
					INSERT INTO artifacts (id, conversation_id, team_id, provider, kind, target,
					                       state, dedup_key, details_json)
					VALUES (?, ?, ?, 'github', ?, 'o/r#7', ?, ?, ?)
				`, id, conversationID, runmode.LocalDefaultTeamID, kind, state, id,
					nullIfEmptyString(detailsJSON)); err != nil {
					t.Fatalf("seed artifact: %v", err)
				}
			},
		}
		return sqlitestore.New(conn).Tasks, runmode.LocalDefaultOrgID, seeder
	})
}
