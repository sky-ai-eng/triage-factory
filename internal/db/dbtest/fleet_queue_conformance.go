package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// FleetQueueSharesFactory is what a per-backend test file hands to
// RunFleetQueueSharesConformance. Returns the wired ConversationQueueStore, the orgID
// its seeder stages rows under, and the seeder.
type FleetQueueSharesFactory func(t *testing.T) (store db.ConversationQueueStore, orgID string, seed FleetQueueSharesSeeder)

// FleetQueueSharesSeeder stages the run + settings states FleetQueueShares
// reads. The store's own guarded flips can't reach terminal/dormant/active
// statuses on demand, so ForceStatus writes them directly.
type FleetQueueSharesSeeder struct {
	// EnqueueConversation stages one queued run (under a running blueprint_run) and
	// returns its id.
	EnqueueConversation func(t *testing.T) (conversationID string)

	// ForceStatus rewrites a run's status directly, bypassing the store's
	// guards.
	ForceStatus func(t *testing.T, conversationID, status string)

	// SetMaxConcurrentRuns upserts the org's max_concurrent_runs cap; a nil
	// cap writes SQL NULL (unlimited).
	SetMaxConcurrentRuns func(t *testing.T, cap *int)
}

// RunFleetQueueSharesConformance covers the per-backend FleetQueueShares
// contract: it counts an org's active (slot-occupying: an unreleased claim,
// whatever phase that claim is in) and queued (needs-driving) runs, excludes
// terminal and hibernated (`open`) runs from both, and
// reports the configured cap — nil for an unset or non-positive value.
// Multi-org fairness ordering is a Postgres-only concern exercised in that
// backend's own tests; this suite runs single-org against both dialects
// (SQLite is N=1).
func RunFleetQueueSharesConformance(t *testing.T, mk FleetQueueSharesFactory) {
	t.Helper()
	ctx := context.Background()

	// shareFor returns the org's row from FleetQueueShares, or a zero value
	// with found=false when the org has no active/queued runs (omitted).
	shareFor := func(t *testing.T, store db.ConversationQueueStore, orgID string) (db.OrgQueueShare, bool) {
		t.Helper()
		shares, err := store.FleetQueueShares(ctx)
		if err != nil {
			t.Fatalf("FleetQueueShares: %v", err)
		}
		for _, s := range shares {
			if s.OrgID == orgID {
				return s, true
			}
		}
		return db.OrgQueueShare{}, false
	}

	t.Run("counts_active_queued_and_cap", func(t *testing.T) {
		store, orgID, seed := mk(t)

		// No runs → the org contributes no row.
		if s, found := shareFor(t, store, orgID); found {
			t.Fatalf("empty queue reported a share %+v, want the org omitted", s)
		}

		// 2 active + 1 still queued. Active is an engagement now, so the two
		// active runs are made active by actually claiming them — which is
		// also the only way to reach the state, the conversation row itself
		// carrying no "running" any more.
		seed.EnqueueConversation(t)
		seed.EnqueueConversation(t)
		seed.EnqueueConversation(t)
		for i := 0; i < 2; i++ {
			claimed, err := store.ClaimNextConversation(ctx, "fleet-share-executor", 1, db.ClaimPlacement{})
			if err != nil {
				t.Fatalf("ClaimNextConversation: %v", err)
			}
			if claimed == nil {
				t.Fatalf("ClaimNextConversation returned nothing on claim %d of 2", i+1)
			}
		}

		s, found := shareFor(t, store, orgID)
		if !found {
			t.Fatalf("org %s missing from FleetQueueShares after enqueue", orgID)
		}
		if s.Active != 2 || s.Queued != 1 {
			t.Fatalf("share = {active:%d queued:%d}, want {active:2 queued:1}", s.Active, s.Queued)
		}
		if s.MaxConcurrentRuns != nil {
			t.Fatalf("cap = %d with no org_settings cap set, want nil (unlimited)", *s.MaxConcurrentRuns)
		}

		// A positive cap surfaces verbatim.
		five := 5
		seed.SetMaxConcurrentRuns(t, &five)
		if s, _ := shareFor(t, store, orgID); s.MaxConcurrentRuns == nil || *s.MaxConcurrentRuns != 5 {
			t.Fatalf("cap = %v after set to 5, want 5", s.MaxConcurrentRuns)
		}

		// Non-positive and NULL both mean unlimited (nil).
		zero := 0
		seed.SetMaxConcurrentRuns(t, &zero)
		if s, _ := shareFor(t, store, orgID); s.MaxConcurrentRuns != nil {
			t.Fatalf("cap = %d after set to 0, want nil (unlimited)", *s.MaxConcurrentRuns)
		}
		seed.SetMaxConcurrentRuns(t, nil)
		if s, _ := shareFor(t, store, orgID); s.MaxConcurrentRuns != nil {
			t.Fatalf("cap = %d after set to NULL, want nil (unlimited)", *s.MaxConcurrentRuns)
		}
	})

	t.Run("terminal_and_dormant_runs_excluded", func(t *testing.T) {
		store, orgID, seed := mk(t)

		queued := seed.EnqueueConversation(t) // counts as queued
		_ = queued
		done := seed.EnqueueConversation(t)
		seed.ForceStatus(t, done, "completed") // terminal: excluded from both
		parked := seed.EnqueueConversation(t)
		seed.ForceStatus(t, parked, "open") // hibernated: excluded from active

		s, found := shareFor(t, store, orgID)
		if !found {
			t.Fatalf("org %s missing from FleetQueueShares", orgID)
		}
		if s.Active != 0 || s.Queued != 1 {
			t.Fatalf("share = {active:%d queued:%d}, want {active:0 queued:1} (terminal + dormant excluded)", s.Active, s.Queued)
		}
	})
}
