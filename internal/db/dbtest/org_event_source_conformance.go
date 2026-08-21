package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// OrgEventSourceStoreFactory hands the conformance suite a wired store and the
// orgID to scope every call. The org row must already exist — org_event_sources
// carries an FK to orgs in both dialects.
type OrgEventSourceStoreFactory func(t *testing.T) (store db.OrgEventSourceStore, orgID string)

// RunOrgEventSourceStoreConformance covers the contract every backend impl must
// hold: absence is an answer rather than a row; the upsert returns what it
// stored; re-enabling clears the pause stamps instead of leaving the last
// pause's on an active source; the disabled list is sorted; and a kind outside
// this build's vocabulary stores inertly rather than being refused.
func RunOrgEventSourceStoreConformance(t *testing.T, mk OrgEventSourceStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("absent_row_is_not_disabled", func(t *testing.T) {
		// The rollout property: every existing deployment has an empty table,
		// and it must read exactly like a build without one.
		store, orgID := mk(t)
		row, err := store.Get(ctx, orgID, "jira")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if row != nil {
			t.Errorf("Get on an untouched source = %+v, want nil", row)
		}
		disabled, err := store.ListDisabled(ctx, orgID)
		if err != nil {
			t.Fatalf("ListDisabled: %v", err)
		}
		if len(disabled) != 0 {
			t.Errorf("ListDisabled on an untouched org = %v, want empty", disabled)
		}
	})

	t.Run("SetDisabled_returns_the_stored_row", func(t *testing.T) {
		// The returned-row standard: what the write handed back is what a point
		// read finds. Both stamps are derived by the statement, so the returned
		// row carries values no caller supplied.
		store, orgID := mk(t)
		read := func() (*domain.OrgEventSource, error) { return store.Get(ctx, orgID, "jira") }

		inserted, err := store.SetDisabled(ctx, orgID, "jira", true, "11111111-1111-1111-1111-111111111111")
		if err != nil {
			t.Fatalf("SetDisabled (insert): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "SetDisabled (insert)", inserted, read)
		if !inserted.Disabled || inserted.DisabledAt == nil || inserted.DisabledBy == nil {
			t.Errorf("SetDisabled(true) returned %+v, want disabled with both stamps set", inserted)
		}

		// Idempotent: disabling an already-disabled source is a legal write, not
		// a conflict, because the admin surface has no way to know the row's
		// state before it writes.
		again, err := store.SetDisabled(ctx, orgID, "jira", true, "22222222-2222-2222-2222-222222222222")
		if err != nil {
			t.Fatalf("SetDisabled (repeat): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "SetDisabled (repeat)", again, read)
		if again.DisabledBy == nil || *again.DisabledBy != "22222222-2222-2222-2222-222222222222" {
			t.Errorf("SetDisabled (repeat) returned disabled_by %v, want the second actor", again.DisabledBy)
		}
	})

	t.Run("re_enable_clears_the_pause_stamps", func(t *testing.T) {
		// The row describes the LIVE policy. A disabled_at left on an enabled
		// source would be a claim nothing in the schema can refuse; the history
		// of who paused what is what access_changes is for.
		store, orgID := mk(t)
		if _, err := store.SetDisabled(ctx, orgID, "github", true, "11111111-1111-1111-1111-111111111111"); err != nil {
			t.Fatalf("SetDisabled (disable): %v", err)
		}
		enabled, err := store.SetDisabled(ctx, orgID, "github", false, "11111111-1111-1111-1111-111111111111")
		if err != nil {
			t.Fatalf("SetDisabled (enable): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "SetDisabled (enable)", enabled,
			func() (*domain.OrgEventSource, error) { return store.Get(ctx, orgID, "github") })
		if enabled.Disabled || enabled.DisabledAt != nil || enabled.DisabledBy != nil {
			t.Errorf("SetDisabled(false) returned %+v, want enabled with both stamps cleared", enabled)
		}
		disabled, err := store.ListDisabled(ctx, orgID)
		if err != nil {
			t.Fatalf("ListDisabled: %v", err)
		}
		if len(disabled) != 0 {
			t.Errorf("ListDisabled after re-enable = %v, want empty — the row survives, the pause does not", disabled)
		}
	})

	t.Run("ListDisabled_names_only_the_paused_sources_sorted", func(t *testing.T) {
		store, orgID := mk(t)
		for _, kind := range []string{"jira", "github", "slack"} {
			if _, err := store.SetDisabled(ctx, orgID, kind, true, ""); err != nil {
				t.Fatalf("SetDisabled %s: %v", kind, err)
			}
		}
		if _, err := store.SetDisabled(ctx, orgID, "github", false, ""); err != nil {
			t.Fatalf("SetDisabled github (enable): %v", err)
		}
		got, err := store.ListDisabled(ctx, orgID)
		if err != nil {
			t.Fatalf("ListDisabled: %v", err)
		}
		want := []string{"jira", "slack"}
		if len(got) != len(want) {
			t.Fatalf("ListDisabled = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("ListDisabled = %v, want %v", got, want)
			}
		}
		// The System variant reads the same rows through the admin pool — the
		// router and the poller must not see a different policy than the
		// request path does.
		sys, err := store.ListDisabledSystem(ctx, orgID)
		if err != nil {
			t.Fatalf("ListDisabledSystem: %v", err)
		}
		if len(sys) != len(want) {
			t.Errorf("ListDisabledSystem = %v, want %v", sys, want)
		}
	})

	t.Run("kind_outside_the_vocabulary_stores_inertly", func(t *testing.T) {
		// kind is deliberately FK-free: the source vocabulary is a runtime
		// registry, not a table, so a row naming a source this build does not
		// carry resolves to nothing rather than being refused at write time.
		store, orgID := mk(t)
		row, err := store.SetDisabled(ctx, orgID, "gerrit", true, "")
		if err != nil {
			t.Fatalf("SetDisabled unknown kind: %v", err)
		}
		if row.Kind != "gerrit" || !row.Disabled {
			t.Errorf("SetDisabled unknown kind returned %+v, want it stored as written", row)
		}
		if row.DisabledBy != nil {
			t.Errorf("SetDisabled with no actor returned disabled_by %v, want nil", row.DisabledBy)
		}
	})
}
