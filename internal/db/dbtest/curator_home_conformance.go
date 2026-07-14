package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// CuratorHomeStoreFactory hands the conformance suite a wired store and the
// orgID to scope every call. curator_homes has no FK chain (org_id / project_id
// are plain text, instances-family convention), so there is nothing to seed.
type CuratorHomeStoreFactory func(t *testing.T) (store db.CuratorHomeStore, orgID string)

// RunCuratorHomeStoreConformance covers the contract every backend impl must
// hold: absence reads as (nil, nil) not an error; upsert then get round-trips
// the home + boot epoch; a re-home replaces the mapping wholesale and refreshes
// updated_at while preserving homed_at; clear is idempotent and scoped.
func RunCuratorHomeStoreConformance(t *testing.T, mk CuratorHomeStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("absent_get_is_nil_not_error", func(t *testing.T) {
		store, orgID := mk(t)
		h, err := store.Get(ctx, orgID, "proj-ghost")
		if err != nil {
			t.Fatalf("get absent: %v", err)
		}
		if h != nil {
			t.Fatalf("absent home must be nil, got %+v", h)
		}
	})

	t.Run("upsert_roundtrips", func(t *testing.T) {
		store, orgID := mk(t)
		if err := store.Upsert(ctx, orgID, "proj-a", "exec-7", 3); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		got, err := store.Get(ctx, orgID, "proj-a")
		if err != nil || got == nil {
			t.Fatalf("get: %v (got %+v)", err, got)
		}
		if got.HomeInstanceID != "exec-7" || got.HomeBootEpoch != 3 {
			t.Fatalf("roundtrip mismatch: %+v", got)
		}
		if got.HomedAt.IsZero() || got.UpdatedAt.IsZero() {
			t.Fatalf("timestamps not stamped: %+v", got)
		}
	})

	t.Run("rehome_replaces_and_keeps_homed_at", func(t *testing.T) {
		store, orgID := mk(t)
		if err := store.Upsert(ctx, orgID, "proj-b", "exec-1", 1); err != nil {
			t.Fatalf("upsert first: %v", err)
		}
		first, err := store.Get(ctx, orgID, "proj-b")
		if err != nil || first == nil {
			t.Fatalf("get first: %v (%+v)", err, first)
		}
		// Re-home to a different executor: home + epoch replace; homed_at is the
		// original mint (sticky); updated_at moves forward (or stays equal —
		// same-instant writes are legal, so assert not-before rather than
		// strictly-after).
		if err := store.Upsert(ctx, orgID, "proj-b", "exec-2", 5); err != nil {
			t.Fatalf("re-home: %v", err)
		}
		second, err := store.Get(ctx, orgID, "proj-b")
		if err != nil || second == nil {
			t.Fatalf("get second: %v (%+v)", err, second)
		}
		if second.HomeInstanceID != "exec-2" || second.HomeBootEpoch != 5 {
			t.Fatalf("re-home did not replace: %+v", second)
		}
		if !second.HomedAt.Equal(first.HomedAt) {
			t.Fatalf("homed_at must be preserved across re-home: first=%v second=%v", first.HomedAt, second.HomedAt)
		}
		if second.UpdatedAt.Before(first.UpdatedAt) {
			t.Fatalf("updated_at moved backward: first=%v second=%v", first.UpdatedAt, second.UpdatedAt)
		}
	})

	t.Run("clear_is_idempotent_and_scoped", func(t *testing.T) {
		store, orgID := mk(t)
		if err := store.Upsert(ctx, orgID, "proj-c", "exec-9", 2); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if err := store.Clear(ctx, orgID, "proj-c"); err != nil {
			t.Fatalf("clear: %v", err)
		}
		got, _ := store.Get(ctx, orgID, "proj-c")
		if got != nil {
			t.Fatalf("row should be gone after clear, got %+v", got)
		}
		// Idempotent: clearing a missing row is not an error.
		if err := store.Clear(ctx, orgID, "proj-c"); err != nil {
			t.Fatalf("second clear should be a no-op, got %v", err)
		}
	})
}
