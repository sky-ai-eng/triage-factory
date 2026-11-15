package postgres_test

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

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

// TestClaimCredentialsStore_Postgres_PutGet pins the basic round trip: Put
// names the claim, Get reads the conversation's active one. A claim id no row
// carries is a no-op.
// Skips cleanly when Docker isn't available (pgtest.Shared).
func TestClaimCredentialsStore_Postgres_PutGet(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	conversationID := seedPgDeliverableConversation(t, h, orgID, teamID, userID)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	// No such claim: Put is a silent no-op and Get finds nothing — there is
	// no engagement to seal for.
	if err := stores.ClaimCredentials.Put(ctx, orgID, uuid.New().String(), "executor-0", 1, []byte("orphan"), nil); err != nil {
		t.Fatalf("Put with no such claim: %v", err)
	}
	if _, ok, err := stores.ClaimCredentials.Get(ctx, orgID, conversationID); err != nil || ok {
		t.Fatalf("Get with no active claim: ok=%v err=%v, want ok=false", ok, err)
	}

	claimID := seedPgActiveClaim(t, h, orgID, conversationID, "executor-1", 1)
	manifest := domain.SourceManifest{
		"github": {State: domain.SourceSealed},
		"linear": {State: domain.SourceUnresolved, Reason: domain.SourceReasonTokenEndpointUnavailable},
	}
	if err := stores.ClaimCredentials.Put(ctx, orgID, claimID, "executor-1", 1, []byte("sealed-v1"), manifest); err != nil {
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

// TestClaimCredentialsStore_Postgres_PutLandsOnlyOnTheClaimItNames pins what
// keying the write by claim closes: a seal made for a claim released while the
// brain was sealing must not land under the conversation's next claim, whose
// sidecar published a different key, however the two executors' boot epochs
// compare. A seal naming the right claim with an identity it does not carry
// lands nowhere either. A re-seal of the same claim replaces its bundle and
// its manifest.
func TestClaimCredentialsStore_Postgres_PutLandsOnlyOnTheClaimItNames(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	conversationID := seedPgDeliverableConversation(t, h, orgID, teamID, userID)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	// Executor A's claim is handed back mid-provision and executor B claims
	// the conversation. A's epoch is the larger: the two are separate
	// executors' counters, and neither says anything about the other's claim.
	claimA := seedPgActiveClaim(t, h, orgID, conversationID, "executor-a", 7)
	if _, err := h.AdminDB.Exec(`UPDATE claims SET released_at = now(), outcome = 'requeued' WHERE id = $1`, claimA); err != nil {
		t.Fatalf("release claim A: %v", err)
	}
	claimB := seedPgActiveClaim(t, h, orgID, conversationID, "executor-b", 2)

	forA := domain.SourceManifest{"github": {State: domain.SourceUnconfigured}}
	if err := stores.ClaimCredentials.Put(ctx, orgID, claimA, "executor-a", 7, []byte("sealed-for-a"), forA); err != nil {
		t.Fatalf("Put (claim A, released): %v", err)
	}
	if err := stores.ClaimCredentials.Put(ctx, orgID, claimB, "executor-a", 7, []byte("sealed-for-a"), forA); err != nil {
		t.Fatalf("Put (claim B, A's identity): %v", err)
	}
	var bundles int
	if err := h.AdminDB.QueryRow(`
		SELECT count(*) FROM claim_credentials WHERE claim_id IN ($1::uuid, $2::uuid)
	`, claimA, claimB).Scan(&bundles); err != nil {
		t.Fatalf("count bundles: %v", err)
	}
	if bundles != 0 {
		t.Fatalf("%d bundles landed from seals made for claim A; want none, on either claim", bundles)
	}
	var manifests int
	if err := h.AdminDB.QueryRow(`
		SELECT count(*) FROM claims WHERE id IN ($1::uuid, $2::uuid) AND source_manifest IS NOT NULL
	`, claimA, claimB).Scan(&manifests); err != nil {
		t.Fatalf("count manifests: %v", err)
	}
	if manifests != 0 {
		t.Fatalf("%d claims carry a manifest from a seal that landed nowhere", manifests)
	}

	forB := domain.SourceManifest{"github": {State: domain.SourceSealed}}
	if err := stores.ClaimCredentials.Put(ctx, orgID, claimB, "executor-b", 2, []byte("sealed-for-b"), forB); err != nil {
		t.Fatalf("Put (claim B): %v", err)
	}
	got, ok, err := stores.ClaimCredentials.Get(ctx, orgID, conversationID)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.ExecutorID != "executor-b" || got.BootEpoch != 2 || string(got.Sealed) != "sealed-for-b" {
		t.Fatalf("Get = (%q, %d, %q), want (executor-b, 2, sealed-for-b)", got.ExecutorID, got.BootEpoch, got.Sealed)
	}
	if !reflect.DeepEqual(got.Sources, forB) {
		t.Fatalf("Sources = %+v, want %+v", got.Sources, forB)
	}

	// A re-seal of the same claim (re-minted tokens for a long-lived one)
	// replaces the bundle and the manifest.
	refreshedManifest := domain.SourceManifest{
		"github": {State: domain.SourceSealed},
		"linear": {State: domain.SourceSealed},
	}
	if err := stores.ClaimCredentials.Put(ctx, orgID, claimB, "executor-b", 2, []byte("sealed-for-b-refreshed"), refreshedManifest); err != nil {
		t.Fatalf("Put (claim B, refresh): %v", err)
	}
	refreshed, ok, err := stores.ClaimCredentials.Get(ctx, orgID, conversationID)
	if err != nil || !ok {
		t.Fatalf("Get after refresh: ok=%v err=%v", ok, err)
	}
	if string(refreshed.Sealed) != "sealed-for-b-refreshed" {
		t.Fatalf("same-claim refresh did not apply: got %q, want sealed-for-b-refreshed", refreshed.Sealed)
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
		claimID := seedPgActiveClaim(t, h, orgID, convID, "executor-1", 1)
		if err := stores.ClaimCredentials.Put(ctx, orgID, claimID, "executor-1", 1, []byte("sealed"), m); err != nil {
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

// TestListAwaitingCredentials_Postgres_SkipsAParkedClaimThatHoldsABundle: the
// backstop sweep re-seals only a parked claim the brain has not answered yet.
// One whose bundle is written is waiting on the executor's next poll, and a
// second seal would only replace that bundle.
func TestListAwaitingCredentials_Postgres_SkipsAParkedClaimThatHoldsABundle(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	park := func() (convID, claimID string) {
		t.Helper()
		convID = seedPgDeliverableConversation(t, h, orgID, teamID, userID)
		claimID = seedPgActiveClaim(t, h, orgID, convID, "executor-1", 1)
		if matched, err := stores.ConversationQueue.MarkAwaitingCredentials(ctx, orgID, convID, "pubkey"); err != nil || !matched {
			t.Fatalf("MarkAwaitingCredentials = (%v, %v), want (true, nil)", matched, err)
		}
		return convID, claimID
	}
	waiting, _ := park()
	sealed, sealedClaim := park()
	if err := stores.ClaimCredentials.Put(ctx, orgID, sealedClaim, "executor-1", 1, []byte("sealed"), domain.SourceManifest{"github": {State: domain.SourceSealed}}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := stores.ConversationQueue.ListAwaitingCredentials(ctx)
	if err != nil {
		t.Fatalf("ListAwaitingCredentials: %v", err)
	}
	var ids []string
	for _, r := range got {
		ids = append(ids, r.ConversationID)
	}
	if !slices.Contains(ids, waiting) {
		t.Errorf("awaiting list %v is missing %s, a parked claim with no bundle", ids, waiting)
	}
	if slices.Contains(ids, sealed) {
		t.Errorf("awaiting list %v includes %s, whose bundle is already written", ids, sealed)
	}
}
