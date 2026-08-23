package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ModelAvailabilityStoreFactory hands the conformance suite a wired store and
// the orgID to scope every call. The org row must already exist —
// model_availability carries an FK to orgs in both dialects.
//
// The factory is called once per subtest and hands back a store over storage
// the earlier subtests did not write to.
type ModelAvailabilityStoreFactory func(t *testing.T) (store db.ModelAvailabilityStore, orgID string)

// RunModelAvailabilityStoreConformance pins the availability lifecycle every
// backend must hold. The lifecycle is the whole feature — a picker badge is
// only worth anything if the states mean exactly one thing each — so each
// transition in it gets a case here:
//
//   - absent is an answer, not a miss: never probed reads as no row.
//   - a refusal stores red WITH the provider's own detail.
//   - green stores no detail, even when a caller passes one: a working model
//     has nothing to explain and a stale refusal riding on it would be false.
//   - green → red on a failed manual re-test. Green is terminal for AUTOMATIC
//     transitions only; the user asked for the truth and gets it.
//   - provider is part of the identity: two providers' rows for one key
//     coexist rather than overwriting each other.
//
// Two properties are deliberately absent. An inconclusive probe writes
// NOTHING, so it has no store-level spelling at all — that is pinned where it
// lives, in the prober's classifier and at the endpoint. And surviving a
// process restart is a question about the backing store rather than about this
// SQL, so it is pinned per dialect: SQLite reopens a file-backed database,
// Postgres is a separate server by construction.
func RunModelAvailabilityStoreConformance(t *testing.T, mk ModelAvailabilityStoreFactory) {
	t.Helper()
	ctx := context.Background()

	const (
		anthropic = "anthropic"
		bedrock   = "bedrock"
		model     = "claude-sonnet-5"
	)

	t.Run("never_probed_reads_as_absent", func(t *testing.T) {
		// The rollout property: every existing deployment has an empty table,
		// and it must read exactly like a build without one.
		store, orgID := mk(t)
		row, err := store.Get(ctx, orgID, anthropic, model)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if row != nil {
			t.Errorf("Get on an unprobed model = %+v, want nil", row)
		}
		all, err := store.List(ctx, orgID)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(all) != 0 {
			t.Errorf("List on an unprobed org = %v, want empty", all)
		}
	})

	t.Run("refusal_stores_red_with_the_detail", func(t *testing.T) {
		store, orgID := mk(t)
		read := func() (*domain.ModelAvailability, error) { return store.Get(ctx, orgID, anthropic, model) }

		const detail = "inference: provider error: model not found (HTTP 404) [not_found_error]"
		before := time.Now().UTC().Add(-time.Second)
		red, err := store.Record(ctx, orgID, anthropic, model, domain.ModelAvailabilityRed, detail)
		if err != nil {
			t.Fatalf("Record(red): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Record(red)", red, read)
		if red.State != domain.ModelAvailabilityRed {
			t.Errorf("state = %q, want %q", red.State, domain.ModelAvailabilityRed)
		}
		if red.Detail != detail {
			t.Errorf("detail = %q, want the provider's own refusal %q", red.Detail, detail)
		}
		// Stamped by the write, not supplied: a caller cannot claim a check it
		// did not run.
		if red.CheckedAt.Before(before) {
			t.Errorf("checked_at = %s, want a stamp from this write (after %s)", red.CheckedAt, before)
		}
	})

	t.Run("green_carries_no_detail", func(t *testing.T) {
		// Passing a detail with a green result is a caller bug, and the store
		// drops it rather than storing a refusal message on a model that works.
		store, orgID := mk(t)
		green, err := store.Record(ctx, orgID, anthropic, model, domain.ModelAvailabilityGreen, "ignore me")
		if err != nil {
			t.Fatalf("Record(green): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Record(green)", green,
			func() (*domain.ModelAvailability, error) { return store.Get(ctx, orgID, anthropic, model) })
		if green.State != domain.ModelAvailabilityGreen || green.Detail != "" {
			t.Errorf("Record(green) = %+v, want green with no detail", green)
		}
	})

	t.Run("green_then_a_failed_manual_retest_is_red", func(t *testing.T) {
		// Green is terminal for AUTOMATIC transitions — nothing on a timer
		// touches it. A person pressing "test connection" is not automatic, and
		// what they asked for is the truth.
		store, orgID := mk(t)
		if _, err := store.Record(ctx, orgID, anthropic, model, domain.ModelAvailabilityGreen, ""); err != nil {
			t.Fatalf("Record(green): %v", err)
		}
		const detail = "inference: provider error: AccessDeniedException (HTTP 403)"
		red, err := store.Record(ctx, orgID, anthropic, model, domain.ModelAvailabilityRed, detail)
		if err != nil {
			t.Fatalf("Record(red after green): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Record(red after green)", red,
			func() (*domain.ModelAvailability, error) { return store.Get(ctx, orgID, anthropic, model) })
		if red.State != domain.ModelAvailabilityRed || red.Detail != detail {
			t.Errorf("re-test after green = %+v, want red carrying the refusal", red)
		}
		// And back again: a red cleared by a later successful test.
		green, err := store.Record(ctx, orgID, anthropic, model, domain.ModelAvailabilityGreen, "")
		if err != nil {
			t.Fatalf("Record(green after red): %v", err)
		}
		if green.State != domain.ModelAvailabilityGreen || green.Detail != "" {
			t.Errorf("re-test after red = %+v, want green with the refusal cleared", green)
		}
	})

	t.Run("provider_is_part_of_the_identity", func(t *testing.T) {
		// One model key reached two ways is two rows: what a probe establishes
		// is that a CREDENTIAL PATH works, and an Anthropic key saying yes
		// tells you nothing about the org's Bedrock grants.
		store, orgID := mk(t)
		if _, err := store.Record(ctx, orgID, anthropic, model, domain.ModelAvailabilityGreen, ""); err != nil {
			t.Fatalf("Record(anthropic): %v", err)
		}
		if _, err := store.Record(ctx, orgID, bedrock, model, domain.ModelAvailabilityRed, "not granted"); err != nil {
			t.Fatalf("Record(bedrock): %v", err)
		}
		all, err := store.List(ctx, orgID)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(all) != 2 {
			t.Fatalf("List = %v, want one row per (provider, model)", all)
		}
		// The list is sorted by (provider, model_key), so anthropic comes
		// first however the rows were written — the order is the sort, not the
		// insert.
		if all[0].Provider != anthropic || all[1].Provider != bedrock {
			t.Errorf("List order = [%s %s], want sorted by provider", all[0].Provider, all[1].Provider)
		}
		if all[0].State != domain.ModelAvailabilityGreen || all[1].State != domain.ModelAvailabilityRed {
			t.Errorf("List = %+v, want each path's own verdict", all)
		}
	})

	t.Run("a_key_this_build_does_not_offer_stores_inertly", func(t *testing.T) {
		// model_key is deliberately FK-free: the catalog is a compiled-in file,
		// not a table, so a row for a retired key resolves to nothing (callers
		// iterate the catalog) rather than being refused at write time.
		store, orgID := mk(t)
		row, err := store.Record(ctx, orgID, anthropic, "claude-retired-9", domain.ModelAvailabilityGreen, "")
		if err != nil {
			t.Fatalf("Record for an unoffered key: %v", err)
		}
		if row.ModelKey != "claude-retired-9" {
			t.Errorf("Record stored %q, want the key as written", row.ModelKey)
		}
	})
}
