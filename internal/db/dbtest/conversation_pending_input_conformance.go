package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// ConversationPendingInputStoreFactory is what a per-backend test file hands to
// RunConversationPendingInputStoreConformance. Returns:
//   - the wired ConversationPendingInputStore impl,
//   - the orgID and a userID to pass to every call,
//   - a ConversationPendingInputSeeder the harness uses to stage the run FK chain.
type ConversationPendingInputStoreFactory func(t *testing.T) (store db.ConversationPendingInputStore, orgID, userID string, seed ConversationPendingInputSeeder)

// ConversationPendingInputSeeder is a bag of callbacks the conformance suite uses to
// stage fixture rows ConversationPendingInputStore doesn't own — which now includes the
// queued rows themselves, since the store reads a queue it does not write.
type ConversationPendingInputSeeder struct {
	// Conversation inserts a conversations row and returns its id. suffix discriminates
	// per-subtest seeds so unique indexes don't collide.
	Conversation func(t *testing.T, suffix string) (conversationID string)

	// StagePending queues one message the way production does — the ordinary
	// transcript insert, undelivered. Backends wire it to their own
	// ConversationStore.InsertMessage rather than a hand-rolled INSERT, so
	// what this suite reads is exactly what the follow-up path writes; a
	// column the writer stops setting fails here rather than in production.
	StagePending func(t *testing.T, conversationID, userID, message string)

	// DeleteConversation removes the run row so the FK ON DELETE CASCADE subtest can
	// verify a purged run takes its pending input with it.
	DeleteConversation func(t *testing.T, conversationID string)

	// SecondUser returns a second user id valid for the same org, so the
	// multi-author subtests can queue rows from two people. The store's
	// attribution rule (newest row wins) is unobservable with one.
	SecondUser func(t *testing.T) string
}

// RunConversationPendingInputStoreConformance covers the ConversationPendingInputStore
// contract every backend impl must hold (TFAC-585): consume is destructive and
// exactly once, the append-and-join queue contract, Peek and Consume agreeing
// on what is pending, per-run isolation, absence reads as ok=false (never an
// error), and FK cascade. Rows are staged through the production writer (see
// ConversationPendingInputSeeder.StagePending), because a reader that only agrees with
// its own test-local INSERT proves nothing.
func RunConversationPendingInputStoreConformance(t *testing.T, mk ConversationPendingInputStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("Store_then_Consume_returns_and_drains", func(t *testing.T) {
		store, orgID, userID, seed := mk(t)
		conversationID := seed.Conversation(t, "drain")

		seed.StagePending(t, conversationID, userID, "pick this back up")
		msg, gotUser, ok, err := store.Consume(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("consume: %v", err)
		}
		if !ok {
			t.Fatal("consume ok=false, want true")
		}
		if msg != "pick this back up" {
			t.Errorf("message = %q, want %q", msg, "pick this back up")
		}
		if gotUser != userID {
			t.Errorf("userID = %q, want %q", gotUser, userID)
		}

		// Destructive: a second consume finds nothing.
		_, _, ok, err = store.Consume(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("second consume: %v", err)
		}
		if ok {
			t.Error("second consume ok=true, want false (consume must drain)")
		}
	})

	t.Run("Peek_returns_without_draining_then_Consume_deletes", func(t *testing.T) {
		store, orgID, userID, seed := mk(t)
		conversationID := seed.Conversation(t, "peek")

		seed.StagePending(t, conversationID, userID, "still here")
		// Peek twice: both return the row (non-destructive).
		for i := 0; i < 2; i++ {
			msg, gotUser, ok, err := store.Peek(ctx, orgID, conversationID)
			if err != nil {
				t.Fatalf("peek %d: %v", i, err)
			}
			if !ok || msg != "still here" || gotUser != userID {
				t.Fatalf("peek %d = (%q, %q, %v); want (%q, %q, true) — peek must not drain", i, msg, gotUser, ok, "still here", userID)
			}
		}
		// Consume then drains it, and a following Peek finds nothing.
		if _, _, ok, err := store.Consume(ctx, orgID, conversationID); err != nil || !ok {
			t.Fatalf("consume after peek = (ok=%v, err=%v); want ok=true", ok, err)
		}
		if _, _, ok, err := store.Peek(ctx, orgID, conversationID); err != nil || ok {
			t.Fatalf("peek after consume = (ok=%v, err=%v); want ok=false", ok, err)
		}
	})

	t.Run("Consume_absent_is_ok_false_not_an_error", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		conversationID := seed.Conversation(t, "empty")
		_, _, ok, err := store.Consume(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("consume: %v", err)
		}
		if ok {
			t.Error("consume of an unstaged run reported ok=true")
		}
	})

	// The heart of the append contract: two messages sent to a parked
	// conversation before it wakes are both delivered. The store this replaced
	// deleted the first one — no error, no log, nothing in the transcript.
	t.Run("Store_appends_and_Consume_joins_in_order", func(t *testing.T) {
		store, orgID, userID, seed := mk(t)
		conversationID := seed.Conversation(t, "append")

		seed.StagePending(t, conversationID, userID, "fix the null check")
		seed.StagePending(t, conversationID, userID, "actually no, fix the test")
		msg, _, ok, err := store.Consume(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("consume: %v", err)
		}
		if !ok {
			t.Fatal("consume ok=false, want true")
		}
		want := "fix the null check" + db.PendingInputSeparator + "actually no, fix the test"
		if msg != want {
			t.Errorf("message = %q, want %q (both messages, oldest first)", msg, want)
		}
		// One flush drains everything it joined — no row is left behind to be
		// re-delivered on a later claim.
		if _, _, ok, err := store.Consume(ctx, orgID, conversationID); err != nil || ok {
			t.Errorf("second consume = (ok=%v, err=%v); want ok=false — the flush must drain every joined row", ok, err)
		}
	})

	// Peek routes the claim to the resume path and Consume delivers; they read
	// the same rows a rehydrate apart, so they must read them the same way.
	t.Run("Peek_and_Consume_agree_across_several_rows", func(t *testing.T) {
		store, orgID, userID, seed := mk(t)
		conversationID := seed.Conversation(t, "agree")

		for _, m := range []string{"one", "two", "three"} {
			seed.StagePending(t, conversationID, userID, m)
		}
		peeked, peekedUser, ok, err := store.Peek(ctx, orgID, conversationID)
		if err != nil || !ok {
			t.Fatalf("peek = (ok=%v, err=%v); want ok=true", ok, err)
		}
		consumed, consumedUser, ok, err := store.Consume(ctx, orgID, conversationID)
		if err != nil || !ok {
			t.Fatalf("consume = (ok=%v, err=%v); want ok=true", ok, err)
		}
		if peeked != consumed {
			t.Errorf("peek = %q but consume = %q; the routing decision and the delivery must agree", peeked, consumed)
		}
		if peekedUser != consumedUser {
			t.Errorf("peek user = %q but consume user = %q; attribution must agree too", peekedUser, consumedUser)
		}
	})

	// Attribution with several authors: one id rides the delivering claim's
	// writes, and it is the newest row's — the most recent human intent.
	t.Run("Attribution_is_the_newest_rows_user", func(t *testing.T) {
		store, orgID, userID, seed := mk(t)
		if seed.SecondUser == nil {
			t.Skip("backend seeder supplies no second user")
		}
		conversationID := seed.Conversation(t, "authors")
		teammate := seed.SecondUser(t)

		seed.StagePending(t, conversationID, userID, "my follow-up")
		seed.StagePending(t, conversationID, teammate, "and while you're in there")
		msg, gotUser, ok, err := store.Consume(ctx, orgID, conversationID)
		if err != nil || !ok {
			t.Fatalf("consume = (ok=%v, err=%v); want ok=true", ok, err)
		}
		if gotUser != teammate {
			t.Errorf("userID = %q, want the newest row's author %q", gotUser, teammate)
		}
		want := "my follow-up" + db.PendingInputSeparator + "and while you're in there"
		if msg != want {
			t.Errorf("message = %q, want %q — a second author appends, it does not replace", msg, want)
		}
	})

	t.Run("Consume_is_per_conversation_isolated", func(t *testing.T) {
		store, orgID, userID, seed := mk(t)
		convA := seed.Conversation(t, "iso-a")
		convB := seed.Conversation(t, "iso-b")
		seed.StagePending(t, convA, userID, "for-A")
		_, _, ok, err := store.Consume(ctx, orgID, convB)
		if err != nil {
			t.Fatalf("consume B: %v", err)
		}
		if ok {
			t.Error("conversation B consume found conversation A's input")
		}
		msg, _, ok, err := store.Consume(ctx, orgID, convA)
		if err != nil {
			t.Fatalf("consume A: %v", err)
		}
		if !ok || msg != "for-A" {
			t.Errorf("conversation A input missing after B consume: msg=%q ok=%v", msg, ok)
		}
	})

	t.Run("Conversation_delete_cascades_pending_input", func(t *testing.T) {
		store, orgID, userID, seed := mk(t)
		conversationID := seed.Conversation(t, "cascade")
		seed.StagePending(t, conversationID, userID, "doomed")
		seed.DeleteConversation(t, conversationID)
		_, _, ok, err := store.Consume(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("consume after delete: %v", err)
		}
		if ok {
			t.Error("pending input survived run deletion (cascade did not fire)")
		}
	})
}
