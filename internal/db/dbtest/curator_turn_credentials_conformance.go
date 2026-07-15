package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// CuratorTurnCredentialsSeeder is what a per-backend test file hands to
// RunCuratorTurnCredentialsConformance to stage fixtures the store's own
// guarded writes can't produce on demand.
type CuratorTurnCredentialsSeeder struct {
	// NewTurn stages one queued curator_requests row homed to HomeInstanceID
	// and returns its id (the dialect factory provisions the project/user/
	// instances rows its FKs and the awaiting-sweep join need).
	NewTurn func(t *testing.T) (requestID string)

	// ForceStatus rewrites the turn's status directly, bypassing the store's
	// guards — the harness needs terminal states the guarded flips can't reach
	// on demand.
	ForceStatus func(t *testing.T, requestID, status string)

	// HomeInstanceID is the executor the seeded turns are homed to, and
	// HomeBootEpoch the boot_epoch the dialect factory registered for it, so
	// the awaiting-sweep's instances join resolves.
	HomeInstanceID string
	HomeBootEpoch  int64
}

// CuratorTurnCredentialsFactory returns the two stores under test (the curator
// store for the pubkey-publish + awaiting-sweep half, the turn-credentials
// store for the sealed-bundle half), the orgID to pass, and a seeder.
type CuratorTurnCredentialsFactory func(t *testing.T) (
	curStore db.CuratorStore,
	credStore db.CuratorTurnCredentialsStore,
	orgID string,
	seed CuratorTurnCredentialsSeeder,
)

// RunCuratorTurnCredentialsConformance covers the per-turn credential contract
// every backend impl must hold: the sealed-bundle channel
// (Put/Get/Delete + the boot_epoch never-regress guard, mirroring
// run_credentials) and the pubkey-publish handshake
// (PublishTurnCredPubKey records once, never overwrites, empty round-trips,
// terminal turns reject; ListAwaitingCredentialTurnsSystem surfaces a keyed
// turn with no fresh bundle and excludes one that has been provisioned).
func RunCuratorTurnCredentialsConformance(t *testing.T, mk CuratorTurnCredentialsFactory) {
	t.Helper()
	ctx := context.Background()

	const pubKey = "dGVzdC1jdXJhdG9yLXNpZGVjYXItcHVia2V5"

	t.Run("CredStore_PutGetDelete", func(t *testing.T) {
		_, cred, orgID, seed := mk(t)
		reqID := seed.NewTurn(t)

		if _, _, _, ok, err := cred.Get(ctx, orgID, reqID); err != nil || ok {
			t.Fatalf("Get before any Put: ok=%v err=%v, want ok=false", ok, err)
		}
		if err := cred.Put(ctx, orgID, reqID, "executor-1", 1, []byte("sealed-v1")); err != nil {
			t.Fatalf("Put: %v", err)
		}
		executorID, bootEpoch, sealed, ok, err := cred.Get(ctx, orgID, reqID)
		if err != nil || !ok {
			t.Fatalf("Get after Put: ok=%v err=%v", ok, err)
		}
		if executorID != "executor-1" || bootEpoch != 1 || string(sealed) != "sealed-v1" {
			t.Fatalf("Get = (%q, %d, %q), want (executor-1, 1, sealed-v1)", executorID, bootEpoch, sealed)
		}
		if ok, err := cred.Delete(ctx, orgID, reqID); err != nil || !ok {
			t.Fatalf("Delete: ok=%v err=%v", ok, err)
		}
		if _, _, _, ok, err := cred.Get(ctx, orgID, reqID); err != nil || ok {
			t.Fatalf("Get after Delete: ok=%v err=%v, want ok=false", ok, err)
		}
	})

	t.Run("CredStore_PutNeverRegressesBootEpoch", func(t *testing.T) {
		_, cred, orgID, seed := mk(t)
		reqID := seed.NewTurn(t)

		// Executor B (boot_epoch 2) provisions first.
		if err := cred.Put(ctx, orgID, reqID, "executor-b", 2, []byte("sealed-for-b")); err != nil {
			t.Fatalf("Put (epoch 2): %v", err)
		}
		// Executor A's stale, slower provision (boot_epoch 1) lands after — a
		// silent no-op, never an overwrite.
		if err := cred.Put(ctx, orgID, reqID, "executor-a", 1, []byte("sealed-for-a")); err != nil {
			t.Fatalf("Put (epoch 1, stale): %v", err)
		}
		executorID, bootEpoch, sealed, ok, err := cred.Get(ctx, orgID, reqID)
		if err != nil || !ok {
			t.Fatalf("Get: ok=%v err=%v", ok, err)
		}
		if executorID != "executor-b" || bootEpoch != 2 || string(sealed) != "sealed-for-b" {
			t.Fatalf("stale write clobbered the fresher one: got (%q, %d, %q), want (executor-b, 2, sealed-for-b)", executorID, bootEpoch, sealed)
		}
		// A same-epoch refresh still applies.
		if err := cred.Put(ctx, orgID, reqID, "executor-b", 2, []byte("sealed-for-b-refreshed")); err != nil {
			t.Fatalf("Put (epoch 2, refresh): %v", err)
		}
		if _, _, sealed, _, _ := cred.Get(ctx, orgID, reqID); string(sealed) != "sealed-for-b-refreshed" {
			t.Fatalf("same-epoch refresh did not apply: got %q, want sealed-for-b-refreshed", sealed)
		}
	})

	t.Run("Publish_records_and_reads_back", func(t *testing.T) {
		cur, _, orgID, seed := mk(t)
		reqID := seed.NewTurn(t)

		matched, err := cur.PublishTurnCredPubKey(ctx, orgID, reqID, pubKey)
		if err != nil || !matched {
			t.Fatalf("PublishTurnCredPubKey = (%v, %v), want (true, nil)", matched, err)
		}
		info, ok, err := cur.GetTurnProvisionInfoSystem(ctx, orgID, reqID)
		if err != nil || !ok {
			t.Fatalf("GetTurnProvisionInfoSystem = (ok=%v, err=%v)", ok, err)
		}
		if info.CredPubKey != pubKey {
			t.Errorf("CredPubKey = %q, want %q", info.CredPubKey, pubKey)
		}
		if info.HomeInstanceID != seed.HomeInstanceID || info.Status != "queued" {
			t.Errorf("provision info = (home=%q, status=%q), want (%q, queued)", info.HomeInstanceID, info.Status, seed.HomeInstanceID)
		}
	})

	t.Run("Publish_guard_never_overwrites", func(t *testing.T) {
		cur, _, orgID, seed := mk(t)
		reqID := seed.NewTurn(t)

		if matched, err := cur.PublishTurnCredPubKey(ctx, orgID, reqID, pubKey); err != nil || !matched {
			t.Fatalf("first publish = (%v, %v), want (true, nil)", matched, err)
		}
		matched, err := cur.PublishTurnCredPubKey(ctx, orgID, reqID, "attacker-or-stale-key")
		if err != nil {
			t.Fatalf("second publish: %v", err)
		}
		if matched {
			t.Error("second publish matched, want guard rejection")
		}
		info, _, _ := cur.GetTurnProvisionInfoSystem(ctx, orgID, reqID)
		if info == nil || info.CredPubKey != pubKey {
			t.Errorf("CredPubKey after rejected duplicate = %v, want the original %q", info, pubKey)
		}
	})

	t.Run("Publish_empty_round_trips", func(t *testing.T) {
		cur, _, orgID, seed := mk(t)
		reqID := seed.NewTurn(t)

		matched, err := cur.PublishTurnCredPubKey(ctx, orgID, reqID, "")
		if err != nil || !matched {
			t.Fatalf("publish(\"\") = (%v, %v), want (true, nil)", matched, err)
		}
		info, _, _ := cur.GetTurnProvisionInfoSystem(ctx, orgID, reqID)
		if info == nil || info.CredPubKey != "" {
			t.Errorf("CredPubKey = %v, want empty", info)
		}
	})

	t.Run("Publish_terminal_turn_rejected", func(t *testing.T) {
		cur, _, orgID, seed := mk(t)
		reqID := seed.NewTurn(t)
		seed.ForceStatus(t, reqID, "done")

		matched, err := cur.PublishTurnCredPubKey(ctx, orgID, reqID, pubKey)
		if err != nil {
			t.Fatalf("publish on terminal turn: %v", err)
		}
		if matched {
			t.Error("publish matched on a terminal turn, want guard rejection")
		}
	})

	t.Run("ListAwaiting_surfaces_keyed_turn_and_excludes_provisioned", func(t *testing.T) {
		cur, cred, orgID, seed := mk(t)
		reqID := seed.NewTurn(t)

		// A keyless turn is never awaiting.
		if got := listAwaitingIDs(t, ctx, cur); containsID(got, reqID) {
			t.Fatalf("keyless turn %s already in awaiting set %v", reqID, got)
		}
		// Publish a key but no bundle yet → it must surface.
		if _, err := cur.PublishTurnCredPubKey(ctx, orgID, reqID, pubKey); err != nil {
			t.Fatalf("publish: %v", err)
		}
		if got := listAwaitingIDs(t, ctx, cur); !containsID(got, reqID) {
			t.Fatalf("keyed turn %s missing from awaiting set %v", reqID, got)
		}
		// Provision a fresh bundle at the home's current epoch → no longer awaiting.
		if err := cred.Put(ctx, orgID, reqID, seed.HomeInstanceID, seed.HomeBootEpoch, []byte("sealed")); err != nil {
			t.Fatalf("Put bundle: %v", err)
		}
		if got := listAwaitingIDs(t, ctx, cur); containsID(got, reqID) {
			t.Fatalf("provisioned turn %s still in awaiting set %v", reqID, got)
		}
		// A terminal turn never awaits, even keyed.
		seed.ForceStatus(t, reqID, "done")
		if got := listAwaitingIDs(t, ctx, cur); containsID(got, reqID) {
			t.Fatalf("terminal turn %s in awaiting set %v", reqID, got)
		}
	})
}

func listAwaitingIDs(t *testing.T, ctx context.Context, cur db.CuratorStore) []string {
	t.Helper()
	rows, err := cur.ListAwaitingCredentialTurnsSystem(ctx)
	if err != nil {
		t.Fatalf("ListAwaitingCredentialTurnsSystem: %v", err)
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

func containsID(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}
