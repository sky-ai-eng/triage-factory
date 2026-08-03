package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// RunPendingInputStoreFactory is what a per-backend test file hands to
// RunRunPendingInputStoreConformance. Returns:
//   - the wired RunPendingInputStore impl,
//   - the orgID and a userID to pass to every call,
//   - a RunPendingInputSeeder the harness uses to stage the run FK chain.
type RunPendingInputStoreFactory func(t *testing.T) (store db.RunPendingInputStore, orgID, userID string, seed RunPendingInputSeeder)

// RunPendingInputSeeder is a bag of callbacks the conformance suite uses to
// stage fixture rows RunPendingInputStore doesn't own.
type RunPendingInputSeeder struct {
	// Run inserts a run row and returns its id. suffix discriminates
	// per-subtest seeds so unique indexes don't collide.
	Run func(t *testing.T, suffix string) (runID string)

	// DeleteRun removes the run row so the FK ON DELETE CASCADE subtest can
	// verify a purged run takes its pending input with it.
	DeleteRun func(t *testing.T, runID string)

	// SecondUser returns a second user id valid for the same org, so the
	// multi-author subtests can queue rows from two people. The store's
	// attribution rule (newest row wins) is unobservable with one.
	SecondUser func(t *testing.T) string
}

// RunRunPendingInputStoreConformance covers the RunPendingInputStore
// contract every backend impl must hold (TFAC-585): store-then-consume
// (destructive, exactly once), the append-and-join queue contract, Peek and
// Consume agreeing on what is pending, per-run isolation, absence reads as
// ok=false (never an error), and FK cascade.
func RunRunPendingInputStoreConformance(t *testing.T, mk RunPendingInputStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("Store_then_Consume_returns_and_drains", func(t *testing.T) {
		store, orgID, userID, seed := mk(t)
		runID := seed.Run(t, "drain")

		if err := store.Store(ctx, orgID, runID, userID, "pick this back up"); err != nil {
			t.Fatalf("store: %v", err)
		}
		msg, gotUser, ok, err := store.Consume(ctx, orgID, runID)
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
		_, _, ok, err = store.Consume(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("second consume: %v", err)
		}
		if ok {
			t.Error("second consume ok=true, want false (consume must drain)")
		}
	})

	t.Run("Peek_returns_without_draining_then_Consume_deletes", func(t *testing.T) {
		store, orgID, userID, seed := mk(t)
		runID := seed.Run(t, "peek")

		if err := store.Store(ctx, orgID, runID, userID, "still here"); err != nil {
			t.Fatalf("store: %v", err)
		}
		// Peek twice: both return the row (non-destructive).
		for i := 0; i < 2; i++ {
			msg, gotUser, ok, err := store.Peek(ctx, orgID, runID)
			if err != nil {
				t.Fatalf("peek %d: %v", i, err)
			}
			if !ok || msg != "still here" || gotUser != userID {
				t.Fatalf("peek %d = (%q, %q, %v); want (%q, %q, true) — peek must not drain", i, msg, gotUser, ok, "still here", userID)
			}
		}
		// Consume then drains it, and a following Peek finds nothing.
		if _, _, ok, err := store.Consume(ctx, orgID, runID); err != nil || !ok {
			t.Fatalf("consume after peek = (ok=%v, err=%v); want ok=true", ok, err)
		}
		if _, _, ok, err := store.Peek(ctx, orgID, runID); err != nil || ok {
			t.Fatalf("peek after consume = (ok=%v, err=%v); want ok=false", ok, err)
		}
	})

	t.Run("Consume_absent_is_ok_false_not_an_error", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		runID := seed.Run(t, "empty")
		_, _, ok, err := store.Consume(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("consume: %v", err)
		}
		if ok {
			t.Error("consume of an unstaged run reported ok=true")
		}
	})

	// The heart of TFAC-735: two messages sent to a parked conversation before
	// it wakes are both delivered. The store this replaced deleted the first
	// one — no error, no log, nothing in the transcript.
	t.Run("Store_appends_and_Consume_joins_in_order", func(t *testing.T) {
		store, orgID, userID, seed := mk(t)
		runID := seed.Run(t, "append")

		if err := store.Store(ctx, orgID, runID, userID, "fix the null check"); err != nil {
			t.Fatalf("store first: %v", err)
		}
		if err := store.Store(ctx, orgID, runID, userID, "actually no, fix the test"); err != nil {
			t.Fatalf("store second: %v", err)
		}
		msg, _, ok, err := store.Consume(ctx, orgID, runID)
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
		if _, _, ok, err := store.Consume(ctx, orgID, runID); err != nil || ok {
			t.Errorf("second consume = (ok=%v, err=%v); want ok=false — the flush must drain every joined row", ok, err)
		}
	})

	// Peek routes the claim to the resume path and Consume delivers; they read
	// the same rows a rehydrate apart, so they must read them the same way.
	t.Run("Peek_and_Consume_agree_across_several_rows", func(t *testing.T) {
		store, orgID, userID, seed := mk(t)
		runID := seed.Run(t, "agree")

		for _, m := range []string{"one", "two", "three"} {
			if err := store.Store(ctx, orgID, runID, userID, m); err != nil {
				t.Fatalf("store %q: %v", m, err)
			}
		}
		peeked, peekedUser, ok, err := store.Peek(ctx, orgID, runID)
		if err != nil || !ok {
			t.Fatalf("peek = (ok=%v, err=%v); want ok=true", ok, err)
		}
		consumed, consumedUser, ok, err := store.Consume(ctx, orgID, runID)
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
		runID := seed.Run(t, "authors")
		teammate := seed.SecondUser(t)

		if err := store.Store(ctx, orgID, runID, userID, "my follow-up"); err != nil {
			t.Fatalf("store mine: %v", err)
		}
		if err := store.Store(ctx, orgID, runID, teammate, "and while you're in there"); err != nil {
			t.Fatalf("store teammate's: %v", err)
		}
		msg, gotUser, ok, err := store.Consume(ctx, orgID, runID)
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

	t.Run("Consume_is_per_run_isolated", func(t *testing.T) {
		store, orgID, userID, seed := mk(t)
		runA := seed.Run(t, "iso-a")
		runB := seed.Run(t, "iso-b")
		if err := store.Store(ctx, orgID, runA, userID, "for-A"); err != nil {
			t.Fatalf("store A: %v", err)
		}
		_, _, ok, err := store.Consume(ctx, orgID, runB)
		if err != nil {
			t.Fatalf("consume B: %v", err)
		}
		if ok {
			t.Error("run B consume found run A's input")
		}
		msg, _, ok, err := store.Consume(ctx, orgID, runA)
		if err != nil {
			t.Fatalf("consume A: %v", err)
		}
		if !ok || msg != "for-A" {
			t.Errorf("run A input missing after B consume: msg=%q ok=%v", msg, ok)
		}
	})

	t.Run("Run_delete_cascades_pending_input", func(t *testing.T) {
		store, orgID, userID, seed := mk(t)
		runID := seed.Run(t, "cascade")
		if err := store.Store(ctx, orgID, runID, userID, "doomed"); err != nil {
			t.Fatalf("store: %v", err)
		}
		seed.DeleteRun(t, runID)
		_, _, ok, err := store.Consume(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("consume after delete: %v", err)
		}
		if ok {
			t.Error("pending input survived run deletion (cascade did not fire)")
		}
	})
}
