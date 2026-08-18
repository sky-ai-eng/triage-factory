package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// StagedInjectionStoreFactory is what a per-backend test file hands to
// RunStagedInjectionStoreConformance. Returns:
//   - the wired StagedInjectionStore impl,
//   - the orgID to pass to every call,
//   - a StagedInjectionSeeder the harness uses to stage the conversation FK
//     chain (a staged injection is a messages row, whose conversation_id FKs
//     conversations; backends seed conversations differently).
type StagedInjectionStoreFactory func(t *testing.T) (store db.StagedInjectionStore, orgID string, seed StagedInjectionSeeder)

// StagedInjectionSeeder is a bag of callbacks the conformance suite uses to stage
// fixture rows the StagedInjectionStore doesn't own.
type StagedInjectionSeeder struct {
	// Conversation inserts a conversations row and returns its id. suffix discriminates per-subtest
	// seeds so unique indexes don't collide.
	Conversation func(t *testing.T, suffix string) (conversationID string)

	// DeleteConversation removes the conversation row so the FK ON DELETE CASCADE
	// subtest can verify a purged conversation takes its undelivered injections
	// with it.
	DeleteConversation func(t *testing.T, conversationID string)
}

// RunStagedInjectionStoreConformance covers the StagedInjectionStore contract every
// backend impl must hold (TFAC-501): append, flush-once (destructive),
// per-conversation isolation, and FK cascade.
func RunStagedInjectionStoreConformance(t *testing.T, mk StagedInjectionStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("Append_then_Flush_returns_notes_sorted_and_drains", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID := seed.Conversation(t, "drain")

		first := &domain.StagedInjection{ConversationID: conversationID, Producer: domain.StagedInjectionProducerPRNewCommits, Body: "first"}
		if err := store.AppendSystem(ctx, orgID, first); err != nil {
			t.Fatalf("append first: %v", err)
		}
		if first.ID == "" {
			t.Error("AppendSystem must mint and write back an id")
		}
		if err := store.AppendSystem(ctx, orgID, &domain.StagedInjection{ConversationID: conversationID, Producer: domain.StagedInjectionProducerPRNewCommits, Body: "second"}); err != nil {
			t.Fatalf("append second: %v", err)
		}

		got, err := store.FlushPendingSystem(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("flush: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("flush returned %d injections, want 2", len(got))
		}
		// Returned in non-decreasing created_at order (the sort the store applies).
		if got[0].CreatedAt.After(got[1].CreatedAt) {
			t.Errorf("flush not sorted oldest-first: %v then %v", got[0].CreatedAt, got[1].CreatedAt)
		}
		bodies := map[string]bool{got[0].Body: true, got[1].Body: true}
		if !bodies["first"] || !bodies["second"] {
			t.Errorf("flush missing a body: got %q and %q", got[0].Body, got[1].Body)
		}
		// Every row carries its conversation/org/producer back.
		for _, n := range got {
			if n.ConversationID != conversationID || n.OrgID != orgID || n.Producer != domain.StagedInjectionProducerPRNewCommits {
				t.Errorf("row fields not preserved: %+v", n)
			}
		}

		// Destructive: a second flush drains nothing.
		again, err := store.FlushPendingSystem(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("second flush: %v", err)
		}
		if len(again) != 0 {
			t.Errorf("second flush returned %d injections, want 0 (flush must drain)", len(again))
		}
	})

	t.Run("Flush_empty_when_nothing_staged", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID := seed.Conversation(t, "empty")
		got, err := store.FlushPendingSystem(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("flush: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("flush of an unstaged conversation returned %d injections, want 0", len(got))
		}
	})

	t.Run("Flush_is_per_conversation_isolated", func(t *testing.T) {
		store, orgID, seed := mk(t)
		convA := seed.Conversation(t, "iso-a")
		convB := seed.Conversation(t, "iso-b")
		if err := store.AppendSystem(ctx, orgID, &domain.StagedInjection{ConversationID: convA, Producer: "p", Body: "for-A"}); err != nil {
			t.Fatalf("append A: %v", err)
		}
		got, err := store.FlushPendingSystem(ctx, orgID, convB)
		if err != nil {
			t.Fatalf("flush B: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("conversation B flush leaked conversation A's injection: %+v", got)
		}
		// Conversation A's injection is still there (B's flush must not have
		// drained it).
		gotA, err := store.FlushPendingSystem(ctx, orgID, convA)
		if err != nil {
			t.Fatalf("flush A: %v", err)
		}
		if len(gotA) != 1 || gotA[0].Body != "for-A" {
			t.Errorf("conversation A injection missing after B flush: %+v", gotA)
		}
	})

	t.Run("Withdrawn_note_is_never_flushed", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID := seed.Conversation(t, "withdraw")

		doomed := &domain.StagedInjection{ConversationID: conversationID, Producer: "p", Body: "withdrawn"}
		if err := store.AppendSystem(ctx, orgID, doomed); err != nil {
			t.Fatalf("append: %v", err)
		}
		if err := store.DeleteSystem(ctx, orgID, doomed.ID); err != nil {
			t.Fatalf("withdraw: %v", err)
		}
		got, err := store.FlushPendingSystem(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("flush: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("flush returned %d injections after withdrawal, want 0: %+v", len(got), got)
		}

		// The withdrawal is per-row: a note staged afterwards flushes normally.
		if err := store.AppendSystem(ctx, orgID, &domain.StagedInjection{ConversationID: conversationID, Producer: "p", Body: "survivor"}); err != nil {
			t.Fatalf("append survivor: %v", err)
		}
		got, err = store.FlushPendingSystem(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("flush survivor: %v", err)
		}
		if len(got) != 1 || got[0].Body != "survivor" {
			t.Errorf("post-withdrawal flush = %+v, want just the survivor", got)
		}

		// Withdrawing an already-flushed id is a tolerated no-op, and never
		// resurrects or hides the delivered row's flush-once state.
		if err := store.DeleteSystem(ctx, orgID, got[0].ID); err != nil {
			t.Fatalf("withdraw flushed id: %v", err)
		}
	})

	t.Run("Conversation_delete_cascades_pending_notes", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID := seed.Conversation(t, "cascade")
		if err := store.AppendSystem(ctx, orgID, &domain.StagedInjection{ConversationID: conversationID, Producer: "p", Body: "doomed"}); err != nil {
			t.Fatalf("append: %v", err)
		}
		seed.DeleteConversation(t, conversationID)
		// FlushPending filters only by (org_id, conversation_id), so a non-empty
		// result here would mean the rows survived the conversation deletion —
		// i.e. the cascade didn't fire. Empty proves the FK ON DELETE CASCADE
		// took them.
		got, err := store.FlushPendingSystem(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("flush after delete: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("staged injections survived conversation deletion (cascade did not fire): %+v", got)
		}
	})
}
