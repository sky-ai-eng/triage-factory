package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// placementClaimCfg is the enabled two-tier claim config every test here uses:
// a 20s aging window and a 12s liveness window, matching the placement
// defaults. Individual tests backdate queued_at / heartbeats relative to
// these to cross the thresholds deterministically (no sleeping).
var placementClaimCfg = db.ClaimPlacement{Enabled: true, AgingInterval: 20 * time.Second, Liveness: 12 * time.Second}

// enqueuePgConversationPreferred mints a fresh blueprint_run + task + prompt and
// enqueues one queued conversation stamped with preferred. Returns the
// conversation id.
func enqueuePgConversationPreferred(t *testing.T, h *pgtest.Harness, stores db.Stores, orgID, userID, preferred string) string {
	t.Helper()
	brID, taskID, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)
	conversationID := uuid.New().String()
	step0 := 0
	if _, err := stores.ConversationQueue.EnqueueConversation(context.Background(), orgID, domain.Conversation{
		ID: conversationID, TaskID: taskID, PromptID: promptID, Model: "m",
		TriggerType: "manual", CreatorUserID: userID, BlueprintRunID: brID, BlueprintStepIndex: &step0,
		PreferredExecutorID: preferred,
	}); err != nil {
		t.Fatalf("EnqueueConversation: %v", err)
	}
	return conversationID
}

// registerLiveExecutor registers an executor whose heartbeat is fresh (now()).
func registerLiveExecutor(t *testing.T, stores db.Stores, id string) {
	t.Helper()
	if _, err := stores.Instances.Register(context.Background(), id, domain.InstanceRoleExecutor, "v1", ""); err != nil {
		t.Fatalf("Register %s: %v", id, err)
	}
}

// backdatePgConversationQueued ages the conversation's current queue episode:
// queued_at is what the tier-2 aging arm reads, and started_at moves with it
// so the fairness order agrees with the age the test is staging.
func backdatePgConversationQueued(t *testing.T, h *pgtest.Harness, conversationID string, age time.Duration) {
	t.Helper()
	pgtest.MustExec(t, h.AdminDB,
		`UPDATE conversations SET queued_at = now() - $2::interval, started_at = now() - $2::interval WHERE id = $1`,
		conversationID, age.String())
}

// backdatePgConversationMint ages the conversation's mint stamp ALONE. It is
// how a test stages a conversation that is old but freshly queued — the
// shape every real resume has — to show the aging window does not read the
// mint.
func backdatePgConversationMint(t *testing.T, h *pgtest.Harness, conversationID string, age time.Duration) {
	t.Helper()
	pgtest.MustExec(t, h.AdminDB, `UPDATE conversations SET started_at = now() - $2::interval WHERE id = $1`,
		conversationID, age.String())
}

func backdatePgHeartbeat(t *testing.T, h *pgtest.Harness, id string, age time.Duration) {
	t.Helper()
	pgtest.MustExec(t, h.AdminDB, `UPDATE instances SET last_heartbeat_at = now() - $2::interval WHERE id = $1`,
		id, age.String())
}

func readPreferred(t *testing.T, h *pgtest.Harness, conversationID string) (string, bool) {
	t.Helper()
	var pref *string
	if err := h.AdminDB.QueryRow(`SELECT preferred_executor_id FROM conversations WHERE id = $1`, conversationID).Scan(&pref); err != nil {
		t.Fatalf("read preferred: %v", err)
	}
	if pref == nil {
		return "", false
	}
	return *pref, true
}

// TestPlacementClaim_TierOneExclusiveToOwner is the warm-cache property: a
// fresh conversation whose live preferred owner exists is claimable ONLY by
// that owner — a non-owner executor sees nothing (skips it) until it ages.
func TestPlacementClaim_TierOneExclusiveToOwner(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()
	orgID, userID := seedPgOrgForBlueprints(t, h)

	registerLiveExecutor(t, stores, "exec-a")
	registerLiveExecutor(t, stores, "exec-b")
	conversationID := enqueuePgConversationPreferred(t, h, stores, orgID, userID, "exec-a")

	// The enqueue actually persisted the stamp.
	if got, ok := readPreferred(t, h, conversationID); !ok || got != "exec-a" {
		t.Fatalf("preferred_executor_id = (%q, %v), want exec-a", got, ok)
	}

	// A non-owner (B) must NOT claim a fresh, live-owned conversation.
	got, err := stores.ConversationQueue.ClaimNextConversation(ctx, "exec-b", 1, placementClaimCfg)
	if err != nil {
		t.Fatalf("B claim: %v", err)
	}
	if got != nil {
		t.Fatalf("exec-b claimed exec-a's fresh conversation %q — tier-1 affinity not exclusive", got.ID)
	}

	// The owner claims it.
	got, err = stores.ConversationQueue.ClaimNextConversation(ctx, "exec-a", 1, placementClaimCfg)
	if err != nil || got == nil || got.ID != conversationID {
		t.Fatalf("A claim = (%+v, %v), want conversation %s", got, err, conversationID)
	}
}

// TestPlacementClaim_AgesToSpillover: once a conversation ages past the
// tier-2 window, any executor may claim it — affinity loses to
// capacity.
func TestPlacementClaim_AgesToSpillover(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()
	orgID, userID := seedPgOrgForBlueprints(t, h)

	registerLiveExecutor(t, stores, "exec-a") // owner, live (so it's only aging, not death, that spills)
	conversationID := enqueuePgConversationPreferred(t, h, stores, orgID, userID, "exec-a")
	backdatePgConversationQueued(t, h, conversationID, 45*time.Second) // > 20s aging window

	got, err := stores.ConversationQueue.ClaimNextConversation(ctx, "exec-b", 1, placementClaimCfg)
	if err != nil || got == nil || got.ID != conversationID {
		t.Fatalf("aged conversation should spill to exec-b: got=(%+v, %v)", got, err)
	}
}

// TestPlacementClaim_DeadPreferredSpillsImmediately: a fresh conversation
// whose preferred owner's heartbeat is stale spills at once, without waiting
// out the aging window — the "dead preferred" tier-2 branch.
func TestPlacementClaim_DeadPreferredSpillsImmediately(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()
	orgID, userID := seedPgOrgForBlueprints(t, h)

	registerLiveExecutor(t, stores, "exec-a")
	conversationID := enqueuePgConversationPreferred(t, h, stores, orgID, userID, "exec-a")
	// Run is FRESH (not aged), but the owner is dead.
	backdatePgHeartbeat(t, h, "exec-a", time.Hour)

	got, err := stores.ConversationQueue.ClaimNextConversation(ctx, "exec-b", 1, placementClaimCfg)
	if err != nil || got == nil || got.ID != conversationID {
		t.Fatalf("dead-owner fresh conversation should spill immediately: got=(%+v, %v)", got, err)
	}
}

// TestPlacementClaim_DrainingAndGatedPreferredSpill: a draining or
// dispatch-gated owner is not a live claimant — its fresh conversations
// spill at once.
func TestPlacementClaim_DrainingAndGatedPreferredSpill(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()
	orgID, userID := seedPgOrgForBlueprints(t, h)

	t.Run("draining", func(t *testing.T) {
		registerLiveExecutor(t, stores, "exec-drain")
		if _, err := stores.Instances.SetDraining(ctx, "exec-drain", true); err != nil {
			t.Fatalf("SetDraining: %v", err)
		}
		conversationID := enqueuePgConversationPreferred(t, h, stores, orgID, userID, "exec-drain")
		got, err := stores.ConversationQueue.ClaimNextConversation(ctx, "exec-helper", 1, placementClaimCfg)
		if err != nil || got == nil || got.ID != conversationID {
			t.Fatalf("draining owner's conversation should spill: got=(%+v, %v)", got, err)
		}
	})

	t.Run("gated", func(t *testing.T) {
		registerLiveExecutor(t, stores, "exec-gated")
		pgtest.MustExec(t, h.AdminDB, `UPDATE instances SET dispatch_gated = true WHERE id = $1`, "exec-gated")
		conversationID := enqueuePgConversationPreferred(t, h, stores, orgID, userID, "exec-gated")
		got, err := stores.ConversationQueue.ClaimNextConversation(ctx, "exec-helper", 1, placementClaimCfg)
		if err != nil || got == nil || got.ID != conversationID {
			t.Fatalf("gated owner's conversation should spill: got=(%+v, %v)", got, err)
		}
	})
}

// TestPlacementClaim_NullPreferredClaimableImmediately: a conversation with
// no stamp (unowned) is claimable by anyone at once — no aging wait. This is
// what makes requeue-to-NULL a correct, no-latency recovery.
func TestPlacementClaim_NullPreferredClaimableImmediately(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()
	orgID, userID := seedPgOrgForBlueprints(t, h)

	registerLiveExecutor(t, stores, "exec-a")
	conversationID := enqueuePgConversationPreferred(t, h, stores, orgID, userID, "") // NULL preferred

	got, err := stores.ConversationQueue.ClaimNextConversation(ctx, "exec-a", 1, placementClaimCfg)
	if err != nil || got == nil || got.ID != conversationID {
		t.Fatalf("unowned conversation should be immediately claimable: got=(%+v, %v)", got, err)
	}
}

// TestPlacementClaim_OwnerPrefersOwnFirst: with both its own fresh
// conversation and an older aged foreign conversation claimable, an executor
// takes its OWN first (tier 1 before tier 2) — warm cache wins over pure
// FIFO.
func TestPlacementClaim_OwnerPrefersOwnFirst(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()
	orgID, userID := seedPgOrgForBlueprints(t, h)

	registerLiveExecutor(t, stores, "exec-a")
	registerLiveExecutor(t, stores, "exec-b")

	// An OLDER foreign conversation (preferred=B), aged so exec-a may claim it
	// via tier 2.
	foreign := enqueuePgConversationPreferred(t, h, stores, orgID, userID, "exec-b")
	backdatePgConversationQueued(t, h, foreign, 2*time.Minute)
	// A NEWER own conversation (preferred=A), fresh.
	own := enqueuePgConversationPreferred(t, h, stores, orgID, userID, "exec-a")

	got, err := stores.ConversationQueue.ClaimNextConversation(ctx, "exec-a", 1, placementClaimCfg)
	if err != nil || got == nil {
		t.Fatalf("A claim: (%+v, %v)", got, err)
	}
	if got.ID != own {
		t.Fatalf("exec-a claimed %q, want its own fresh conversation %q first (tier 1 before the older aged foreign %q)", got.ID, own, foreign)
	}
}

// TestPlacementClaim_DisabledIgnoresPreferred is the feature-flag-off parity
// case: a disabled ClaimPlacement claims the globally-oldest conversation
// regardless of its stamp — dropping the placement layer changes nothing
// about which conversation a dispatcher gets.
func TestPlacementClaim_DisabledIgnoresPreferred(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()
	orgID, userID := seedPgOrgForBlueprints(t, h)

	registerLiveExecutor(t, stores, "exec-a")
	// Fresh conversation stamped to exec-a; a DIFFERENT executor claims with
	// placement OFF and must still get it (global-oldest, stamp ignored).
	conversationID := enqueuePgConversationPreferred(t, h, stores, orgID, userID, "exec-a")

	got, err := stores.ConversationQueue.ClaimNextConversation(ctx, "exec-other", 1, db.ClaimPlacement{})
	if err != nil || got == nil || got.ID != conversationID {
		t.Fatalf("disabled placement must ignore the stamp: got=(%+v, %v)", got, err)
	}
}

// TestPlacementClaim_RequeueClearsPreferred: the requeue/reset paths clear the
// stamp so a requeued conversation never carries a stale preference toward
// the executor that just failed it.
func TestPlacementClaim_RequeueClearsPreferred(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()
	orgID, userID := seedPgOrgForBlueprints(t, h)

	registerLiveExecutor(t, stores, "exec-a")
	conversationID := enqueuePgConversationPreferred(t, h, stores, orgID, userID, "exec-a")

	claimed, err := stores.ConversationQueue.ClaimNextConversation(ctx, "exec-a", 1, placementClaimCfg)
	if err != nil || claimed == nil {
		t.Fatalf("claim: (%+v, %v)", claimed, err)
	}
	if _, err := stores.ConversationQueue.RequeueConversation(ctx, orgID, conversationID, "transient"); err != nil {
		t.Fatalf("RequeueConversation: %v", err)
	}
	if got, ok := readPreferred(t, h, conversationID); ok {
		t.Fatalf("requeue must clear preferred_executor_id, still = %q", got)
	}
}

// TestPlacementClaim_ResumeFollowsTheWarmTree is the resume path's affinity:
// a stopped conversation woken by a follow-up routes back to the executor
// whose engagement drove it last — the machine still holding its workspace
// tree — rather than re-queueing unowned for whichever executor scans first.
//
// Both halves of "advisory" are asserted on one row: while the last executor
// is alive the conversation is exclusively its own (tier 1), and the moment
// that executor stops heartbeating the stamp costs nothing (tier 2 admits
// anyone at once, no aging wait).
func TestPlacementClaim_ResumeFollowsTheWarmTree(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()
	orgID, userID := seedPgOrgForBlueprints(t, h)

	registerLiveExecutor(t, stores, "exec-a")
	registerLiveExecutor(t, stores, "exec-b")

	// Born unowned, so nothing but the engagement itself can put A on the row.
	conversationID := enqueuePgConversationPreferred(t, h, stores, orgID, userID, "")
	claimed, err := stores.ConversationQueue.ClaimNextConversation(ctx, "exec-a", 1, placementClaimCfg)
	if err != nil || claimed == nil || claimed.ID != conversationID {
		t.Fatalf("A's first claim = (%+v, %v), want conversation %s", claimed, err, conversationID)
	}

	// The stop → "wait, one more thing" → resume flow.
	resume := func(t *testing.T) {
		t.Helper()
		if ok, pErr := stores.Conversations.ParkOpen(ctx, orgID, conversationID, db.ParkStopped(domain.ParkReasonUserCancelled, "")); pErr != nil || !ok {
			t.Fatalf("park: ok=%v err=%v", ok, pErr)
		}
		if ok, mErr := stores.Conversations.MarkQueuedForResume(ctx, orgID, conversationID); mErr != nil || !ok {
			t.Fatalf("MarkQueuedForResume: ok=%v err=%v", ok, mErr)
		}
		if got, ok := readPreferred(t, h, conversationID); !ok || got != "exec-a" {
			t.Fatalf("preferred_executor_id after the resume = (%q, %v), want exec-a", got, ok)
		}
	}
	resume(t)

	// B is live and idle, and the conversation is claimable — but it is A's
	// until it ages, because A is where the warm tree is.
	got, err := stores.ConversationQueue.ClaimNextConversation(ctx, "exec-b", 1, placementClaimCfg)
	if err != nil {
		t.Fatalf("B claim: %v", err)
	}
	if got != nil {
		t.Fatalf("exec-b claimed the resumed conversation %q inside the aging window; the resume stamp is not tier-1 exclusive", got.ID)
	}

	// A takes it straight back — no aging wait, and the tree it left behind is
	// seconds old.
	got, err = stores.ConversationQueue.ClaimNextConversation(ctx, "exec-a", 1, placementClaimCfg)
	if err != nil || got == nil || got.ID != conversationID {
		t.Fatalf("A's resume claim = (%+v, %v), want conversation %s", got, err, conversationID)
	}

	// Same row, same stamp, but A has stopped heartbeating: the warm tree is
	// on a machine nobody can reach, so the stamp yields immediately.
	resume(t)
	backdatePgHeartbeat(t, h, "exec-a", time.Hour)
	got, err = stores.ConversationQueue.ClaimNextConversation(ctx, "exec-b", 1, placementClaimCfg)
	if err != nil || got == nil || got.ID != conversationID {
		t.Fatalf("B's claim of a dead executor's resumed conversation = (%+v, %v), want conversation %s", got, err, conversationID)
	}
}

// TestPlacementClaim_ResumeAgesFromTheWake is the anchor of the aging window:
// a conversation minted long past the window, parked, and woken is its last
// executor's alone for a FULL window from the wake — the conversation's age
// buys a rival nothing — and claimable by anyone once the wake itself has
// aged. The dead/draining/gated spills keep ignoring the anchor.
func TestPlacementClaim_ResumeAgesFromTheWake(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()
	orgID, userID := seedPgOrgForBlueprints(t, h)

	registerLiveExecutor(t, stores, "exec-a")
	registerLiveExecutor(t, stores, "exec-b")

	conversationID := enqueuePgConversationPreferred(t, h, stores, orgID, userID, "")
	claimed, err := stores.ConversationQueue.ClaimNextConversation(ctx, "exec-a", 1, placementClaimCfg)
	if err != nil || claimed == nil || claimed.ID != conversationID {
		t.Fatalf("A's first claim = (%+v, %v), want conversation %s", claimed, err, conversationID)
	}
	// The conversation is an hour old by the time it is stopped and woken —
	// the shape of every real resume.
	backdatePgConversationMint(t, h, conversationID, time.Hour)
	if ok, pErr := stores.Conversations.ParkOpen(ctx, orgID, conversationID, db.ParkStopped(domain.ParkReasonUserCancelled, "")); pErr != nil || !ok {
		t.Fatalf("park: ok=%v err=%v", ok, pErr)
	}
	if ok, mErr := stores.Conversations.MarkQueuedForResume(ctx, orgID, conversationID); mErr != nil || !ok {
		t.Fatalf("MarkQueuedForResume: ok=%v err=%v", ok, mErr)
	}

	// B is live and idle, and the conversation is far older than the window;
	// it is still A's, because the window opened at the wake.
	got, err := stores.ConversationQueue.ClaimNextConversation(ctx, "exec-b", 1, placementClaimCfg)
	if err != nil {
		t.Fatalf("B claim: %v", err)
	}
	if got != nil {
		t.Fatalf("exec-b claimed the hour-old resumed conversation %q inside the aging window; the window is anchored on the mint, not the wake", got.ID)
	}

	// The wake ages past the window while the mint stamp stays where it was:
	// now anyone may take it.
	pgtest.MustExec(t, h.AdminDB, `UPDATE conversations SET queued_at = now() - interval '21 seconds' WHERE id = $1`, conversationID)
	got, err = stores.ConversationQueue.ClaimNextConversation(ctx, "exec-b", 1, placementClaimCfg)
	if err != nil || got == nil || got.ID != conversationID {
		t.Fatalf("B's claim once the wake aged = (%+v, %v), want conversation %s", got, err, conversationID)
	}
}
