package postgres_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestTaskStore_AttentionOrder_Postgres runs the shared attention-tier suite
// against the Postgres impl. Fixtures are seeded through the harness's admin
// connection (BYPASSRLS) and the store is wired on the same pool — the suite is
// about an ORDER BY, so it stays independent of the auth path. Skips cleanly
// when Docker isn't available.
func TestTaskStore_AttentionOrder_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	dbtest.RunTaskAttentionOrderConformance(t, func(t *testing.T) (db.TaskStore, string, dbtest.TaskAttentionOrderSeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID, _ := seedPgOrgUserAgent(t, h)
		conn := h.AdminDB

		seeder := dbtest.TaskAttentionOrderSeeder{
			Task: func(t *testing.T, f dbtest.TaskAttentionFixture) string {
				t.Helper()
				now := time.Now().UTC()
				entityID, eventID, taskID := uuid.New().String(), uuid.New().String(), uuid.New().String()
				sourceID := fmt.Sprintf("attn-%s-%d", f.Suffix, now.UnixNano())
				// In the seeded events_catalog, so the FK resolves without
				// re-seeding it inline.
				eventType := "github:pr:ci_check_failed"
				if _, err := conn.Exec(`
					INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
					VALUES ($1, $2, 'github', $3, 'pr', $4, $5, '{}'::jsonb, $6)
				`, entityID, orgID, sourceID, f.Title, "https://example/"+sourceID, now); err != nil {
					t.Fatalf("seed entity: %v", err)
				}
				if _, err := conn.Exec(`
					INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
					VALUES ($1, $2, $3, $4, '', '{}'::jsonb, $5)
				`, eventID, orgID, entityID, eventType, now); err != nil {
					t.Fatalf("seed event: %v", err)
				}
				var closedAt any
				if f.ClosedAt != nil {
					closedAt = *f.ClosedAt
				}
				if _, err := conn.Exec(`
					INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id,
					                   event_type, dedup_key, primary_event_id, status, scoring_status,
					                   priority_score, created_at, closed_at)
					VALUES ($1, $2, $3,
					        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
					        'team', $4, $5, '', $6, $7, 'pending', $8, $9, $10)
				`, taskID, orgID, userID, entityID, eventType, eventID, f.Status, f.Priority, now, closedAt); err != nil {
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
					INSERT INTO conversations (id, org_id, creator_user_id, team_id, visibility, task_id,
					                           trigger_type, origin, status)
					VALUES ($1, $2, $3,
					        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
					        'team', $4, 'manual', 'interactive', NULLIF($5, ''))
				`, convID, orgID, userID, taskID, storedStatus); err != nil {
					t.Fatalf("seed conversation: %v", err)
				}
				return convID
			},
			ActiveClaim: func(t *testing.T, conversationID string) string {
				t.Helper()
				var claimID string
				if err := conn.QueryRow(`
					INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch)
					VALUES ($1, $2, $3, 'exec-attn', 1) RETURNING id::text
				`, uuid.New().String(), orgID, conversationID).Scan(&claimID); err != nil {
					t.Fatalf("seed claim: %v", err)
				}
				return claimID
			},
			PendingPermission: func(t *testing.T, conversationID, claimID string) {
				t.Helper()
				if _, err := conn.Exec(`
					INSERT INTO conversation_permissions (id, org_id, conversation_id, claim_id,
					                                     tool_call_id, tool_name, state, requested_at)
					VALUES ($1, $2, $3, $4, $5, 'Bash', 'pending', $6)
				`, uuid.New().String(), orgID, conversationID, claimID,
					"toolu_"+uuid.New().String()[:8], time.Now().UTC()); err != nil {
					t.Fatalf("seed permission: %v", err)
				}
			},
			Artifact: func(t *testing.T, conversationID, kind, state, detailsJSON string) {
				t.Helper()
				id := uuid.New().String()
				if _, err := conn.Exec(`
					INSERT INTO artifacts (id, org_id, conversation_id, team_id, provider, kind, target,
					                       state, dedup_key, details_json)
					VALUES ($1, $2, $3,
					        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
					        'github', $4, 'o/r#7', $5, $6, NULLIF($7, ''))
				`, id, orgID, conversationID, kind, state, id, detailsJSON); err != nil {
					t.Fatalf("seed artifact: %v", err)
				}
			},
		}
		return stores.Tasks, orgID, seeder
	})
}
