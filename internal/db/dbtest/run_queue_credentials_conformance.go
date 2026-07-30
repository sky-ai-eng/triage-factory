package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// RunQueueCredentialsFactory is what a per-backend test file hands to
// RunRunQueueCredentialsConformance. Returns:
//   - the wired RunQueueStore impl,
//   - the orgID to pass to every call,
//   - a RunQueueCredentialsSeeder the harness uses to stage fixture rows.
type RunQueueCredentialsFactory func(t *testing.T) (store db.RunQueueStore, orgID string, seed RunQueueCredentialsSeeder)

// RunQueueCredentialsSeeder is a bag of callbacks the conformance suite uses
// to stage and observe states the store's own guarded flips can't produce
// on demand.
type RunQueueCredentialsSeeder struct {
	// EnqueueRun stages one claimable queued run (under a running
	// blueprint_run) and returns its id.
	EnqueueRun func(t *testing.T) (runID string)

	// RunStatus reads the run's stored conversations.status directly, so
	// the suite can assert the phase park never touches the conversation
	// row.
	RunStatus func(t *testing.T, runID string) string

	// SetActivePhase rewrites the run's active claim's phase directly,
	// bypassing the store's guards — the harness needs states that other
	// subsystems produce in real flows (e.g. the cleared phase that
	// follows a delivered bundle) without depending on those subsystems
	// here. Empty phase writes NULL.
	SetActivePhase func(t *testing.T, runID, phase string)
}

// RunRunQueueCredentialsConformance covers the per-run credential-pubkey
// contract every backend impl must hold: MarkAwaitingCredentials parks the
// ACTIVE claim (phase='awaiting_credentials') with the sidecar pubkey in one
// write while the conversation row stays 'running', GetClaim / the sweep
// reads return the key, the phase-IS-NULL guard never overwrites a key
// already recorded, RequeueAwaitingCredentials only fires on a parked claim,
// and every path that releases a claim back to 'queued' clears the key with
// the rest of the ownership stamp — a queued row has no owner and no key, so
// the brain can never seal a fresh claim's bundle to a stale sidecar.
func RunRunQueueCredentialsConformance(t *testing.T, mk RunQueueCredentialsFactory) {
	t.Helper()
	ctx := context.Background()

	const (
		executorID = "cred-conf-executor"
		bootEpoch  = int64(7)
		pubKey     = "dGVzdC1zaWRlY2FyLXB1YmtleQ=="
	)

	claim := func(t *testing.T, store db.RunQueueStore, runID string) *domain.Conversation {
		t.Helper()
		got, err := store.ClaimNextRun(ctx, executorID, bootEpoch, db.ClaimPlacement{})
		if err != nil {
			t.Fatalf("ClaimNextRun: %v", err)
		}
		if got == nil || got.ID != runID {
			t.Fatalf("ClaimNextRun = %+v, want run %s", got, runID)
		}
		return got
	}

	t.Run("ClaimNextRun_returns_the_minted_claim_id", func(t *testing.T) {
		// The executor stamps the engagement's measured sandbox cost by claim
		// id at teardown, after the claim is released — so the claim it just
		// minted has to come back with the run, per engagement. A requeue and
		// re-claim is a NEW engagement and must carry its own id, or the
		// second run's actuals would land on the first one's row.
		store, orgID, seed := mk(t)
		runID := seed.EnqueueRun(t)

		first := claim(t, store, runID)
		if first.ClaimID == "" {
			t.Fatal("ClaimNextRun returned no claim id; teardown has nothing to stamp")
		}
		if err := store.RequeueRun(ctx, orgID, runID, "transient"); err != nil {
			t.Fatalf("RequeueRun: %v", err)
		}
		second := claim(t, store, runID)
		if second.ClaimID == "" {
			t.Fatal("re-claim returned no claim id")
		}
		if second.ClaimID == first.ClaimID {
			t.Errorf("both engagements report claim %s; each claim is its own row", first.ClaimID)
		}
	})

	t.Run("Mark_parks_claim_records_pubkey_and_reads_return_it", func(t *testing.T) {
		store, orgID, seed := mk(t)
		runID := seed.EnqueueRun(t)
		claim(t, store, runID)

		matched, err := store.MarkAwaitingCredentials(ctx, orgID, runID, pubKey)
		if err != nil || !matched {
			t.Fatalf("MarkAwaitingCredentials = (%v, %v), want (true, nil)", matched, err)
		}
		// The park is claim-side only: the conversation row keeps its
		// claimed status.
		if st := seed.RunStatus(t, runID); st != "running" {
			t.Errorf("conversation status after park = %q, want running", st)
		}

		got, ok, err := store.GetClaim(ctx, orgID, runID)
		if err != nil || !ok {
			t.Fatalf("GetClaim = (ok=%v, err=%v), want (true, nil)", ok, err)
		}
		if got.CredPubKey != pubKey {
			t.Errorf("GetClaim CredPubKey = %q, want %q", got.CredPubKey, pubKey)
		}
		if got.ExecutorID != executorID || got.BootEpoch != bootEpoch {
			t.Errorf("GetClaim ownership = (%q, %d), want (%q, %d)", got.ExecutorID, got.BootEpoch, executorID, bootEpoch)
		}

		// The backstop-sweep read must carry the key too — the brain seals
		// off whichever read reached the run first.
		listed, err := store.ListAwaitingCredentials(ctx)
		if err != nil {
			t.Fatalf("ListAwaitingCredentials: %v", err)
		}
		found := false
		for _, r := range listed {
			if r.RunID == runID {
				found = true
				if r.CredPubKey != pubKey {
					t.Errorf("ListAwaitingCredentials CredPubKey = %q, want %q", r.CredPubKey, pubKey)
				}
			}
		}
		if !found {
			t.Errorf("run %s missing from ListAwaitingCredentials", runID)
		}
	})

	t.Run("Empty_pubkey_round_trips_as_empty", func(t *testing.T) {
		store, orgID, seed := mk(t)
		runID := seed.EnqueueRun(t)
		claim(t, store, runID)

		matched, err := store.MarkAwaitingCredentials(ctx, orgID, runID, "")
		if err != nil || !matched {
			t.Fatalf("MarkAwaitingCredentials(\"\") = (%v, %v), want (true, nil)", matched, err)
		}
		got, ok, err := store.GetClaim(ctx, orgID, runID)
		if err != nil || !ok {
			t.Fatalf("GetClaim = (ok=%v, err=%v), want (true, nil)", ok, err)
		}
		if got.CredPubKey != "" {
			t.Errorf("GetClaim CredPubKey = %q, want empty", got.CredPubKey)
		}
	})

	t.Run("Mark_guard_never_overwrites_a_recorded_key", func(t *testing.T) {
		store, orgID, seed := mk(t)
		runID := seed.EnqueueRun(t)
		claim(t, store, runID)

		if matched, err := store.MarkAwaitingCredentials(ctx, orgID, runID, pubKey); err != nil || !matched {
			t.Fatalf("first MarkAwaitingCredentials = (%v, %v), want (true, nil)", matched, err)
		}
		// The claim is now parked (phase non-NULL), so the guard must
		// reject a duplicate — and keep the key the brain may already be
		// sealing to.
		matched, err := store.MarkAwaitingCredentials(ctx, orgID, runID, "attacker-or-stale-key")
		if err != nil {
			t.Fatalf("second MarkAwaitingCredentials: %v", err)
		}
		if matched {
			t.Error("second MarkAwaitingCredentials matched, want guard rejection")
		}
		got, ok, err := store.GetClaim(ctx, orgID, runID)
		if err != nil || !ok {
			t.Fatalf("GetClaim = (ok=%v, err=%v), want (true, nil)", ok, err)
		}
		if got.CredPubKey != pubKey {
			t.Errorf("CredPubKey = %q after rejected duplicate, want the original %q", got.CredPubKey, pubKey)
		}
	})

	t.Run("RequeueAwaitingCredentials_requires_parked_claim", func(t *testing.T) {
		store, orgID, seed := mk(t)
		runID := seed.EnqueueRun(t)
		claim(t, store, runID)

		// Not parked yet: the guard must refuse — a timeout racing a claim
		// that never parked (or already moved on) must not resurrect it.
		if matched, err := store.RequeueAwaitingCredentials(ctx, orgID, runID); err != nil || matched {
			t.Fatalf("RequeueAwaitingCredentials before park = (%v, %v), want (false, nil)", matched, err)
		}
		if st := seed.RunStatus(t, runID); st != "running" {
			t.Errorf("status after refused requeue = %q, want running", st)
		}

		if matched, err := store.MarkAwaitingCredentials(ctx, orgID, runID, pubKey); err != nil || !matched {
			t.Fatalf("MarkAwaitingCredentials = (%v, %v), want (true, nil)", matched, err)
		}
		matched, err := store.RequeueAwaitingCredentials(ctx, orgID, runID)
		if err != nil || !matched {
			t.Fatalf("RequeueAwaitingCredentials = (%v, %v), want (true, nil)", matched, err)
		}
		if st := seed.RunStatus(t, runID); st != "queued" {
			t.Errorf("status after requeue = %q, want queued", st)
		}
		got, ok, err := store.GetClaim(ctx, orgID, runID)
		if err != nil || !ok {
			t.Fatalf("GetClaim = (ok=%v, err=%v), want (true, nil)", ok, err)
		}
		if got.CredPubKey != "" {
			t.Errorf("CredPubKey = %q after requeue, want empty", got.CredPubKey)
		}
		if got.ExecutorID != "" || got.BootEpoch != 0 {
			t.Errorf("ownership = (%q, %d) after requeue, want cleared", got.ExecutorID, got.BootEpoch)
		}
		// A duplicate timeout after the release finds no parked claim.
		if matched, err := store.RequeueAwaitingCredentials(ctx, orgID, runID); err != nil || matched {
			t.Errorf("duplicate RequeueAwaitingCredentials = (%v, %v), want (false, nil)", matched, err)
		}
	})

	t.Run("RequeueRun_clears_key", func(t *testing.T) {
		store, orgID, seed := mk(t)
		runID := seed.EnqueueRun(t)
		claim(t, store, runID)
		if matched, err := store.MarkAwaitingCredentials(ctx, orgID, runID, pubKey); err != nil || !matched {
			t.Fatalf("MarkAwaitingCredentials = (%v, %v), want (true, nil)", matched, err)
		}
		// Stage the post-bundle shape: the bundle arrived and the claim's
		// park cleared, key still recorded — then a transient dispatcher
		// failure requeues it.
		seed.SetActivePhase(t, runID, "")

		if err := store.RequeueRun(ctx, orgID, runID, "transient"); err != nil {
			t.Fatalf("RequeueRun: %v", err)
		}
		got, ok, err := store.GetClaim(ctx, orgID, runID)
		if err != nil || !ok {
			t.Fatalf("GetClaim = (ok=%v, err=%v), want (true, nil)", ok, err)
		}
		if got.CredPubKey != "" {
			t.Errorf("CredPubKey = %q after RequeueRun, want empty", got.CredPubKey)
		}
		if got.ExecutorID != "" || got.BootEpoch != 0 {
			t.Errorf("ownership = (%q, %d) after RequeueRun, want cleared", got.ExecutorID, got.BootEpoch)
		}
	})

	t.Run("RequeueRun_from_parked_claim_requeues", func(t *testing.T) {
		// A sidecar bring-up that fails AFTER MarkAwaitingCredentials leaves
		// the active claim parked, and the setup-failure path requeues via
		// RequeueRun (not the timeout-only RequeueAwaitingCredentials). The
		// run must reset and become claimable — anything else strands it
		// forever while the backstop sweep re-seals its bundle every tick.
		store, orgID, seed := mk(t)
		runID := seed.EnqueueRun(t)
		claim(t, store, runID)
		if matched, err := store.MarkAwaitingCredentials(ctx, orgID, runID, pubKey); err != nil || !matched {
			t.Fatalf("MarkAwaitingCredentials = (%v, %v), want (true, nil)", matched, err)
		}

		// Requeue straight from the parked shape the failed bring-up leaves.
		if err := store.RequeueRun(ctx, orgID, runID, "sidecar bring-up failed"); err != nil {
			t.Fatalf("RequeueRun: %v", err)
		}
		got, ok, err := store.GetClaim(ctx, orgID, runID)
		if err != nil || !ok {
			t.Fatalf("GetClaim = (ok=%v, err=%v), want (true, nil)", ok, err)
		}
		if got.CredPubKey != "" {
			t.Errorf("CredPubKey = %q after RequeueRun, want empty", got.CredPubKey)
		}
		if got.ExecutorID != "" || got.BootEpoch != 0 {
			t.Errorf("ownership = (%q, %d) after RequeueRun, want cleared", got.ExecutorID, got.BootEpoch)
		}
		// The run is genuinely back on the queue: it re-claims. A no-op
		// requeue would have left it claimed-and-parked, and this would fail.
		claim(t, store, runID)
	})

	t.Run("ResetProcessingRuns_clears_key", func(t *testing.T) {
		store, orgID, seed := mk(t)
		runID := seed.EnqueueRun(t)
		claim(t, store, runID)
		if matched, err := store.MarkAwaitingCredentials(ctx, orgID, runID, pubKey); err != nil || !matched {
			t.Fatalf("MarkAwaitingCredentials = (%v, %v), want (true, nil)", matched, err)
		}

		// A restart of the claiming executor (same id, later epoch) sweeps
		// its own parked orphan back to queued.
		n, err := store.ResetProcessingRuns(ctx, executorID, bootEpoch+1)
		if err != nil {
			t.Fatalf("ResetProcessingRuns: %v", err)
		}
		if n != 1 {
			t.Fatalf("ResetProcessingRuns reset %d rows, want 1", n)
		}
		got, ok, err := store.GetClaim(ctx, orgID, runID)
		if err != nil || !ok {
			t.Fatalf("GetClaim = (ok=%v, err=%v), want (true, nil)", ok, err)
		}
		if got.CredPubKey != "" {
			t.Errorf("CredPubKey = %q after reset, want empty", got.CredPubKey)
		}
		if got.ExecutorID != "" || got.BootEpoch != 0 {
			t.Errorf("ownership = (%q, %d) after reset, want cleared", got.ExecutorID, got.BootEpoch)
		}
	})
}
