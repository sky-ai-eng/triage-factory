package postgres_test

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestConversationStore_NativeLoopReadsNeedNoRequestIdentity pins which pool
// the native loop's three transcript methods run on.
//
// The shared conformance suite deliberately wires both pools against AdminDB —
// it tests behavior, not auth — so it cannot see this. Nothing else could
// either: on the app pool these methods compile, pass every behavioral test,
// and then fail only in a real multi-mode deployment, because RLS evaluation
// calls current_user_id() and a JWT-less background job has no permission to
// execute it. The failure surfaces as a permission-denied error on the
// executor at the first assembly, with the run already claimed.
//
// So this test wires the pools the way production wires them (distinct admin
// and app connections) and drives the methods with NO user context, which is
// exactly the shape of an executor driving a claimed conversation. On the app
// pool every call here fails; on the admin pool every call succeeds.
func TestConversationStore_NativeLoopReadsNeedNoRequestIdentity(t *testing.T) {
	h := pgtest.Shared(t)
	// Production wiring, unlike the conformance suite's: admin for the system
	// methods, app for the RLS ones. Passing AdminDB twice is what hides the
	// bug this test exists to catch.
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	h.Reset(t)

	orgID, userID, agentID := seedPgConversationOrg(t, h)
	promptID := seedPgConversationPrompt(t, h, orgID, userID)
	seeder := newPgConversationSeeder(h.AdminDB, orgID, userID, agentID, promptID)

	entityID := seeder.Entity(t, "native-pool")
	eventID := seeder.Event(t, entityID, string(domain.EventGitHubPRCICheckFailed))
	taskID := seeder.Task(t, entityID, string(domain.EventGitHubPRCICheckFailed), eventID)
	blueprintRunID := seeder.BlueprintRun(t, taskID)
	stepIdx := 0
	conversationID := seeder.Conversation(t, domain.Conversation{
		TaskID:             taskID,
		PromptID:           promptID,
		Status:             "running",
		Model:              "claude-sonnet-5",
		BlueprintRunID:     blueprintRunID,
		BlueprintStepIndex: &stepIdx,
	})

	store := stores.Conversations
	// No pgtest.WithUser and no RLS claims: a claimed conversation is driven by
	// an executor that has no request identity to authenticate as.
	ctx := context.Background()

	// The fenced flush needs an engagement to fence against; SetExecutorSystem
	// mints the claim exactly as the dispatcher does.
	if err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-pool", 1); err != nil {
		t.Fatalf("SetExecutorSystem: %v", err)
	}
	var claimID string
	if err := h.AdminDB.QueryRow(
		`SELECT id::text FROM claims WHERE conversation_id = $1 AND released_at IS NULL`, conversationID,
	).Scan(&claimID); err != nil {
		t.Fatalf("read minted claim: %v", err)
	}

	id, err := store.InsertMessageSystem(ctx, orgID, &domain.Message{
		ConversationID: conversationID,
		Role:           "user",
		Content:        "begin",
	})
	if err != nil {
		t.Fatalf("InsertMessageSystem: %v", err)
	}

	rows, err := store.ListForAssemblySystem(ctx, orgID, conversationID)
	if err != nil {
		t.Fatalf("ListForAssemblySystem: %v — the native loop cannot assemble a window", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListForAssemblySystem returned %d rows, want 1", len(rows))
	}

	if err := store.MarkDeliveredForClaimSystem(ctx, orgID, conversationID, claimID, []int{int(id)}, "injection:steer"); err != nil {
		t.Fatalf("MarkDeliveredForClaimSystem: %v — the loop cannot retire a pending input", err)
	}

	if _, err := store.SetWindowStateSystem(ctx, orgID, conversationID, 1e9, domain.MessageWindowActive, domain.MessageWindowInactive); err != nil {
		t.Fatalf("SetWindowStateSystem: %v — the loop cannot compact its window", err)
	}
}
