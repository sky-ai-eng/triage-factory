package postgres_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestConversationStore_Postgres_LookupOrgForRunSystem_ReturnsRealOrgID
// pins the cold-start identity probe that cmd/exec runident depends
// on: a delegated agent subprocess only has TRIAGE_FACTORY_CONVERSATION_ID in
// its env, so the lookup has to discover the run's owning org by
// runID alone. Returns the real Postgres org UUID, NOT the local-mode
// sentinel — the exact regression this ticket exists to fix.
func TestConversationStore_Postgres_LookupOrgForRunSystem_ReturnsRealOrgID(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgID, userID, _ := seedPgConversationOrg(t, h)
	promptID := seedPgConversationPrompt(t, h, orgID, userID)

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	// Stage the entity + event + task + run chain so the runs row
	// exists with the real Postgres UUID in org_id.
	entityID := uuid.New().String()
	eventID := uuid.New().String()
	taskID := uuid.New().String()
	runID := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'github', $3, 'pr', 'Lookup probe', '', '{}'::jsonb, now())
	`, entityID, orgID, "lookup-"+orgID[:8]); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := h.AdminDB.Exec(`
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES ($1, $2, $3, 'github:pr:opened', '', '{}'::jsonb, now())
	`, eventID, orgID, entityID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := h.AdminDB.Exec(`
		INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key, primary_event_id, status, scoring_status, priority_score)
		VALUES ($1, $2, $3,
		        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
		        'team', $4, 'github:pr:opened', '', $5, 'queued', 'pending', 0.5)
	`, taskID, orgID, userID, entityID, eventID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	brID := seedPgBlueprintRun(t, h, orgID, userID, taskID)
	seedPgConversation(t, h.AdminDB, orgID, domain.Conversation{
		ID: runID, TaskID: taskID, PromptID: promptID, Status: "running", Model: "m",
		CreatorUserID: userID, BlueprintRunID: brID,
	})

	got, err := stores.Conversations.LookupOrgForRunSystem(ctx, runID)
	if err != nil {
		t.Fatalf("LookupOrgForRunSystem: %v", err)
	}
	if got != orgID {
		t.Errorf("LookupOrgForRunSystem = %q; want %q (the real Postgres org UUID, NOT the local sentinel)", got, orgID)
	}
}

// TestConversationStore_Postgres_LookupOrgForRunSystem_UnknownReturnsEmpty
// — an unknown runID returns ("", nil). The runident helper maps this
// to ErrRunIdentityNotFound so the agent subprocess surfaces a clear
// "stale env var / spawner bug" message in stderr rather than reading
// nil-dereference-style panics.
func TestConversationStore_Postgres_LookupOrgForRunSystem_UnknownReturnsEmpty(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	got, err := stores.Conversations.LookupOrgForRunSystem(context.Background(), uuid.New().String())
	if err != nil {
		t.Fatalf("LookupOrgForRunSystem on unknown run: %v", err)
	}
	if got != "" {
		t.Errorf("LookupOrgForRunSystem on unknown run = %q; want empty string", got)
	}
}
