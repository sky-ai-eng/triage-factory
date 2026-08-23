package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// PlacementOverrideStoreFactory hands the conformance suite a wired store and
// the orgID to scope every call. placement_overrides has no FK chain, so
// there is nothing to seed.
type PlacementOverrideStoreFactory func(t *testing.T) (store db.PlacementOverrideStore, orgID string)

// RunPlacementOverrideStoreConformance covers the contract every backend impl
// must hold: absence reads as (nil, nil) not an error; upsert then
// get round-trips a pin and a replica count; upsert is replace-wholesale
// keyed on (org, kind, value); list is org-scoped and ordered; delete reports
// matched and is idempotent.
func RunPlacementOverrideStoreConformance(t *testing.T, mk PlacementOverrideStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("Upsert_returns_the_stored_row", func(t *testing.T) {
		// The returned-row standard: what the write handed back is what a point
		// read finds. ov carries no updated_at, and the empty pin it is handed
		// is stored as NULL — the row and the argument disagree by design.
		store, orgID := mk(t)
		read := func() (*domain.PlacementOverride, error) {
			return store.Get(ctx, orgID, domain.PlacementKindRepo, "acme/ret")
		}

		first, err := store.Upsert(ctx, domain.PlacementOverride{
			OrgID: orgID, KeyKind: domain.PlacementKindRepo, KeyValue: "acme/ret", Replicas: 2,
		})
		if err != nil {
			t.Fatalf("Upsert (insert): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Upsert (insert)", first, read)
		if first.UpdatedAt.IsZero() || first.PinnedInstanceID != "" {
			t.Errorf("Upsert returned %+v, want a stamped updated_at and no pin", first)
		}

		replaced, err := store.Upsert(ctx, domain.PlacementOverride{
			OrgID: orgID, KeyKind: domain.PlacementKindRepo, KeyValue: "acme/ret",
			PinnedInstanceID: "exec-9", Replicas: 5,
		})
		if err != nil {
			t.Fatalf("Upsert (replace): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Upsert (replace)", replaced, read)
		if replaced.PinnedInstanceID != "exec-9" || replaced.Replicas != 5 {
			t.Errorf("Upsert (replace) returned %+v, want the replaced pin and replica count", replaced)
		}
	})

	t.Run("absent_get_is_nil_not_error", func(t *testing.T) {
		store, orgID := mk(t)
		ov, err := store.Get(ctx, orgID, domain.PlacementKindRepo, "acme/ghost")
		if err != nil {
			t.Fatalf("get absent: %v", err)
		}
		if ov != nil {
			t.Fatalf("absent override must be nil, got %+v", ov)
		}
	})

	t.Run("upsert_pin_roundtrips", func(t *testing.T) {
		store, orgID := mk(t)
		want := domain.PlacementOverride{OrgID: orgID, KeyKind: domain.PlacementKindRepo, KeyValue: "acme/web", PinnedInstanceID: "exec-7"}
		if _, err := store.Upsert(ctx, want); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		got, err := store.Get(ctx, orgID, domain.PlacementKindRepo, "acme/web")
		if err != nil || got == nil {
			t.Fatalf("get: %v (got %+v)", err, got)
		}
		if got.PinnedInstanceID != "exec-7" || got.Replicas != 0 {
			t.Fatalf("roundtrip mismatch: %+v", got)
		}
		if got.UpdatedAt.IsZero() {
			t.Fatalf("updated_at not stamped")
		}
	})

	t.Run("upsert_replaces_wholesale", func(t *testing.T) {
		store, orgID := mk(t)
		key := "acme/mono"
		if _, err := store.Upsert(ctx, domain.PlacementOverride{OrgID: orgID, KeyKind: domain.PlacementKindRepo, KeyValue: key, PinnedInstanceID: "exec-1"}); err != nil {
			t.Fatalf("upsert pin: %v", err)
		}
		// Re-upsert the same key with a replica count and no pin — must
		// replace, not merge (pin cleared, replicas set).
		if _, err := store.Upsert(ctx, domain.PlacementOverride{OrgID: orgID, KeyKind: domain.PlacementKindRepo, KeyValue: key, Replicas: 3}); err != nil {
			t.Fatalf("upsert replicas: %v", err)
		}
		got, err := store.Get(ctx, orgID, domain.PlacementKindRepo, key)
		if err != nil || got == nil {
			t.Fatalf("get: %v (%+v)", err, got)
		}
		if got.PinnedInstanceID != "" {
			t.Fatalf("pin should have been replaced away, got %q", got.PinnedInstanceID)
		}
		if got.Replicas != 3 {
			t.Fatalf("replicas = %d, want 3", got.Replicas)
		}
	})

	t.Run("list_is_org_scoped_and_ordered", func(t *testing.T) {
		store, orgID := mk(t)
		_, _ = store.Upsert(ctx, domain.PlacementOverride{OrgID: orgID, KeyKind: domain.PlacementKindRepo, KeyValue: "b/two", Replicas: 2})
		_, _ = store.Upsert(ctx, domain.PlacementOverride{OrgID: orgID, KeyKind: domain.PlacementKindRepo, KeyValue: "a/one", PinnedInstanceID: "x"})
		// key_kind is a free-text column (no FK/CHECK) — an arbitrary second
		// kind here proves the ORDER BY (key_kind, key_value) sorts across
		// kinds, not just within the one the app mints today.
		_, _ = store.Upsert(ctx, domain.PlacementOverride{OrgID: orgID, KeyKind: "hostgroup", KeyValue: "HG", Replicas: 1})

		list, err := store.List(ctx, orgID)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) != 3 {
			t.Fatalf("expected 3 overrides, got %d: %+v", len(list), list)
		}
		// Ordered (key_kind, key_value): "hostgroup" sorts before "repo"
		// alphabetically, then key_value within a kind.
		if list[0].KeyKind != "hostgroup" || list[0].KeyValue != "HG" {
			t.Fatalf("first should be hostgroup/HG: %+v", list)
		}
		if list[1].KeyValue != "a/one" || list[2].KeyValue != "b/two" {
			t.Fatalf("repo keys out of order: %+v", list)
		}
	})

	t.Run("delete_reports_matched_and_is_idempotent", func(t *testing.T) {
		store, orgID := mk(t)
		key := "acme/del"
		_, _ = store.Upsert(ctx, domain.PlacementOverride{OrgID: orgID, KeyKind: domain.PlacementKindRepo, KeyValue: key, PinnedInstanceID: "z"})

		matched, err := store.Delete(ctx, orgID, domain.PlacementKindRepo, key)
		if err != nil || !matched {
			t.Fatalf("first delete: matched=%v err=%v", matched, err)
		}
		matched, err = store.Delete(ctx, orgID, domain.PlacementKindRepo, key)
		if err != nil {
			t.Fatalf("second delete err: %v", err)
		}
		if matched {
			t.Fatalf("second delete should report matched=false")
		}
		got, _ := store.Get(ctx, orgID, domain.PlacementKindRepo, key)
		if got != nil {
			t.Fatalf("row should be gone after delete, got %+v", got)
		}
	})
}
