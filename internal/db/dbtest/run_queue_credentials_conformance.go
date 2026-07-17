package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// RunQueueCredentialsFactory is what a per-backend test file hands to
// RunRunQueueCredentialsConformance. Returns:
//   - the wired RunQueueStore impl,
//   - the orgID to pass to every call,
//   - a RunQueueCredentialsSeeder the harness uses to stage fixture rows.
type RunQueueCredentialsFactory func(t *testing.T) (store db.RunQueueStore, orgID string, seed RunQueueCredentialsSeeder)

// RunQueueCredentialsSeeder is a bag of callbacks the conformance suite uses
// to stage run rows in states the store's own guarded flips can't produce
// on demand.
type RunQueueCredentialsSeeder struct {
	// EnqueueRun stages one claimable queued run (under a running
	// blueprint_run) and returns its id.
	EnqueueRun func(t *testing.T) (runID string)

	// ForceStatus rewrites the run's status directly, bypassing the
	// store's guards — the harness needs states that other subsystems
	// produce in real flows (e.g. the running state that follows a
	// delivered bundle) without depending on those subsystems here.
	ForceStatus func(t *testing.T, runID, status string)

	// SeedCredential writes a run_credentials bundle for runID (via the
	// RunCredentialsStore the dialect factory holds), and CredentialExists
	// reports whether one is present — so the suite can assert the
	// stale-bundle clear without RunQueueStore knowing about that table.
	SeedCredential   func(t *testing.T, runID string)
	CredentialExists func(t *testing.T, runID string) bool
}

// RunRunQueueCredentialsConformance covers the per-run credential-pubkey
// contract every backend impl must hold: MarkAwaitingCredentials records the
// sidecar pubkey alongside the status flip and GetClaim / the sweep reads
// return it, the running-only guard never overwrites a key already recorded,
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

	claim := func(t *testing.T, store db.RunQueueStore, runID string) {
		t.Helper()
		got, err := store.ClaimNextRun(ctx, executorID, bootEpoch, db.ClaimPlacement{})
		if err != nil {
			t.Fatalf("ClaimNextRun: %v", err)
		}
		if got == nil || got.ID != runID {
			t.Fatalf("ClaimNextRun = %+v, want run %s", got, runID)
		}
	}

	t.Run("Mark_records_pubkey_and_reads_return_it", func(t *testing.T) {
		store, orgID, seed := mk(t)
		runID := seed.EnqueueRun(t)
		claim(t, store, runID)

		matched, err := store.MarkAwaitingCredentials(ctx, orgID, runID, pubKey)
		if err != nil || !matched {
			t.Fatalf("MarkAwaitingCredentials = (%v, %v), want (true, nil)", matched, err)
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
		// The run is now 'awaiting_credentials', so the running-only guard
		// must reject a duplicate — and keep the key the brain may already
		// be sealing to.
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

	t.Run("RequeueAwaitingCredentials_clears_key_with_ownership", func(t *testing.T) {
		store, orgID, seed := mk(t)
		runID := seed.EnqueueRun(t)
		claim(t, store, runID)
		if matched, err := store.MarkAwaitingCredentials(ctx, orgID, runID, pubKey); err != nil || !matched {
			t.Fatalf("MarkAwaitingCredentials = (%v, %v), want (true, nil)", matched, err)
		}

		matched, err := store.RequeueAwaitingCredentials(ctx, orgID, runID)
		if err != nil || !matched {
			t.Fatalf("RequeueAwaitingCredentials = (%v, %v), want (true, nil)", matched, err)
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
	})

	t.Run("RequeueRun_clears_key", func(t *testing.T) {
		store, orgID, seed := mk(t)
		runID := seed.EnqueueRun(t)
		claim(t, store, runID)
		if matched, err := store.MarkAwaitingCredentials(ctx, orgID, runID, pubKey); err != nil || !matched {
			t.Fatalf("MarkAwaitingCredentials = (%v, %v), want (true, nil)", matched, err)
		}
		// Stage the post-bundle shape: the bundle arrived and the run went
		// back to running, key still recorded — then a transient dispatcher
		// failure requeues it.
		seed.ForceStatus(t, runID, "running")

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

	t.Run("MarkAwaitingCredentials_clears_stale_bundle", func(t *testing.T) {
		// A re-claim (or a different executor whose boot_epoch collides) mints a
		// NEW per-run sidecar key. A bundle sealed to the PRIOR key must not
		// survive into this attempt — the epoch check can't tell it apart, so
		// the executor would relay an un-unsealable box ("sealed to a different
		// key"). MarkAwaitingCredentials clears it as the recipient key rotates.
		store, orgID, seed := mk(t)
		if seed.SeedCredential == nil || seed.CredentialExists == nil {
			t.Skip("seeder does not support credential seeding")
		}
		runID := seed.EnqueueRun(t)
		claim(t, store, runID)
		seed.SeedCredential(t, runID)
		if !seed.CredentialExists(t, runID) {
			t.Fatal("precondition: seeded bundle should exist before MarkAwaitingCredentials")
		}
		if matched, err := store.MarkAwaitingCredentials(ctx, orgID, runID, pubKey); err != nil || !matched {
			t.Fatalf("MarkAwaitingCredentials = (%v, %v), want (true, nil)", matched, err)
		}
		if seed.CredentialExists(t, runID) {
			t.Error("stale run_credentials bundle survived MarkAwaitingCredentials; want it cleared as the key rotated")
		}
	})

	t.Run("RequeueRun_from_awaiting_credentials_requeues", func(t *testing.T) {
		// The stranding regression: a sidecar bring-up that fails AFTER
		// MarkAwaitingCredentials leaves the run in 'awaiting_credentials', and
		// the setup-failure path requeues it via RequeueRun (not the unused
		// RequeueAwaitingCredentials). A 'running'-only guard silently no-ops
		// here, stranding the run forever while the backstop sweep re-seals its
		// bundle every tick. RequeueRun must reset it and make it claimable.
		store, orgID, seed := mk(t)
		runID := seed.EnqueueRun(t)
		claim(t, store, runID)
		if matched, err := store.MarkAwaitingCredentials(ctx, orgID, runID, pubKey); err != nil || !matched {
			t.Fatalf("MarkAwaitingCredentials = (%v, %v), want (true, nil)", matched, err)
		}

		// No ForceStatus back to 'running' — requeue straight from
		// 'awaiting_credentials', the exact state the failed bring-up leaves.
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
		// The run is genuinely back on the queue: it re-claims. A no-op requeue
		// would have left it in 'awaiting_credentials', and this would fail.
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
