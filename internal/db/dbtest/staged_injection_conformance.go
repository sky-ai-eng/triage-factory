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

	t.Run("Provenance_round_trips_through_the_metadata_column", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID := seed.Conversation(t, "provenance")

		// The write returns the row it persisted, so the returned copy and a
		// later flush of the same row must agree — both map through the
		// shared mapper, and this is what pins the metadata keys one writes
		// to the keys the other reads.
		want := domain.NoteProvenance{EventID: "evt-provenance-1", EventType: domain.EventGitHubPRCICheckFailed}
		written, err := store.AppendSystem(ctx, orgID, domain.StagedInjection{
			ConversationID: conversationID,
			Producer:       domain.StagedInjectionProducerPRCoherence,
			Body:           "CI went red",
			Provenance:     want,
		})
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		if written.Provenance != want {
			t.Errorf("returned row provenance = %+v, want %+v", written.Provenance, want)
		}

		got, err := store.FlushPendingSystem(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("flush: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("flush returned %d rows, want 1", len(got))
		}
		if got[0].Provenance != want {
			t.Errorf("flushed provenance = %+v, want %+v", got[0].Provenance, want)
		}
		if got[0].Producer != domain.StagedInjectionProducerPRCoherence {
			t.Errorf("flushed producer = %q, want %q", got[0].Producer, domain.StagedInjectionProducerPRCoherence)
		}
	})

	t.Run("A_note_with_no_originating_event_stays_valid", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID := seed.Conversation(t, "no-provenance")

		written, err := store.AppendSystem(ctx, orgID, domain.StagedInjection{
			ConversationID: conversationID,
			Producer:       domain.StagedInjectionProducerPRCoherence,
			Body:           "no event caused this",
		})
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		if !written.Provenance.Empty() {
			t.Errorf("returned provenance = %+v, want empty", written.Provenance)
		}
		got, err := store.FlushPendingSystem(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("flush: %v", err)
		}
		if len(got) != 1 || !got[0].Provenance.Empty() {
			t.Errorf("flushed rows = %+v, want one with empty provenance", got)
		}
	})

	t.Run("Append_then_Flush_returns_notes_sorted_and_drains", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID := seed.Conversation(t, "drain")

		first, err := store.AppendSystem(ctx, orgID, domain.StagedInjection{ConversationID: conversationID, Producer: domain.StagedInjectionProducerPRNewCommits, Body: "first"})
		if err != nil {
			t.Fatalf("append first: %v", err)
		}
		if first.ID == "" {
			t.Error("AppendSystem must return a row with a minted id")
		}
		if _, err := store.AppendSystem(ctx, orgID, domain.StagedInjection{ConversationID: conversationID, Producer: domain.StagedInjectionProducerPRNewCommits, Body: "second"}); err != nil {
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
		if _, err := store.AppendSystem(ctx, orgID, domain.StagedInjection{ConversationID: convA, Producer: "p", Body: "for-A"}); err != nil {
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

		doomed, err := store.AppendSystem(ctx, orgID, domain.StagedInjection{ConversationID: conversationID, Producer: "p", Body: "withdrawn"})
		if err != nil {
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
		if _, err := store.AppendSystem(ctx, orgID, domain.StagedInjection{ConversationID: conversationID, Producer: "p", Body: "survivor"}); err != nil {
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
		if _, err := store.AppendSystem(ctx, orgID, domain.StagedInjection{ConversationID: conversationID, Producer: "p", Body: "doomed"}); err != nil {
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

// StagedInjectionReturnedRowFactory hands the returned-row-only conformance
// arm a wired store, the orgID to pass, and a conversation id the FK can
// point at — already seeded by the caller, since this arm doesn't own the
// seed/delete lifecycle RunStagedInjectionStoreConformance does.
type StagedInjectionReturnedRowFactory func(t *testing.T) (store db.StagedInjectionStore, orgID, conversationID string)

// RunStagedInjectionReturnedRowConformance covers the returned-row standard
// on AppendSystem. StagedInjectionStore has no plain point read —
// FlushPendingSystem, its only other read, is destructive — so the "point
// read" this arm compares against is a flush called immediately after one
// Append on a conversation with nothing else staged: one row went in, one row
// must come out, and it must be the row Append reported back.
func RunStagedInjectionReturnedRowConformance(t *testing.T, mk StagedInjectionReturnedRowFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("AppendSystem_returns_the_stored_row", func(t *testing.T) {
		store, orgID, conversationID := mk(t)

		appended, err := store.AppendSystem(ctx, orgID, domain.StagedInjection{
			ConversationID: conversationID,
			Producer:       domain.StagedInjectionProducerPRNewCommits,
			Body:           "returned-row check",
		})
		if err != nil {
			t.Fatalf("AppendSystem: %v", err)
		}
		if appended.ID == "" {
			t.Error("AppendSystem must return a row with a minted id")
		}
		if appended.CreatedAt.IsZero() {
			t.Error("AppendSystem must return a row with created_at stamped")
		}

		read := func() (*domain.StagedInjection, error) {
			rows, err := store.FlushPendingSystem(ctx, orgID, conversationID)
			if err != nil {
				return nil, err
			}
			if len(rows) == 0 {
				return nil, nil
			}
			return &rows[0], nil
		}
		AssertWriteReturnedStoredRow(t, "AppendSystem", appended, read)
	})
}
