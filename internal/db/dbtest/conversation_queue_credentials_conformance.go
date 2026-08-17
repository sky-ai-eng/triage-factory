package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ClaimCredentialsFactory is what a per-backend test file hands to
// RunClaimCredentialsConformance. Returns:
//   - the wired ConversationQueueStore impl,
//   - the orgID to pass to every call,
//   - a ClaimCredentialsSeeder the harness uses to stage fixture rows.
type ClaimCredentialsFactory func(t *testing.T) (store db.ConversationQueueStore, orgID string, seed ClaimCredentialsSeeder)

// ClaimCredentialsSeeder is a bag of callbacks the conformance suite uses
// to stage and observe states the store's own guarded flips can't produce
// on demand.
type ClaimCredentialsSeeder struct {
	// EnqueueConversation stages one claimable queued run (under a running
	// blueprint_run) and returns its id.
	EnqueueConversation func(t *testing.T) (conversationID string)

	// ConversationStatus reads the conversation's STORED conversations.status directly (SQL
	// NULL as ""), so the suite can assert what the column does and does not
	// carry: the phase park never touches the conversation row, and neither
	// a claim nor a requeue writes a status onto it — mid-flight is the
	// absence of an outcome, not a value.
	ConversationStatus func(t *testing.T, conversationID string) string

	// SetActivePhase rewrites the run's active claim's phase directly,
	// bypassing the store's guards — the harness needs states that other
	// subsystems produce in real flows (e.g. the cleared phase that
	// follows a delivered bundle) without depending on those subsystems
	// here. Empty phase writes NULL.
	SetActivePhase func(t *testing.T, conversationID, phase string)
}

// RunClaimCredentialsConformance covers the per-run credential-pubkey
// contract every backend impl must hold: MarkAwaitingCredentials parks the
// ACTIVE claim (phase='awaiting_credentials') with the sidecar pubkey in one
// write while the conversation row stays 'running', GetClaim / the sweep
// reads return the key, the phase-IS-NULL guard never overwrites a key
// already recorded, RequeueAwaitingCredentials only fires on a parked claim,
// and every path that releases a claim clears the key with the rest of the
// ownership stamp — a claimless row has no owner and no key, so the brain
// can never seal a fresh claim's bundle to a stale sidecar.
func RunClaimCredentialsConformance(t *testing.T, mk ClaimCredentialsFactory) {
	t.Helper()
	ctx := context.Background()

	const (
		executorID = "cred-conf-executor"
		bootEpoch  = int64(7)
		pubKey     = "dGVzdC1zaWRlY2FyLXB1YmtleQ=="
	)

	claim := func(t *testing.T, store db.ConversationQueueStore, conversationID string) *domain.Conversation {
		t.Helper()
		got, err := store.ClaimNextConversation(ctx, executorID, bootEpoch, db.ClaimPlacement{})
		if err != nil {
			t.Fatalf("ClaimNextConversation: %v", err)
		}
		if got == nil || got.ID != conversationID {
			t.Fatalf("ClaimNextConversation = %+v, want run %s", got, conversationID)
		}
		return got
	}

	t.Run("ClaimNextConversation_returns_the_minted_claim_id", func(t *testing.T) {
		// The executor stamps the engagement's measured sandbox cost by claim
		// id at teardown, after the claim is released — so the claim it just
		// minted has to come back with the run, per engagement. A requeue and
		// re-claim is a NEW engagement and must carry its own id, or the
		// second run's actuals would land on the first one's row.
		store, orgID, seed := mk(t)
		conversationID := seed.EnqueueConversation(t)

		first := claim(t, store, conversationID)
		if first.ClaimID == "" {
			t.Fatal("ClaimNextConversation returned no claim id; teardown has nothing to stamp")
		}
		if err := store.RequeueConversation(ctx, orgID, conversationID, "transient"); err != nil {
			t.Fatalf("RequeueConversation: %v", err)
		}
		second := claim(t, store, conversationID)
		if second.ClaimID == "" {
			t.Fatal("re-claim returned no claim id")
		}
		if second.ClaimID == first.ClaimID {
			t.Errorf("both engagements report claim %s; each claim is its own row", first.ClaimID)
		}
	})

	t.Run("Mark_parks_claim_records_pubkey_and_reads_return_it", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID := seed.EnqueueConversation(t)
		claim(t, store, conversationID)

		matched, err := store.MarkAwaitingCredentials(ctx, orgID, conversationID, pubKey)
		if err != nil || !matched {
			t.Fatalf("MarkAwaitingCredentials = (%v, %v), want (true, nil)", matched, err)
		}
		// The park is claim-side only: the conversation row is untouched,
		// which under the derived model means it carries no stored status
		// at all.
		if st := seed.ConversationStatus(t, conversationID); st != "" {
			t.Errorf("conversation status after park = %q, want no stored status", st)
		}

		got, ok, err := store.GetClaim(ctx, orgID, conversationID)
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
			if r.ConversationID == conversationID {
				found = true
				if r.CredPubKey != pubKey {
					t.Errorf("ListAwaitingCredentials CredPubKey = %q, want %q", r.CredPubKey, pubKey)
				}
			}
		}
		if !found {
			t.Errorf("run %s missing from ListAwaitingCredentials", conversationID)
		}
	})

	t.Run("Empty_pubkey_round_trips_as_empty", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID := seed.EnqueueConversation(t)
		claim(t, store, conversationID)

		matched, err := store.MarkAwaitingCredentials(ctx, orgID, conversationID, "")
		if err != nil || !matched {
			t.Fatalf("MarkAwaitingCredentials(\"\") = (%v, %v), want (true, nil)", matched, err)
		}
		got, ok, err := store.GetClaim(ctx, orgID, conversationID)
		if err != nil || !ok {
			t.Fatalf("GetClaim = (ok=%v, err=%v), want (true, nil)", ok, err)
		}
		if got.CredPubKey != "" {
			t.Errorf("GetClaim CredPubKey = %q, want empty", got.CredPubKey)
		}
	})

	t.Run("Mark_guard_never_overwrites_a_recorded_key", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID := seed.EnqueueConversation(t)
		claim(t, store, conversationID)

		if matched, err := store.MarkAwaitingCredentials(ctx, orgID, conversationID, pubKey); err != nil || !matched {
			t.Fatalf("first MarkAwaitingCredentials = (%v, %v), want (true, nil)", matched, err)
		}
		// The claim is now parked (phase non-NULL), so the guard must
		// reject a duplicate — and keep the key the brain may already be
		// sealing to.
		matched, err := store.MarkAwaitingCredentials(ctx, orgID, conversationID, "attacker-or-stale-key")
		if err != nil {
			t.Fatalf("second MarkAwaitingCredentials: %v", err)
		}
		if matched {
			t.Error("second MarkAwaitingCredentials matched, want guard rejection")
		}
		got, ok, err := store.GetClaim(ctx, orgID, conversationID)
		if err != nil || !ok {
			t.Fatalf("GetClaim = (ok=%v, err=%v), want (true, nil)", ok, err)
		}
		if got.CredPubKey != pubKey {
			t.Errorf("CredPubKey = %q after rejected duplicate, want the original %q", got.CredPubKey, pubKey)
		}
	})

	t.Run("RequeueAwaitingCredentials_requires_parked_claim", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID := seed.EnqueueConversation(t)
		claim(t, store, conversationID)

		// Not parked yet: the guard must refuse — a timeout racing a claim
		// that never parked (or already moved on) must not resurrect it.
		if matched, err := store.RequeueAwaitingCredentials(ctx, orgID, conversationID); err != nil || matched {
			t.Fatalf("RequeueAwaitingCredentials before park = (%v, %v), want (false, nil)", matched, err)
		}
		if st := seed.ConversationStatus(t, conversationID); st != "" {
			t.Errorf("status after refused requeue = %q, want no stored status", st)
		}

		if matched, err := store.MarkAwaitingCredentials(ctx, orgID, conversationID, pubKey); err != nil || !matched {
			t.Fatalf("MarkAwaitingCredentials = (%v, %v), want (true, nil)", matched, err)
		}
		matched, err := store.RequeueAwaitingCredentials(ctx, orgID, conversationID)
		if err != nil || !matched {
			t.Fatalf("RequeueAwaitingCredentials = (%v, %v), want (true, nil)", matched, err)
		}
		// Releasing the claim IS the requeue: the row stays mid-flight and
		// nothing writes a status.
		if st := seed.ConversationStatus(t, conversationID); st != "" {
			t.Errorf("status after requeue = %q, want no stored status", st)
		}
		got, ok, err := store.GetClaim(ctx, orgID, conversationID)
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
		if matched, err := store.RequeueAwaitingCredentials(ctx, orgID, conversationID); err != nil || matched {
			t.Errorf("duplicate RequeueAwaitingCredentials = (%v, %v), want (false, nil)", matched, err)
		}
	})

	t.Run("RequeueConversation_clears_key", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID := seed.EnqueueConversation(t)
		claim(t, store, conversationID)
		if matched, err := store.MarkAwaitingCredentials(ctx, orgID, conversationID, pubKey); err != nil || !matched {
			t.Fatalf("MarkAwaitingCredentials = (%v, %v), want (true, nil)", matched, err)
		}
		// Stage the post-bundle shape: the bundle arrived and the claim's
		// park cleared, key still recorded — then a transient dispatcher
		// failure requeues it.
		seed.SetActivePhase(t, conversationID, "")

		if err := store.RequeueConversation(ctx, orgID, conversationID, "transient"); err != nil {
			t.Fatalf("RequeueConversation: %v", err)
		}
		got, ok, err := store.GetClaim(ctx, orgID, conversationID)
		if err != nil || !ok {
			t.Fatalf("GetClaim = (ok=%v, err=%v), want (true, nil)", ok, err)
		}
		if got.CredPubKey != "" {
			t.Errorf("CredPubKey = %q after RequeueConversation, want empty", got.CredPubKey)
		}
		if got.ExecutorID != "" || got.BootEpoch != 0 {
			t.Errorf("ownership = (%q, %d) after RequeueConversation, want cleared", got.ExecutorID, got.BootEpoch)
		}
	})

	t.Run("RequeueConversation_from_parked_claim_requeues", func(t *testing.T) {
		// A sidecar bring-up that fails AFTER MarkAwaitingCredentials leaves
		// the active claim parked, and the setup-failure path requeues via
		// RequeueConversation (not the timeout-only RequeueAwaitingCredentials). The
		// run must reset and become claimable — anything else strands it
		// forever while the backstop sweep re-seals its bundle every tick.
		store, orgID, seed := mk(t)
		conversationID := seed.EnqueueConversation(t)
		claim(t, store, conversationID)
		if matched, err := store.MarkAwaitingCredentials(ctx, orgID, conversationID, pubKey); err != nil || !matched {
			t.Fatalf("MarkAwaitingCredentials = (%v, %v), want (true, nil)", matched, err)
		}

		// Requeue straight from the parked shape the failed bring-up leaves.
		if err := store.RequeueConversation(ctx, orgID, conversationID, "sidecar bring-up failed"); err != nil {
			t.Fatalf("RequeueConversation: %v", err)
		}
		got, ok, err := store.GetClaim(ctx, orgID, conversationID)
		if err != nil || !ok {
			t.Fatalf("GetClaim = (ok=%v, err=%v), want (true, nil)", ok, err)
		}
		if got.CredPubKey != "" {
			t.Errorf("CredPubKey = %q after RequeueConversation, want empty", got.CredPubKey)
		}
		if got.ExecutorID != "" || got.BootEpoch != 0 {
			t.Errorf("ownership = (%q, %d) after RequeueConversation, want cleared", got.ExecutorID, got.BootEpoch)
		}
		// The run is genuinely back on the queue: it re-claims. A no-op
		// requeue would have left it claimed-and-parked, and this would fail.
		claim(t, store, conversationID)
	})

	t.Run("ResetProcessingConversations_clears_key", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID := seed.EnqueueConversation(t)
		claim(t, store, conversationID)
		if matched, err := store.MarkAwaitingCredentials(ctx, orgID, conversationID, pubKey); err != nil || !matched {
			t.Fatalf("MarkAwaitingCredentials = (%v, %v), want (true, nil)", matched, err)
		}

		// A restart of the claiming executor (same id, later epoch) sweeps
		// its own parked orphan back to queued.
		n, err := store.ResetProcessingConversations(ctx, executorID, bootEpoch+1)
		if err != nil {
			t.Fatalf("ResetProcessingConversations: %v", err)
		}
		if n != 1 {
			t.Fatalf("ResetProcessingConversations reset %d rows, want 1", n)
		}
		got, ok, err := store.GetClaim(ctx, orgID, conversationID)
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
