package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// GitHubDeliverySeeder stages and inspects rows the dedup suite cannot reach
// through the store: the store has exactly one method, and it neither backdates
// a row nor counts them. Both are raw-SQL probes, one per backend.
type GitHubDeliverySeeder struct {
	// Stale records one delivery with received_at set that far in the past.
	// The prune's cutoff is the only thing a test can vary, so a row's age is
	// how this suite says "already expired" or "still inside the window".
	Stale func(t *testing.T, installationID, deliveryID string, age time.Duration)

	// Recorded reports whether (installationID, deliveryID) is still in the
	// table. Nothing in the product reads this — the dedup answer is the
	// store's return value — so asserting what the prune removed needs a probe
	// rather than a reader nobody would otherwise call.
	Recorded func(t *testing.T, installationID, deliveryID string) bool
}

// GitHubDeliveryFactory is what a per-backend test file hands to
// RunGitHubDeliveryDedupConformance. Each call returns a fresh, isolated
// backend so subtests don't leak delivery rows into one another.
type GitHubDeliveryFactory func(t *testing.T) (db.GitHubDeliveryStore, GitHubDeliverySeeder)

// RunGitHubDeliveryDedupConformance is the shared assertion suite for GitHub
// webhook delivery dedup, in both dialects. It covers the insert-conflict path
// that makes a redelivery recognizable, the negative space that keeps dedup
// per-delivery rather than per-subject, and the prune.
//
// The negative space is the half worth stating plainly: two DIFFERENT
// deliveries that describe the same installation must both take effect. GitHub
// sends one delivery per event, and an installation legitimately produces many
// — created, then suspend, then unsuspend. A store that deduped on the
// installation would swallow every one after the first and leave the mirror
// permanently wrong.
func RunGitHubDeliveryDedupConformance(t *testing.T, mk GitHubDeliveryFactory) {
	t.Helper()
	ctx := context.Background()

	// mark runs one delivery through the store and fails on a store error —
	// every subtest wants the fresh/duplicate answer, none want the plumbing.
	mark := func(t *testing.T, store db.GitHubDeliveryStore, installationID, deliveryID string) bool {
		t.Helper()
		fresh, err := store.MarkDeliveredSystem(ctx, installationID, deliveryID)
		if err != nil {
			t.Fatalf("MarkDeliveredSystem(%q, %q): %v", installationID, deliveryID, err)
		}
		return fresh
	}

	t.Run("FirstDeliveryIsFresh", func(t *testing.T) {
		store, _ := mk(t)
		if !mark(t, store, "4242", "d-1") {
			t.Error("fresh = false for a delivery never seen before; want true")
		}
	})

	t.Run("RedeliveryConflicts", func(t *testing.T) {
		// The defect this table exists for: an operator hits Redeliver, the
		// same delivery id arrives again, and the second copy must be
		// recognizable before anything is applied.
		store, _ := mk(t)
		if !mark(t, store, "4242", "d-1") {
			t.Fatal("first delivery reported as a duplicate; the test asserts nothing")
		}
		if mark(t, store, "4242", "d-1") {
			t.Error("fresh = true for a redelivery of the same delivery id; want false")
		}
		// Redelivery is not once-only: GitHub allows repeated attempts inside
		// its 30-day window, and every one of them has to lose.
		if mark(t, store, "4242", "d-1") {
			t.Error("fresh = true on a third attempt; want false")
		}
	})

	t.Run("DifferentDeliveriesSameInstallation", func(t *testing.T) {
		// Negative space: dedup is per delivery, never per subject. An
		// installation's lifecycle is a sequence of distinct deliveries, and
		// every one of them takes effect.
		store, _ := mk(t)
		for _, deliveryID := range []string{"created-1", "suspend-2", "unsuspend-3"} {
			if !mark(t, store, "4242", deliveryID) {
				t.Errorf("delivery %q for an already-seen installation reported as a duplicate; want fresh", deliveryID)
			}
		}
	})

	t.Run("SameDeliveryDifferentInstallations", func(t *testing.T) {
		// The other half of the key. Delivery ids are GUIDs, so this pair
		// doesn't arise from GitHub — it pins that the installation is part of
		// the key rather than decoration on it, which is what lets a future
		// shared-App receiver key deliveries before it knows the tenant.
		store, _ := mk(t)
		if !mark(t, store, "4242", "d-1") {
			t.Fatal("first delivery reported as a duplicate; the test asserts nothing")
		}
		if !mark(t, store, "9999", "d-1") {
			t.Error("fresh = false for the same delivery id under another installation; want true")
		}
	})

	t.Run("DeliveryWithNoInstallation", func(t *testing.T) {
		// github_app_authorization and friends carry no installation object.
		// They key on the delivery id alone (installationID "") and dedup among
		// themselves exactly like any other delivery.
		store, _ := mk(t)
		if !mark(t, store, "", "d-1") {
			t.Fatal("first installation-less delivery reported as a duplicate; the test asserts nothing")
		}
		if mark(t, store, "", "d-1") {
			t.Error("fresh = true for a redelivered installation-less delivery; want false")
		}
		if !mark(t, store, "", "d-2") {
			t.Error("fresh = false for a second, distinct installation-less delivery; want true")
		}
	})

	t.Run("PrunesExpiredRows", func(t *testing.T) {
		// A row past the retention window is swept by the next insert. What
		// makes this observable is that its delivery becomes fresh again —
		// which is the honest consequence of a bounded table, and why the
		// window is far longer than the redelivery burst it absorbs.
		store, seed := mk(t)
		seed.Stale(t, "4242", "expired", db.GitHubDeliveryPruneAge+time.Hour)

		if !mark(t, store, "4242", "trigger-the-prune") {
			t.Fatal("the pruning insert reported as a duplicate; the test asserts nothing")
		}
		if seed.Recorded(t, "4242", "expired") {
			t.Error("a row older than the retention window survived the prune")
		}
	})

	t.Run("PruneSparesRowsInsideTheWindow", func(t *testing.T) {
		// The invariant the prune must not break: a delivery GitHub could still
		// be redelivering keeps deduping. A prune that swept the whole table
		// would pass the test above and destroy the guarantee.
		store, seed := mk(t)
		seed.Stale(t, "4242", "recent", db.GitHubDeliveryPruneAge-time.Hour)

		if !mark(t, store, "4242", "trigger-the-prune") {
			t.Fatal("the pruning insert reported as a duplicate; the test asserts nothing")
		}
		if !seed.Recorded(t, "4242", "recent") {
			t.Fatal("a row inside the retention window was pruned")
		}
		if mark(t, store, "4242", "recent") {
			t.Error("fresh = true for a delivery still inside the retention window; want false")
		}
	})
}
