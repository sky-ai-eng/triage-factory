package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// WorkspaceSnapshotStoreFactory is what a per-backend test file hands to
// RunWorkspaceSnapshotStoreConformance. Returns:
//   - the wired WorkspaceSnapshotStore impl,
//   - the orgID to pass to every call,
//   - a WorkspaceSnapshotSeeder for the blueprint_runs FK the key points at
//     (backends build that chain differently).
type WorkspaceSnapshotStoreFactory func(t *testing.T) (store db.WorkspaceSnapshotStore, orgID string, seed WorkspaceSnapshotSeeder)

// WorkspaceSnapshotSeeder stages the rows the store doesn't own.
type WorkspaceSnapshotSeeder struct {
	// BlueprintRun inserts a blueprint_runs row and returns its id — the
	// snapshot key's other half. suffix discriminates per-subtest seeds.
	BlueprintRun func(t *testing.T, suffix string) (blueprintRunID string)

	// DeleteBlueprintRun removes the blueprint_runs row, so the cascade
	// subtest can prove a purged run takes its snapshot state with it.
	DeleteBlueprintRun func(t *testing.T, blueprintRunID string)
}

// Claim ids the suite writes as writers. Real uuids because the Postgres
// column is a uuid — a bare label would fail the bind there and pass on
// SQLite, which is the dialect drift the shared suite exists to catch.
const (
	snapshotWriterA = "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"
	snapshotWriterB = "bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb"
	snapshotWriterC = "cccccccc-3333-4333-8333-cccccccccccc"
)

// RunWorkspaceSnapshotStoreConformance covers the contract every backend must
// hold: the begin/finish/get/delete round-trip in both terminal directions,
// the completion CAS refusing a writer a successor has displaced, the
// unconditional takeover that displaces it, per-key isolation, and the FK
// cascade that keeps a purged blueprint run from leaving lifecycle behind.
func RunWorkspaceSnapshotStoreConformance(t *testing.T, mk WorkspaceSnapshotStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("no_row_until_a_snapshot_begins", func(t *testing.T) {
		store, orgID, seed := mk(t)
		br := seed.BlueprintRun(t, "silent")

		got, err := store.GetSnapshotStateSystem(ctx, orgID, br)
		if err != nil {
			t.Fatalf("get before begin: %v", err)
		}
		if got != nil {
			t.Fatalf("a key nobody has snapshotted reads as %+v, want nil — absence is what distinguishes 'never owed' from 'owed'", got)
		}
	})

	t.Run("begin_then_finish_ok_records_written", func(t *testing.T) {
		store, orgID, seed := mk(t)
		br := seed.BlueprintRun(t, "written")

		if err := store.BeginSnapshotSystem(ctx, orgID, br, snapshotWriterA); err != nil {
			t.Fatalf("begin: %v", err)
		}
		pending, err := store.GetSnapshotStateSystem(ctx, orgID, br)
		if err != nil {
			t.Fatalf("get after begin: %v", err)
		}
		if pending == nil {
			t.Fatal("begin recorded no row")
		}
		if pending.State != domain.WorkspaceSnapshotPending {
			t.Errorf("state after begin = %q, want pending", pending.State)
		}
		if pending.WriterClaimID != snapshotWriterA {
			t.Errorf("writer after begin = %q, want %q", pending.WriterClaimID, snapshotWriterA)
		}
		if pending.OrgID != orgID || pending.BlueprintRunID != br {
			t.Errorf("key round-trip = (%q, %q), want (%q, %q)", pending.OrgID, pending.BlueprintRunID, orgID, br)
		}
		if pending.UpdatedAt.IsZero() {
			t.Error("updated_at is zero — a waiter reads it to decide how long a pending write has been owed")
		}

		matched, err := store.FinishSnapshotSystem(ctx, orgID, br, snapshotWriterA, true)
		if err != nil {
			t.Fatalf("finish: %v", err)
		}
		if !matched {
			t.Fatal("finish by the row's own writer did not match")
		}
		done, err := store.GetSnapshotStateSystem(ctx, orgID, br)
		if err != nil {
			t.Fatalf("get after finish: %v", err)
		}
		if done == nil || done.State != domain.WorkspaceSnapshotWritten {
			t.Fatalf("state after a successful finish = %+v, want written", done)
		}
		if done.WriterClaimID != snapshotWriterA {
			t.Errorf("writer after finish = %q, want it preserved as %q", done.WriterClaimID, snapshotWriterA)
		}
		if done.UpdatedAt.Before(pending.UpdatedAt) {
			t.Errorf("updated_at went backwards: %v then %v", pending.UpdatedAt, done.UpdatedAt)
		}
	})

	t.Run("finish_not_ok_records_failed", func(t *testing.T) {
		store, orgID, seed := mk(t)
		br := seed.BlueprintRun(t, "failed")

		if err := store.BeginSnapshotSystem(ctx, orgID, br, snapshotWriterA); err != nil {
			t.Fatalf("begin: %v", err)
		}
		matched, err := store.FinishSnapshotSystem(ctx, orgID, br, snapshotWriterA, false)
		if err != nil {
			t.Fatalf("finish(failed): %v", err)
		}
		if !matched {
			t.Fatal("finish(failed) by the row's own writer did not match")
		}
		got, err := store.GetSnapshotStateSystem(ctx, orgID, br)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got == nil || got.State != domain.WorkspaceSnapshotFailed {
			t.Fatalf("state = %+v, want failed — a durable failure is what lets a waiter stop waiting", got)
		}
	})

	t.Run("finish_cas_refuses_a_superseded_writer", func(t *testing.T) {
		store, orgID, seed := mk(t)
		br := seed.BlueprintRun(t, "superseded")

		if err := store.BeginSnapshotSystem(ctx, orgID, br, snapshotWriterA); err != nil {
			t.Fatalf("begin A: %v", err)
		}
		if err := store.BeginSnapshotSystem(ctx, orgID, br, snapshotWriterB); err != nil {
			t.Fatalf("begin B (takeover): %v", err)
		}

		matched, err := store.FinishSnapshotSystem(ctx, orgID, br, snapshotWriterA, true)
		if err != nil {
			t.Fatalf("finish A: %v", err)
		}
		if matched {
			t.Fatal("A's finish matched after B took the key over — the CAS is what tells a stale writer it was superseded")
		}
		got, err := store.GetSnapshotStateSystem(ctx, orgID, br)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got == nil || got.State != domain.WorkspaceSnapshotPending || got.WriterClaimID != snapshotWriterB {
			t.Fatalf("row after the refused finish = %+v, want pending under %q — A must not close out B's write", got, snapshotWriterB)
		}

		// And B, who owns the key, still closes it out normally.
		matched, err = store.FinishSnapshotSystem(ctx, orgID, br, snapshotWriterB, true)
		if err != nil {
			t.Fatalf("finish B: %v", err)
		}
		if !matched {
			t.Fatal("B's finish did not match — the successor must be able to complete its own write")
		}
	})

	t.Run("begin_takes_over_a_terminal_row", func(t *testing.T) {
		store, orgID, seed := mk(t)
		br := seed.BlueprintRun(t, "takeover")

		if err := store.BeginSnapshotSystem(ctx, orgID, br, snapshotWriterA); err != nil {
			t.Fatalf("begin A: %v", err)
		}
		if _, err := store.FinishSnapshotSystem(ctx, orgID, br, snapshotWriterA, true); err != nil {
			t.Fatalf("finish A: %v", err)
		}
		// A later engagement parking the same blueprint starts a new
		// lifecycle on the same key: unconditional, so a written row is not a
		// tombstone that blocks the next snapshot.
		if err := store.BeginSnapshotSystem(ctx, orgID, br, snapshotWriterC); err != nil {
			t.Fatalf("begin C: %v", err)
		}
		got, err := store.GetSnapshotStateSystem(ctx, orgID, br)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got == nil || got.State != domain.WorkspaceSnapshotPending || got.WriterClaimID != snapshotWriterC {
			t.Fatalf("row after re-begin = %+v, want pending under %q", got, snapshotWriterC)
		}
	})

	t.Run("finish_is_not_reentrant", func(t *testing.T) {
		store, orgID, seed := mk(t)
		br := seed.BlueprintRun(t, "reentrant")

		if err := store.BeginSnapshotSystem(ctx, orgID, br, snapshotWriterA); err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := store.FinishSnapshotSystem(ctx, orgID, br, snapshotWriterA, true); err != nil {
			t.Fatalf("first finish: %v", err)
		}
		matched, err := store.FinishSnapshotSystem(ctx, orgID, br, snapshotWriterA, false)
		if err != nil {
			t.Fatalf("second finish: %v", err)
		}
		if matched {
			t.Fatal("a second finish matched — the CAS is guarded on pending, so a terminal row is closed")
		}
		got, err := store.GetSnapshotStateSystem(ctx, orgID, br)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got == nil || got.State != domain.WorkspaceSnapshotWritten {
			t.Fatalf("state = %+v, want the first finish's written to stand", got)
		}
	})

	t.Run("keys_are_independent", func(t *testing.T) {
		store, orgID, seed := mk(t)
		first := seed.BlueprintRun(t, "iso-first")
		second := seed.BlueprintRun(t, "iso-second")

		if err := store.BeginSnapshotSystem(ctx, orgID, first, snapshotWriterA); err != nil {
			t.Fatalf("begin first: %v", err)
		}
		if err := store.BeginSnapshotSystem(ctx, orgID, second, snapshotWriterB); err != nil {
			t.Fatalf("begin second: %v", err)
		}
		if _, err := store.FinishSnapshotSystem(ctx, orgID, first, snapshotWriterA, true); err != nil {
			t.Fatalf("finish first: %v", err)
		}

		other, err := store.GetSnapshotStateSystem(ctx, orgID, second)
		if err != nil {
			t.Fatalf("get second: %v", err)
		}
		if other == nil || other.State != domain.WorkspaceSnapshotPending || other.WriterClaimID != snapshotWriterB {
			t.Fatalf("second key = %+v, want its own pending row under %q", other, snapshotWriterB)
		}
	})

	t.Run("delete_removes_the_row_and_is_idempotent", func(t *testing.T) {
		store, orgID, seed := mk(t)
		br := seed.BlueprintRun(t, "delete")

		if err := store.BeginSnapshotSystem(ctx, orgID, br, snapshotWriterA); err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := store.FinishSnapshotSystem(ctx, orgID, br, snapshotWriterA, true); err != nil {
			t.Fatalf("finish: %v", err)
		}
		if err := store.DeleteSnapshotStateSystem(ctx, orgID, br); err != nil {
			t.Fatalf("delete: %v", err)
		}
		got, err := store.GetSnapshotStateSystem(ctx, orgID, br)
		if err != nil {
			t.Fatalf("get after delete: %v", err)
		}
		if got != nil {
			t.Fatalf("row survived the delete: %+v — a reaped snapshot must read as 'no lifecycle', not as written-pointing-at-nothing", got)
		}
		if err := store.DeleteSnapshotStateSystem(ctx, orgID, br); err != nil {
			t.Fatalf("second delete should be a no-op: %v", err)
		}
	})

	t.Run("purging_the_blueprint_run_cascades", func(t *testing.T) {
		store, orgID, seed := mk(t)
		br := seed.BlueprintRun(t, "cascade")

		if err := store.BeginSnapshotSystem(ctx, orgID, br, snapshotWriterA); err != nil {
			t.Fatalf("begin: %v", err)
		}
		seed.DeleteBlueprintRun(t, br)

		got, err := store.GetSnapshotStateSystem(ctx, orgID, br)
		if err != nil {
			t.Fatalf("get after purge: %v", err)
		}
		if got != nil {
			t.Fatalf("state outlived its blueprint run: %+v", got)
		}
	})
}
