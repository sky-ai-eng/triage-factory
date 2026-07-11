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
}

// RunRunPendingInputStoreConformance covers the RunPendingInputStore
// contract every backend impl must hold (TFAC-585): store-then-consume
// (destructive, exactly once), the idempotent upsert-keyed-by-run_id crash
// contract, per-run isolation, absence reads as ok=false (never an error),
// and FK cascade.
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

	t.Run("Store_is_an_idempotent_upsert_keyed_by_run_id", func(t *testing.T) {
		store, orgID, userID, seed := mk(t)
		runID := seed.Run(t, "upsert")

		if err := store.Store(ctx, orgID, runID, userID, "first attempt"); err != nil {
			t.Fatalf("store first: %v", err)
		}
		// A retry (e.g. the requeue step failed and the user/client resends)
		// overwrites rather than accumulating a second row.
		if err := store.Store(ctx, orgID, runID, userID, "retried attempt"); err != nil {
			t.Fatalf("store retry: %v", err)
		}
		msg, _, ok, err := store.Consume(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("consume: %v", err)
		}
		if !ok {
			t.Fatal("consume ok=false, want true")
		}
		if msg != "retried attempt" {
			t.Errorf("message = %q, want the latest write %q (upsert must replace, not accumulate)", msg, "retried attempt")
		}
		// And nothing further is pending — proves there was only ever one row.
		_, _, ok, err = store.Consume(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("second consume: %v", err)
		}
		if ok {
			t.Error("a second row survived the upsert")
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
