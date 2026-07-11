package postgres_test

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestRunCredentialsStore_Postgres_PutGetDelete pins the basic round trip.
// Skips cleanly when Docker isn't available (pgtest.Shared).
func TestRunCredentialsStore_Postgres_PutGetDelete(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	runID := seedPgArtifactRun(t, h, orgID, teamID, userID)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	if _, _, _, ok, err := stores.RunCredentials.Get(ctx, orgID, runID); err != nil || ok {
		t.Fatalf("Get before any Put: ok=%v err=%v, want ok=false", ok, err)
	}

	if err := stores.RunCredentials.Put(ctx, orgID, runID, "executor-1", 1, []byte("sealed-v1")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	executorID, bootEpoch, sealed, ok, err := stores.RunCredentials.Get(ctx, orgID, runID)
	if err != nil || !ok {
		t.Fatalf("Get after Put: ok=%v err=%v", ok, err)
	}
	if executorID != "executor-1" || bootEpoch != 1 || string(sealed) != "sealed-v1" {
		t.Fatalf("Get = (%q, %d, %q), want (executor-1, 1, sealed-v1)", executorID, bootEpoch, sealed)
	}

	if ok, err := stores.RunCredentials.Delete(ctx, orgID, runID); err != nil || !ok {
		t.Fatalf("Delete: ok=%v err=%v", ok, err)
	}
	if _, _, _, ok, err := stores.RunCredentials.Get(ctx, orgID, runID); err != nil || ok {
		t.Fatalf("Get after Delete: ok=%v err=%v, want ok=false", ok, err)
	}
}

// TestRunCredentialsStore_Postgres_PutNeverRegressesBootEpoch pins the CAS
// guard: a write for an OLDER boot_epoch arriving after a NEWER one (the
// slow-provision-races-a-reclaim window) must be a silent no-op, never an
// overwrite. A same-epoch write (a refresh for the SAME still-live claim)
// must still apply normally.
func TestRunCredentialsStore_Postgres_PutNeverRegressesBootEpoch(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	runID := seedPgArtifactRun(t, h, orgID, teamID, userID)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	// Executor B (boot_epoch 2) reclaims and provisions first.
	if err := stores.RunCredentials.Put(ctx, orgID, runID, "executor-b", 2, []byte("sealed-for-b")); err != nil {
		t.Fatalf("Put (epoch 2): %v", err)
	}

	// Executor A's stale, slower provision (boot_epoch 1) lands after.
	if err := stores.RunCredentials.Put(ctx, orgID, runID, "executor-a", 1, []byte("sealed-for-a")); err != nil {
		t.Fatalf("Put (epoch 1, stale): %v", err)
	}

	executorID, bootEpoch, sealed, ok, err := stores.RunCredentials.Get(ctx, orgID, runID)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if executorID != "executor-b" || bootEpoch != 2 || string(sealed) != "sealed-for-b" {
		t.Fatalf("stale write clobbered the fresher one: got (%q, %d, %q), want (executor-b, 2, sealed-for-b)",
			executorID, bootEpoch, sealed)
	}

	// A same-epoch refresh (re-minted tokens for the same still-live claim)
	// must still apply.
	if err := stores.RunCredentials.Put(ctx, orgID, runID, "executor-b", 2, []byte("sealed-for-b-refreshed")); err != nil {
		t.Fatalf("Put (epoch 2, refresh): %v", err)
	}
	_, _, sealed, ok, err = stores.RunCredentials.Get(ctx, orgID, runID)
	if err != nil || !ok {
		t.Fatalf("Get after refresh: ok=%v err=%v", ok, err)
	}
	if string(sealed) != "sealed-for-b-refreshed" {
		t.Fatalf("same-epoch refresh did not apply: got %q, want sealed-for-b-refreshed", sealed)
	}
}
