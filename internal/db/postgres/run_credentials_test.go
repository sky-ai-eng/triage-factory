package postgres_test

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// seedPgActiveClaim mints an active claims row for the run — the engagement
// the bundle channel keys rows by.
func seedPgActiveClaim(t *testing.T, h *pgtest.Harness, orgID, runID, executorID string, bootEpoch int64) string {
	t.Helper()
	var id string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO claims (org_id, conversation_id, executor_id, boot_epoch)
		VALUES ($1, $2, $3, $4) RETURNING id::text
	`, orgID, runID, executorID, bootEpoch).Scan(&id); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	return id
}

// TestRunCredentialsStore_Postgres_PutGet pins the basic round trip through
// the run's active claim, plus the no-active-claim no-op.
// Skips cleanly when Docker isn't available (pgtest.Shared).
func TestRunCredentialsStore_Postgres_PutGet(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	runID := seedPgArtifactRun(t, h, orgID, teamID, userID)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	// No active claim yet: Put is a silent no-op and Get finds nothing —
	// there is no engagement to seal for.
	if err := stores.RunCredentials.Put(ctx, orgID, runID, "executor-0", 1, []byte("orphan")); err != nil {
		t.Fatalf("Put with no active claim: %v", err)
	}
	if _, ok, err := stores.RunCredentials.Get(ctx, orgID, runID); err != nil || ok {
		t.Fatalf("Get with no active claim: ok=%v err=%v, want ok=false", ok, err)
	}

	seedPgActiveClaim(t, h, orgID, runID, "executor-1", 1)
	if err := stores.RunCredentials.Put(ctx, orgID, runID, "executor-1", 1, []byte("sealed-v1")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok, err := stores.RunCredentials.Get(ctx, orgID, runID)
	if err != nil || !ok {
		t.Fatalf("Get after Put: ok=%v err=%v", ok, err)
	}
	if got.ExecutorID != "executor-1" || got.BootEpoch != 1 || string(got.Sealed) != "sealed-v1" {
		t.Fatalf("Get = (%q, %d, %q), want (executor-1, 1, sealed-v1)", got.ExecutorID, got.BootEpoch, got.Sealed)
	}
	if got.SealedAt.IsZero() {
		t.Error("Get returned a zero SealedAt; the workspace-add freshness gate has nothing to compare")
	}

	// Releasing the claim takes the bundle out of reach (Get resolves the
	// ACTIVE claim only) — a re-claim is a NEW claim with its own seal.
	if _, err := h.AdminDB.Exec(`
		UPDATE claims SET released_at = now(), outcome = 'requeued' WHERE conversation_id = $1
	`, runID); err != nil {
		t.Fatalf("release claim: %v", err)
	}
	if _, ok, err := stores.RunCredentials.Get(ctx, orgID, runID); err != nil || ok {
		t.Fatalf("Get after release: ok=%v err=%v, want ok=false", ok, err)
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
	seedPgActiveClaim(t, h, orgID, runID, "executor-b", 2)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	// Executor B (boot_epoch 2) reclaims and provisions first.
	if err := stores.RunCredentials.Put(ctx, orgID, runID, "executor-b", 2, []byte("sealed-for-b")); err != nil {
		t.Fatalf("Put (epoch 2): %v", err)
	}

	// Executor A's stale, slower provision (boot_epoch 1) lands after.
	if err := stores.RunCredentials.Put(ctx, orgID, runID, "executor-a", 1, []byte("sealed-for-a")); err != nil {
		t.Fatalf("Put (epoch 1, stale): %v", err)
	}

	got, ok, err := stores.RunCredentials.Get(ctx, orgID, runID)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.ExecutorID != "executor-b" || got.BootEpoch != 2 || string(got.Sealed) != "sealed-for-b" {
		t.Fatalf("stale write clobbered the fresher one: got (%q, %d, %q), want (executor-b, 2, sealed-for-b)",
			got.ExecutorID, got.BootEpoch, got.Sealed)
	}

	// A same-epoch refresh (re-minted tokens for the same still-live claim)
	// must still apply.
	if err := stores.RunCredentials.Put(ctx, orgID, runID, "executor-b", 2, []byte("sealed-for-b-refreshed")); err != nil {
		t.Fatalf("Put (epoch 2, refresh): %v", err)
	}
	refreshed, ok, err := stores.RunCredentials.Get(ctx, orgID, runID)
	if err != nil || !ok {
		t.Fatalf("Get after refresh: ok=%v err=%v", ok, err)
	}
	if string(refreshed.Sealed) != "sealed-for-b-refreshed" {
		t.Fatalf("same-epoch refresh did not apply: got %q, want sealed-for-b-refreshed", refreshed.Sealed)
	}
	// created_at is replaced on every re-seal, never left at the claim's
	// first provision — the whole basis for deciding freshness from the
	// timestamp instead of from the bundle's contents.
	if !refreshed.SealedAt.After(got.SealedAt) {
		t.Errorf("SealedAt did not advance on re-seal: %s then %s", got.SealedAt, refreshed.SealedAt)
	}
}
