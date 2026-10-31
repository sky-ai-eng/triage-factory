package postgres_test

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// seedPgActiveClaim mints an active claims row for the conversation — the
// engagement the bundle channel keys rows by.
func seedPgActiveClaim(t *testing.T, h *pgtest.Harness, orgID, conversationID, executorID string, bootEpoch int64) string {
	t.Helper()
	var id string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO claims (org_id, conversation_id, executor_id, boot_epoch, lease_expires_at)
		VALUES ($1, $2, $3, $4, now() + interval '300 seconds') RETURNING id::text
	`, orgID, conversationID, executorID, bootEpoch).Scan(&id); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	return id
}

// TestClaimCredentialsStore_Postgres_PutGet pins the basic round trip through
// the conversation's active claim, plus the no-active-claim no-op.
// Skips cleanly when Docker isn't available (pgtest.Shared).
func TestClaimCredentialsStore_Postgres_PutGet(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	conversationID := seedPgDeliverableConversation(t, h, orgID, teamID, userID)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	// No active claim yet: Put is a silent no-op and Get finds nothing —
	// there is no engagement to seal for.
	if err := stores.ClaimCredentials.Put(ctx, orgID, conversationID, "executor-0", 1, []byte("orphan"), nil); err != nil {
		t.Fatalf("Put with no active claim: %v", err)
	}
	if _, ok, err := stores.ClaimCredentials.Get(ctx, orgID, conversationID); err != nil || ok {
		t.Fatalf("Get with no active claim: ok=%v err=%v, want ok=false", ok, err)
	}

	seedPgActiveClaim(t, h, orgID, conversationID, "executor-1", 1)
	manifest := domain.SourceManifest{
		"github": {State: domain.SourceSealed},
		"linear": {State: domain.SourceUnresolved, Reason: domain.SourceReasonTokenEndpointUnavailable},
	}
	claimID := seedPgClaimID(t, h, conversationID)
	if err := stores.ClaimCredentials.Put(ctx, orgID, conversationID, "executor-1", 1, []byte("sealed-v1"), manifest); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok, err := stores.ClaimCredentials.Get(ctx, orgID, conversationID)
	if err != nil || !ok {
		t.Fatalf("Get after Put: ok=%v err=%v", ok, err)
	}
	if got.ExecutorID != "executor-1" || got.BootEpoch != 1 || string(got.Sealed) != "sealed-v1" {
		t.Fatalf("Get = (%q, %d, %q), want (executor-1, 1, sealed-v1)", got.ExecutorID, got.BootEpoch, got.Sealed)
	}
	if got.SealedAt.IsZero() {
		t.Error("Get returned a zero SealedAt; the workspace-add freshness gate has nothing to compare")
	}
	if !reflect.DeepEqual(got.Sources, manifest) {
		t.Errorf("Sources = %+v, want %+v", got.Sources, manifest)
	}

	// Releasing the claim takes the bundle out of reach (Get resolves the
	// ACTIVE claim only) — a re-claim is a NEW claim with its own seal.
	if _, err := h.AdminDB.Exec(`
		UPDATE claims SET released_at = now(), outcome = 'requeued' WHERE conversation_id = $1
	`, conversationID); err != nil {
		t.Fatalf("release claim: %v", err)
	}
	if _, ok, err := stores.ClaimCredentials.Get(ctx, orgID, conversationID); err != nil || ok {
		t.Fatalf("Get after release: ok=%v err=%v, want ok=false", ok, err)
	}

	// Teardown deletes the bundle; the manifest stays on the claim, which is
	// what the fleet console reads for a finished engagement.
	if _, err := h.AdminDB.Exec(`DELETE FROM claim_credentials WHERE claim_id = $1`, claimID); err != nil {
		t.Fatalf("delete bundle: %v", err)
	}
	claim, err := stores.ConversationQueue.ClaimByIDSystem(ctx, claimID)
	if err != nil || claim == nil {
		t.Fatalf("ClaimByIDSystem: %v, %v", claim, err)
	}
	if !reflect.DeepEqual(claim.Sources, manifest) {
		t.Errorf("claim Sources after teardown = %+v, want %+v", claim.Sources, manifest)
	}
}

// seedPgClaimID is the id of the conversation's active claim.
func seedPgClaimID(t *testing.T, h *pgtest.Harness, conversationID string) string {
	t.Helper()
	var id string
	if err := h.AdminDB.QueryRow(`
		SELECT id::text FROM claims WHERE conversation_id = $1 AND released_at IS NULL
	`, conversationID).Scan(&id); err != nil {
		t.Fatalf("read active claim: %v", err)
	}
	return id
}

// TestClaimCredentialsStore_Postgres_PutNeverRegressesBootEpoch pins the CAS
// guard: a write for an OLDER boot_epoch arriving after a NEWER one (the
// slow-provision-races-a-reclaim window) must be a silent no-op, never an
// overwrite. A same-epoch write (a refresh for the SAME still-live claim)
// must still apply normally.
func TestClaimCredentialsStore_Postgres_PutNeverRegressesBootEpoch(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	conversationID := seedPgDeliverableConversation(t, h, orgID, teamID, userID)
	seedPgActiveClaim(t, h, orgID, conversationID, "executor-b", 2)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	// Executor B (boot_epoch 2) reclaims and provisions first.
	forB := domain.SourceManifest{"github": {State: domain.SourceSealed}}
	if err := stores.ClaimCredentials.Put(ctx, orgID, conversationID, "executor-b", 2, []byte("sealed-for-b"), forB); err != nil {
		t.Fatalf("Put (epoch 2): %v", err)
	}

	// Executor A's stale, slower provision (boot_epoch 1) lands after. Its
	// manifest must not land either: the claim's manifest describes the
	// bundle the row still holds.
	forA := domain.SourceManifest{"github": {State: domain.SourceUnconfigured}}
	if err := stores.ClaimCredentials.Put(ctx, orgID, conversationID, "executor-a", 1, []byte("sealed-for-a"), forA); err != nil {
		t.Fatalf("Put (epoch 1, stale): %v", err)
	}

	got, ok, err := stores.ClaimCredentials.Get(ctx, orgID, conversationID)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.ExecutorID != "executor-b" || got.BootEpoch != 2 || string(got.Sealed) != "sealed-for-b" {
		t.Fatalf("stale write clobbered the fresher one: got (%q, %d, %q), want (executor-b, 2, sealed-for-b)",
			got.ExecutorID, got.BootEpoch, got.Sealed)
	}
	if !reflect.DeepEqual(got.Sources, forB) {
		t.Fatalf("stale write replaced the manifest: got %+v, want %+v", got.Sources, forB)
	}

	// A same-epoch refresh (re-minted tokens for the same still-live claim)
	// must still apply, manifest included.
	refreshedManifest := domain.SourceManifest{
		"github": {State: domain.SourceSealed},
		"linear": {State: domain.SourceSealed},
	}
	if err := stores.ClaimCredentials.Put(ctx, orgID, conversationID, "executor-b", 2, []byte("sealed-for-b-refreshed"), refreshedManifest); err != nil {
		t.Fatalf("Put (epoch 2, refresh): %v", err)
	}
	refreshed, ok, err := stores.ClaimCredentials.Get(ctx, orgID, conversationID)
	if err != nil || !ok {
		t.Fatalf("Get after refresh: ok=%v err=%v", ok, err)
	}
	if string(refreshed.Sealed) != "sealed-for-b-refreshed" {
		t.Fatalf("same-epoch refresh did not apply: got %q, want sealed-for-b-refreshed", refreshed.Sealed)
	}
	if !reflect.DeepEqual(refreshed.Sources, refreshedManifest) {
		t.Errorf("Sources after refresh = %+v, want %+v", refreshed.Sources, refreshedManifest)
	}
	// created_at is replaced on every re-seal, never left at the claim's
	// first provision — the whole basis for deciding freshness from the
	// timestamp instead of from the bundle's contents.
	if !refreshed.SealedAt.After(got.SealedAt) {
		t.Errorf("SealedAt did not advance on re-seal: %s then %s", got.SealedAt, refreshed.SealedAt)
	}
}

// TestListActiveNeedingCredentialRefresh_Postgres pins the refresh sweep's
// three arms: a bundle past the age cutoff, a manifest with an unresolved
// source, and a sealed credential expiring inside the horizon. A fresh bundle
// whose credentials outlive the horizon is left alone, and so is a claim
// parked awaiting its first bundle.
func TestListActiveNeedingCredentialRefresh_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	now := time.Now()
	soon, later := now.Add(3*time.Minute), now.Add(2*time.Hour)
	seal := func(m domain.SourceManifest) string {
		t.Helper()
		convID := seedPgDeliverableConversation(t, h, orgID, teamID, userID)
		seedPgActiveClaim(t, h, orgID, convID, "executor-1", 1)
		if err := stores.ClaimCredentials.Put(ctx, orgID, convID, "executor-1", 1, []byte("sealed"), m); err != nil {
			t.Fatalf("Put: %v", err)
		}
		return convID
	}
	fresh := seal(domain.SourceManifest{
		"github": {State: domain.SourceSealed, ExpiresAt: &later},
		"jira":   {State: domain.SourceDisabled},
	})
	unresolved := seal(domain.SourceManifest{
		"github": {State: domain.SourceSealed},
		"linear": {State: domain.SourceUnresolved, Reason: domain.SourceReasonTokenEndpointUnavailable},
	})
	expiring := seal(domain.SourceManifest{
		"github": {State: domain.SourceSealed},
		"linear": {State: domain.SourceSealed, ExpiresAt: &soon},
	})
	aged := seal(domain.SourceManifest{"github": {State: domain.SourceSealed}})
	if _, err := h.AdminDB.Exec(`
		UPDATE claim_credentials SET created_at = now() - interval '1 hour'
		WHERE claim_id = (SELECT id FROM claims WHERE conversation_id = $1 AND released_at IS NULL)
	`, aged); err != nil {
		t.Fatalf("age bundle: %v", err)
	}
	parked := seal(domain.SourceManifest{"linear": {State: domain.SourceUnresolved}})
	if _, err := h.AdminDB.Exec(`
		UPDATE claims SET phase = 'awaiting_credentials' WHERE conversation_id = $1 AND released_at IS NULL
	`, parked); err != nil {
		t.Fatalf("park claim: %v", err)
	}

	got, err := stores.ConversationQueue.ListActiveNeedingCredentialRefresh(ctx, now.Add(-30*time.Minute), now.Add(7*time.Minute))
	if err != nil {
		t.Fatalf("ListActiveNeedingCredentialRefresh: %v", err)
	}
	var ids []string
	for _, r := range got {
		ids = append(ids, r.ConversationID)
	}
	for _, want := range []string{unresolved, expiring, aged} {
		if !slices.Contains(ids, want) {
			t.Errorf("refresh list %v is missing %s", ids, want)
		}
	}
	for _, not := range []string{fresh, parked} {
		if slices.Contains(ids, not) {
			t.Errorf("refresh list %v includes %s, which needs no re-seal", ids, not)
		}
	}
}
