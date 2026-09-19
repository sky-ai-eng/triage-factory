package dbtest

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ConversationQueueReturnedRowScaffold stages one claimable step and hands
// back its conversation id. RequeueConversation only acts on a mid-flight
// conversation with a live claim, so the suite needs a row
// ClaimNextConversation can actually find: a 'running' blueprint_run whose
// current_step_index names this step. Minted fresh per call so the requeue
// subtests never share a blueprint_run's single current step.
type ConversationQueueReturnedRowScaffold func(t *testing.T) (conversationID string)

// ConversationQueueReturnedRowFactory is what a per-backend test file hands to
// RunConversationQueueReturnedRowConformance: the ConversationQueueStore
// under test, its ConversationStore sibling (the point-read projection
// RequeueConversation shares — ConversationStore's Get/GetSystem), the org to
// pass, and the scaffold above.
type ConversationQueueReturnedRowFactory func(t *testing.T) (
	queue db.ConversationQueueStore,
	store db.ConversationStore,
	orgID string,
	scaffold ConversationQueueReturnedRowScaffold,
)

// RunConversationQueueReturnedRowConformance covers the returned-row standard
// for ConversationQueueStore's one conversations-row write:
// RequeueConversation returns the requeued row on a mid-flight conversation
// with a live claim, or nil — the guard declining — when there is nothing to
// hand back (the EntityStore.Close shape).
//
// There is no app-pool arm here. ConversationQueueStore is wired only
// against the admin pool in production — it is a system-service store, the
// dispatcher runs with no per-user identity — so the write has no RLS-gated
// door to lose visibility on. See ConversationAppPoolFactory's doc
// (conversation_returned_row_conformance.go) for the same reasoning applied
// to ConversationStore's claims-scoped writes.
func RunConversationQueueReturnedRowConformance(t *testing.T, mk ConversationQueueReturnedRowFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("RequeueConversation_returns_the_requeued_row_then_declines", func(t *testing.T) {
		queue, store, orgID, scaffold := mk(t)
		conversationID := scaffold(t)

		claimed, err := queue.ClaimNextConversation(ctx, "rr-executor", 1, db.ClaimPlacement{})
		if err != nil || claimed == nil || claimed.ID != conversationID {
			t.Fatalf("ClaimNextConversation = (%+v, %v), want conversation %s", claimed, err, conversationID)
		}

		conv, err := queue.RequeueConversation(ctx, orgID, conversationID, "transient rr failure")
		if err != nil {
			t.Fatalf("RequeueConversation: %v", err)
		}
		if conv == nil {
			t.Fatal("RequeueConversation returned nil for a mid-flight conversation with a live claim")
		}
		AssertWriteReturnedStoredRow(t, "RequeueConversation", *conv,
			func() (*domain.Conversation, error) { return store.GetSystem(ctx, orgID, conversationID) })

		// The guard-declined shape: the call above already released the only
		// claim, so there is nothing mid-flight-with-a-live-claim left to hand
		// back.
		declined, err := queue.RequeueConversation(ctx, orgID, conversationID, "duplicate")
		if err != nil {
			t.Fatalf("RequeueConversation (duplicate): %v", err)
		}
		if declined != nil {
			t.Errorf("RequeueConversation on an already-requeued conversation = %+v, want nil (the guard declining)", declined)
		}
	})

	t.Run("RequeueConversation_declines_on_a_missing_conversation", func(t *testing.T) {
		queue, _, orgID, _ := mk(t)
		missingID := uuid.New().String()
		conv, err := queue.RequeueConversation(ctx, orgID, missingID, "x")
		if err != nil || conv != nil {
			t.Errorf("RequeueConversation on a missing conversation id = (%+v, %v), want (nil, nil) — the guard declining", conv, err)
		}
	})
}
