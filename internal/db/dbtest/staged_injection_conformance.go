package dbtest

import (
	"context"
	"errors"
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
			Subtype:        domain.MessageSubtypeInjectionSystemNote,
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
			Subtype:        domain.MessageSubtypeInjectionSystemNote,
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

	t.Run("Subtype_is_stored_and_any_other_value_refused", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID := seed.Conversation(t, "subtype")

		for _, subtype := range []string{domain.MessageSubtypeInjectionEvent, domain.MessageSubtypeInjectionSystemNote} {
			written, err := store.AppendSystem(ctx, orgID, domain.StagedInjection{
				ConversationID: conversationID, Subtype: subtype, Producer: "p", Body: subtype,
			})
			if err != nil {
				t.Fatalf("append %s: %v", subtype, err)
			}
			if written.Subtype != subtype {
				t.Errorf("returned subtype = %q, want %q", written.Subtype, subtype)
			}
		}
		for _, subtype := range []string{"", domain.MessageSubtypeInjectionSteer, "text"} {
			_, err := store.AppendSystem(ctx, orgID, domain.StagedInjection{
				ConversationID: conversationID, Subtype: subtype, Producer: "p", Body: "refused",
			})
			if !errors.Is(err, db.ErrStagedInjectionSubtype) {
				t.Errorf("append subtype %q: err = %v, want ErrStagedInjectionSubtype", subtype, err)
			}
		}

		got, err := store.FlushPendingSystem(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("flush: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("flush returned %d rows, want the two accepted appends: %+v", len(got), got)
		}
		for _, n := range got {
			if n.Subtype != n.Body {
				t.Errorf("flushed row stored under %q, appended as %q", n.Subtype, n.Body)
			}
		}
	})

	t.Run("Flush_and_withdraw_cover_both_subtypes", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID := seed.Conversation(t, "both")

		var ids []string
		for _, subtype := range []string{
			domain.MessageSubtypeInjectionEvent, domain.MessageSubtypeInjectionSystemNote,
			domain.MessageSubtypeInjectionEvent, domain.MessageSubtypeInjectionSystemNote,
		} {
			written, err := store.AppendSystem(ctx, orgID, domain.StagedInjection{
				ConversationID: conversationID, Subtype: subtype, Producer: "p", Body: subtype,
			})
			if err != nil {
				t.Fatalf("append %s: %v", subtype, err)
			}
			ids = append(ids, written.ID)
		}
		// Withdraw one row of each subtype; the other two flush, oldest first.
		for _, id := range ids[2:] {
			if consumed, err := store.DeleteSystem(ctx, orgID, id); err != nil || consumed {
				t.Fatalf("withdraw %s = (consumed %v, %v), want a withdrawal", id, consumed, err)
			}
		}

		got, err := store.FlushPendingSystem(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("flush: %v", err)
		}
		if len(got) != 2 || got[0].ID != ids[0] || got[1].ID != ids[1] {
			t.Fatalf("flush = %+v, want rows %s then %s", got, ids[0], ids[1])
		}
		if got[0].Subtype != domain.MessageSubtypeInjectionEvent || got[1].Subtype != domain.MessageSubtypeInjectionSystemNote {
			t.Errorf("flushed subtypes = %q, %q, want injection:event then injection:system-note", got[0].Subtype, got[1].Subtype)
		}
		if again, err := store.FlushPendingSystem(ctx, orgID, conversationID); err != nil || len(again) != 0 {
			t.Errorf("second flush = (%+v, %v), want nothing left", again, err)
		}
	})

	t.Run("Append_then_Flush_returns_notes_sorted_and_drains", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID := seed.Conversation(t, "drain")

		first, err := store.AppendSystem(ctx, orgID, domain.StagedInjection{ConversationID: conversationID, Subtype: domain.MessageSubtypeInjectionSystemNote, Producer: domain.StagedInjectionProducerPRNewCommits, Body: "first"})
		if err != nil {
			t.Fatalf("append first: %v", err)
		}
		if first.ID == "" {
			t.Error("AppendSystem must return a row with a minted id")
		}
		if _, err := store.AppendSystem(ctx, orgID, domain.StagedInjection{ConversationID: conversationID, Subtype: domain.MessageSubtypeInjectionSystemNote, Producer: domain.StagedInjectionProducerPRNewCommits, Body: "second"}); err != nil {
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
		if _, err := store.AppendSystem(ctx, orgID, domain.StagedInjection{ConversationID: convA, Subtype: domain.MessageSubtypeInjectionSystemNote, Producer: "p", Body: "for-A"}); err != nil {
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

		doomed, err := store.AppendSystem(ctx, orgID, domain.StagedInjection{ConversationID: conversationID, Subtype: domain.MessageSubtypeInjectionSystemNote, Producer: "p", Body: "withdrawn"})
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		if consumed, err := store.DeleteSystem(ctx, orgID, doomed.ID); err != nil || consumed {
			t.Fatalf("withdraw = (consumed %v, %v), want a withdrawal", consumed, err)
		}
		got, err := store.FlushPendingSystem(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("flush: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("flush returned %d injections after withdrawal, want 0: %+v", len(got), got)
		}

		// The withdrawal is per-row: a note staged afterwards flushes normally.
		if _, err := store.AppendSystem(ctx, orgID, domain.StagedInjection{ConversationID: conversationID, Subtype: domain.MessageSubtypeInjectionSystemNote, Producer: "p", Body: "survivor"}); err != nil {
			t.Fatalf("append survivor: %v", err)
		}
		got, err = store.FlushPendingSystem(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("flush survivor: %v", err)
		}
		if len(got) != 1 || got[0].Body != "survivor" {
			t.Errorf("post-withdrawal flush = %+v, want just the survivor", got)
		}

		// Withdrawing an already-flushed id is a no-op that reports the row
		// consumed, and never resurrects or hides its flush-once state.
		consumed, err := store.DeleteSystem(ctx, orgID, got[0].ID)
		if err != nil || !consumed {
			t.Fatalf("withdraw flushed id = (consumed %v, %v), want consumed", consumed, err)
		}
		if again, err := store.FlushPendingSystem(ctx, orgID, conversationID); err != nil || len(again) != 0 {
			t.Errorf("flush after withdrawing a flushed id = (%+v, %v), want nothing", again, err)
		}

		// An id that names no row is a no-op that reports nothing consumed.
		if consumed, err := store.DeleteSystem(ctx, orgID, "999999999"); err != nil || consumed {
			t.Errorf("withdraw unknown id = (consumed %v, %v), want (false, nil)", consumed, err)
		}
	})

	t.Run("Conversation_delete_cascades_pending_notes", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID := seed.Conversation(t, "cascade")
		if _, err := store.AppendSystem(ctx, orgID, domain.StagedInjection{ConversationID: conversationID, Subtype: domain.MessageSubtypeInjectionSystemNote, Producer: "p", Body: "doomed"}); err != nil {
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
			Subtype:        domain.MessageSubtypeInjectionSystemNote,
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
