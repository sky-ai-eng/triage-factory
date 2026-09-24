package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// pgConversationQueueExecutorID/pgConversationQueueBootEpoch are the fixed ownership identity
// most claims in this file stamp a row with; tests specifically exercising
// the ownership-scoping predicate (TFAC-578) pass their own values inline.
const (
	pgConversationQueueExecutorID = "test-executor"
	pgConversationQueueBootEpoch  = int64(1)
)

// TestConversationQueueStore_Postgres_ClaimCycle exercises the basic
// fire → claim → requeue → reset cycle against real Postgres (admin pool).
func TestConversationQueueStore_Postgres_ClaimCycle(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID := seedPgOrgForBlueprints(t, h)
	bpID, taskID, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)

	// Empty queue.
	if got, err := stores.ConversationQueue.ClaimNextConversation(ctx, pgConversationQueueExecutorID, pgConversationQueueBootEpoch, db.ClaimPlacement{}, db.DefaultClaimLease); err != nil || got != nil {
		t.Fatalf("ClaimNextConversation on empty queue = (%v, %v), want (nil, nil)", got, err)
	}

	conversationID := firePgStep(t, h, stores, orgID, bpID, taskID, domain.Conversation{
		PromptID: promptID, CreatorUserID: userID,
	}).ID

	got, err := stores.ConversationQueue.ClaimNextConversation(ctx, pgConversationQueueExecutorID, pgConversationQueueBootEpoch, db.ClaimPlacement{}, db.DefaultClaimLease)
	if err != nil || got == nil {
		t.Fatalf("ClaimNextConversation: (%v, %v)", got, err)
	}
	if got.ID != conversationID || got.Attempts != 1 {
		t.Fatalf("claimed = %+v", got)
	}
	// The claim writes no status: mid-flight is the absence of an outcome,
	// and the claim row itself is the ownership.
	if st, _ := pgConversationStatus(t, h, conversationID); st != "" {
		t.Fatalf("stored status after claim = %q, want none", st)
	}
	// team_id rides back on the claim (TFAC-458) and matches the value the
	// mint derived from the parent task — this is the construction-path
	// ConversationInfo.TeamID source the capture writers attribute artifacts by.
	var wantTeam string
	if err := h.AdminDB.QueryRow(`SELECT team_id::text FROM tasks WHERE id = $1`, taskID).Scan(&wantTeam); err != nil {
		t.Fatalf("read task team_id: %v", err)
	}
	if got.TeamID == "" || got.TeamID != wantTeam {
		t.Fatalf("claimed team_id = %q, want %q (task-derived)", got.TeamID, wantTeam)
	}

	// Requeue → re-claimable, attempts retained.
	if _, err := stores.ConversationQueue.RequeueConversation(ctx, orgID, conversationID, db.RequeueSetupFailure, "transient"); err != nil {
		t.Fatalf("RequeueConversation: %v", err)
	}
	got2, err := stores.ConversationQueue.ClaimNextConversation(ctx, pgConversationQueueExecutorID, pgConversationQueueBootEpoch, db.ClaimPlacement{}, db.DefaultClaimLease)
	if err != nil || got2 == nil || got2.Attempts != 2 {
		t.Fatalf("re-claim = (%+v, %v), want attempts=2", got2, err)
	}

	// ResetProcessingConversations flips the mid-flight 'running' row back to queued —
	// a later boot epoch of the same executor (a restart) sweeps it.
	n, err := stores.ConversationQueue.ResetProcessingConversations(ctx, pgConversationQueueExecutorID, pgConversationQueueBootEpoch+1)
	if err != nil || n != 1 {
		t.Fatalf("ResetProcessingConversations = (%d, %v), want (1, nil)", n, err)
	}
}

// TestConversationQueueStore_Postgres_ResetProcessingConversations_ScopedToOwner is the core
// TFAC-578 hazard test: two processes (distinct persistent executor
// identities) against one Postgres. Process B booting must NOT reset process
// A's claimed/running row — A's work would otherwise be re-queued and
// re-executed by A while it's still live, a duplicate-execution hazard on a
// rolling deploy or any N>1 topology.
func TestConversationQueueStore_Postgres_ResetProcessingConversations_ScopedToOwner(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID := seedPgOrgForBlueprints(t, h)
	bpID, taskID, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)
	conversationID := firePgStep(t, h, stores, orgID, bpID, taskID, domain.Conversation{
		PromptID: promptID, CreatorUserID: userID,
	}).ID

	// Process A claims and is still live (never crashed).
	claimed, err := stores.ConversationQueue.ClaimNextConversation(ctx, "process-a", 1, db.ClaimPlacement{}, db.DefaultClaimLease)
	if err != nil || claimed == nil || claimed.ID != conversationID {
		t.Fatalf("process-a claim: got=%v err=%v", claimed, err)
	}

	// Process B boots (a distinct instance, not a restart of A) and runs its
	// own boot reconcile. A's row must be untouched — different executor_id.
	n, err := stores.ConversationQueue.ResetProcessingConversations(ctx, "process-b", 1)
	if err != nil {
		t.Fatalf("process-b ResetProcessingConversations: %v", err)
	}
	if n != 0 {
		t.Errorf("process-b's boot reset %d rows, want 0 (must not touch process-a's live claim)", n)
	}
	if st, _ := pgConversationStatus(t, h, conversationID); st != "" {
		t.Errorf("conversation status = %q, want none (untouched by process-b's boot reset)", st)
	}
	if !pgHasActiveClaim(t, h, conversationID) {
		t.Error("process-b's boot reset released process-a's live claim")
	}

	// A itself restarting (a later boot epoch of the SAME executor_id) DOES
	// sweep its own orphan — this is the "kill -9 mid-flight, restart" path.
	n2, err := stores.ConversationQueue.ResetProcessingConversations(ctx, "process-a", 2)
	if err != nil {
		t.Fatalf("process-a restart ResetProcessingConversations: %v", err)
	}
	if n2 != 1 {
		t.Errorf("process-a's restart reset %d rows, want 1 (its own prior-boot orphan)", n2)
	}
	if st, _ := pgConversationStatus(t, h, conversationID); st != "" {
		t.Errorf("conversation status = %q, want none after process-a's own restart swept it", st)
	}
	if pgHasActiveClaim(t, h, conversationID) {
		t.Error("process-a's restart left its own prior-boot claim active")
	}
}

// TestConversationQueueStore_Postgres_ResetProcessingConversations_NeverResetsCurrentEpoch pins
// that a boot's own reconcile never touches rows claimed under its OWN
// current epoch — only strictly earlier boots of itself are orphans. This is
// what keeps ResetProcessingConversations safe to call unconditionally at every boot.
func TestConversationQueueStore_Postgres_ResetProcessingConversations_NeverResetsCurrentEpoch(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID := seedPgOrgForBlueprints(t, h)
	bpID, taskID, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)
	conversationID := firePgStep(t, h, stores, orgID, bpID, taskID, domain.Conversation{
		PromptID: promptID, CreatorUserID: userID,
	}).ID

	if _, err := stores.ConversationQueue.ClaimNextConversation(ctx, "process-self", 5, db.ClaimPlacement{}, db.DefaultClaimLease); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Same executor, same epoch: must not reset (not an earlier boot).
	if n, err := stores.ConversationQueue.ResetProcessingConversations(ctx, "process-self", 5); err != nil {
		t.Fatalf("ResetProcessingConversations at same epoch: %v", err)
	} else if n != 0 {
		t.Errorf("ResetProcessingConversations at the SAME epoch reset %d rows, want 0", n)
	}
	if !pgHasActiveClaim(t, h, conversationID) {
		t.Error("a same-epoch reset released the live claim")
	}
}

// TestConversationQueueStore_Postgres_CancelRequestedNotClaimed pins that a queued step
// of a cancel-requested blueprint is never claimed.
func TestConversationQueueStore_Postgres_CancelRequestedNotClaimed(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID := seedPgOrgForBlueprints(t, h)
	bpID, taskID, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)

	step := firePgStep(t, h, stores, orgID, bpID, taskID, domain.Conversation{
		PromptID: promptID, CreatorUserID: userID,
	})
	if changed, err := stores.Blueprints.RequestRunCancelSystem(ctx, orgID, step.BlueprintRunID); err != nil || !changed {
		t.Fatalf("RequestRunCancelSystem = (%v, %v)", changed, err)
	}
	if got, err := stores.ConversationQueue.ClaimNextConversation(ctx, pgConversationQueueExecutorID, pgConversationQueueBootEpoch, db.ClaimPlacement{}, db.DefaultClaimLease); err != nil || got != nil {
		t.Fatalf("ClaimNextConversation on cancel-requested blueprint = (%v, %v), want (nil, nil)", got, err)
	}
}

// TestConversationQueueStore_Postgres_ConcurrentClaim proves the FOR UPDATE SKIP LOCKED
// claim never hands the same queued conversation to two claimers: it stages N
// conversations and drains them from G goroutines, asserting every conversation
// is claimed exactly once.
func TestConversationQueueStore_Postgres_ConcurrentClaim(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID := seedPgOrgForBlueprints(t, h)
	bpID, _, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)

	// N runs queued at once = N tasks, each with one blueprint_run on its own
	// step 0. Neither unit offers two conversations at once — a blueprint
	// drives the step its pointer names, a task drives its live conversation —
	// so N simultaneously claimable conversations means N of each.
	const n = 40
	want := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		conv := firePgStep(t, h, stores, orgID, bpID, seedPgTask(t, h, orgID, userID), domain.Conversation{
			PromptID: promptID, CreatorUserID: userID,
		})
		want[conv.ID] = true
	}

	const workers = 8
	var (
		mu      sync.Mutex
		claimed = make(map[string]int)
		wg      sync.WaitGroup
	)
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for {
				conv, err := stores.ConversationQueue.ClaimNextConversation(ctx, pgConversationQueueExecutorID, pgConversationQueueBootEpoch, db.ClaimPlacement{}, db.DefaultClaimLease)
				if err != nil {
					t.Errorf("ClaimNextConversation: %v", err)
					return
				}
				if conv == nil {
					return // queue drained
				}
				mu.Lock()
				claimed[conv.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(claimed) != n {
		t.Fatalf("claimed %d distinct conversations, want %d", len(claimed), n)
	}
	for id, c := range claimed {
		if c != 1 {
			t.Errorf("conversation %s claimed %d times (double-claim)", id, c)
		}
		if !want[id] {
			t.Errorf("claimed unknown conversation %s", id)
		}
	}
}

// TestConversationQueueStore_Postgres_ReconcileOrphanedConversations heals the desync where a
// child conversation is left non-terminal under an already-terminal
// blueprint_run: the boot sweep cancels it (stamping completed_at) and only it,
// leaving a child under a still-running parent untouched.
func TestConversationQueueStore_Postgres_ReconcileOrphanedConversations(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID := seedPgOrgForBlueprints(t, h)

	// Orphan: terminal (cancelled) parent, child still running. A second
	// mid-flight orphan under the same parent — the step the blueprint moved
	// on to — that no claim ever picked up must also be parked: a claimable
	// step under a non-running parent is never actually claimed, so it would
	// sit in the queue forever.
	bpA, taskA, promptA := seedPgConversationQueueFixture(t, h, orgID, userID)
	orphan := firePgStep(t, h, stores, orgID, bpA, taskA, domain.Conversation{
		PromptID: promptA, CreatorUserID: userID,
	})
	orphanID := orphan.ID
	queuedOrphanID := advancePgStep(t, stores, orgID, orphan, domain.Conversation{
		PromptID: promptA, CreatorUserID: userID,
	}).ID
	if _, err := h.AdminDB.Exec(`UPDATE blueprint_runs SET status = 'cancelled', cancel_requested = false WHERE id = $1`, orphan.BlueprintRunID); err != nil {
		t.Fatalf("set parent cancelled: %v", err)
	}

	// Healthy: running parent, child running — must be left alone.
	bpB, taskB, promptB := seedPgConversationQueueFixture(t, h, orgID, userID)
	healthyID := firePgStep(t, h, stores, orgID, bpB, taskB, domain.Conversation{
		PromptID: promptB, CreatorUserID: userID,
	}).ID
	// A genuinely running child holds an active claim (ClaimNextConversation mints
	// it); without one, the claim-desync requeue arm would rightly treat the
	// row as stranded.
	if _, err := h.AdminDB.Exec(`
		INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch, lease_expires_at)
		VALUES ($1, $2, $3, 'exec-healthy', 1, now() + interval '300 seconds')
	`, uuid.New().String(), orgID, healthyID); err != nil {
		t.Fatalf("seed healthy claim: %v", err)
	}

	n, _, err := stores.ConversationQueue.ReconcileOrphanedConversations(ctx)
	if err != nil || n != 2 {
		t.Fatalf("ReconcileOrphanedConversations = (%d, %v), want (2, nil)", n, err)
	}
	if st, parked := pgConversationParked(t, h, orphanID); st != "open" || !parked {
		t.Errorf("orphan = (%q, parked=%v), want (open, true)", st, parked)
	}
	if st, parked := pgConversationParked(t, h, queuedOrphanID); st != "open" || !parked {
		t.Errorf("unclaimed orphan = (%q, parked=%v), want (open, true)", st, parked)
	}
	if st, _ := pgConversationStatus(t, h, healthyID); st != "" {
		t.Errorf("healthy conversation status = %q, want none (must not touch a mid-flight child under a running parent)", st)
	}

	// Idempotent: a second sweep finds nothing.
	if n2, _, err := stores.ConversationQueue.ReconcileOrphanedConversations(ctx); err != nil || n2 != 0 {
		t.Errorf("second ReconcileOrphanedConversations = (%d, %v), want (0, nil)", n2, err)
	}
}

// TestConversationQueueStore_Postgres_ReconcileCountsClaimDesyncs pins the
// checker: a terminal conversation with a live claim is counted and sampled,
// and nothing — its claim included — is repaired, because no writer produces
// the shape. A mid-flight row with no claim is simply claimable and left alone.
func TestConversationQueueStore_Postgres_ReconcileCountsClaimDesyncs(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID := seedPgOrgForBlueprints(t, h)
	bpID, _, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)

	// A task apiece, so every staged conversation is under its own running
	// blueprint_run: the arms below are about claims, and a shared parent
	// would let one subject's terminal decide another's.
	seedChild := func(status string) string {
		t.Helper()
		id := firePgStep(t, h, stores, orgID, bpID, seedPgTask(t, h, orgID, userID), domain.Conversation{
			PromptID: promptID, CreatorUserID: userID,
		}).ID
		if status != "" {
			pgtest.MustExec(t, h.AdminDB, `UPDATE conversations SET status = $2 WHERE id = $1`, id, status)
		}
		return id
	}
	activeClaim := func(convID string) string {
		t.Helper()
		claimID := uuid.New().String()
		pgtest.MustExec(t, h.AdminDB, `
			INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch, lease_expires_at)
			VALUES ($1, $2, $3, 'exec-ds', 1, now() + interval '300 seconds')
		`, claimID, orgID, convID)
		return claimID
	}

	// Dangling claims on terminal rows (the crash-after-flip shape).
	doneID := seedChild("completed")
	doneClaim := activeClaim(doneID)
	failedID := seedChild("failed")
	failedClaim := activeClaim(failedID)
	// Mid-flight row with no claim: under the derived model this is simply
	// a claimable conversation, so the sweep must leave it (and its stale
	// placement stamp, which the next claim re-earns) alone.
	strandedID := seedChild("")
	pgtest.MustExec(t, h.AdminDB, `UPDATE conversations SET preferred_executor_id = 'exec-dead' WHERE id = $1`, strandedID)
	// Healthy shapes.
	healthyID := seedChild("")
	healthyClaim := activeClaim(healthyID)
	queuedID := seedChild("")

	n, check, err := stores.ConversationQueue.ReconcileOrphanedConversations(ctx)
	if err != nil {
		t.Fatalf("ReconcileOrphanedConversations: %v", err)
	}
	if n != 0 {
		t.Errorf("healed count = %d, want 0 (a desync is counted, never healed)", n)
	}
	if check.ClaimDesyncs != 2 || len(check.ClaimDesyncSample) != 2 {
		t.Errorf("claim desyncs = (%d, %v), want 2 counted and sampled", check.ClaimDesyncs, check.ClaimDesyncSample)
	}

	claimState := func(id string) (released bool, outcome string) {
		t.Helper()
		if err := h.AdminDB.QueryRow(
			`SELECT released_at IS NOT NULL, COALESCE(outcome, '') FROM claims WHERE id = $1`, id,
		).Scan(&released, &outcome); err != nil {
			t.Fatalf("read claim %s: %v", id, err)
		}
		return released, outcome
	}
	if rel, _ := claimState(doneClaim); rel {
		t.Error("completed row's claim was released; the checker repairs nothing")
	}
	if rel, _ := claimState(failedClaim); rel {
		t.Error("failed row's claim was released; the checker repairs nothing")
	}
	var strandedStatus sql.NullString
	var pref any
	if err := h.AdminDB.QueryRow(
		`SELECT status, preferred_executor_id FROM conversations WHERE id = $1`, strandedID,
	).Scan(&strandedStatus, &pref); err != nil {
		t.Fatalf("read stranded row: %v", err)
	}
	if strandedStatus.Valid {
		t.Errorf("mid-flight claimless row status = %q, want none (already claimable)", strandedStatus.String)
	}
	if rel, _ := claimState(healthyClaim); rel {
		t.Error("healthy engaged row's live claim was released")
	}
	if st, _ := pgConversationStatus(t, h, healthyID); st != "" {
		t.Errorf("healthy engaged row status = %q, want none", st)
	}
	if st, _ := pgConversationStatus(t, h, queuedID); st != "" {
		t.Errorf("claimless row status = %q, want none (already claimable, nothing to heal)", st)
	}

	// Counting is idempotent: a second sweep finds the same two.
	if n2, check2, err := stores.ConversationQueue.ReconcileOrphanedConversations(ctx); err != nil || n2 != 0 || check2.ClaimDesyncs != 2 {
		t.Errorf("second sweep = (%d, %d desyncs, %v), want (0, 2, nil)", n2, check2.ClaimDesyncs, err)
	}
}

// TestConversationQueueStore_Postgres_MintStampsActorAgent is the Postgres parity of
// the SQLite actor-stamp test: the mint (both the manual and event branches)
// persists conversations.actor_agent_id, and ConversationStore.GetSystem
// JOINs agents to surface the display name as ActorAgentName. A conversation with
// no actor reads back with both fields empty.
func TestConversationQueueStore_Postgres_MintStampsActorAgent(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID := seedPgOrgForBlueprints(t, h)
	bpID, taskID, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)

	// One org agent backs the composite FK (actor_agent_id, org_id) and carries
	// the display name the read JOIN denormalizes.
	agentID := uuid.New().String()
	if _, err := h.AdminDB.Exec(
		`INSERT INTO agents (id, org_id, display_name) VALUES ($1, $2, 'Triage Bot')`,
		agentID, orgID,
	); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	// Manual branch stamps the actor.
	manual := firePgStep(t, h, stores, orgID, bpID, taskID, domain.Conversation{
		PromptID: promptID, CreatorUserID: userID, ActorAgentID: agentID,
	})
	got, err := stores.Conversations.GetSystem(ctx, orgID, manual.ID)
	if err != nil || got == nil {
		t.Fatalf("GetSystem (manual): (%v, %v)", got, err)
	}
	if got.ActorAgentID != agentID {
		t.Errorf("manual ActorAgentID = %q, want %q", got.ActorAgentID, agentID)
	}
	if got.ActorAgentName != "Triage Bot" {
		t.Errorf("manual ActorAgentName = %q, want %q (read JOIN)", got.ActorAgentName, "Triage Bot")
	}

	// Event branch (creator_user_id NULL per the schema CHECK) also stamps it.
	// A step after the first is minted by the advance, which is the only door
	// that mints one — so this walks the sequence rather than re-firing.
	event := advancePgStep(t, stores, orgID, manual, domain.Conversation{
		PromptID: promptID, TriggerType: "event", ActorAgentID: agentID,
	})
	ev, err := stores.Conversations.GetSystem(ctx, orgID, event.ID)
	if err != nil || ev == nil {
		t.Fatalf("GetSystem (event): (%v, %v)", ev, err)
	}
	if ev.ActorAgentID != agentID || ev.ActorAgentName != "Triage Bot" {
		t.Errorf("event conversation actor = (%q, %q), want (%q, Triage Bot)", ev.ActorAgentID, ev.ActorAgentName, agentID)
	}

	// No actor → both fields empty (nullable column + LEFT JOIN).
	bareID := advancePgStep(t, stores, orgID, event, domain.Conversation{
		PromptID: promptID, CreatorUserID: userID,
	}).ID
	bare, err := stores.Conversations.GetSystem(ctx, orgID, bareID)
	if err != nil || bare == nil {
		t.Fatalf("GetSystem (no actor): (%v, %v)", bare, err)
	}
	if bare.ActorAgentID != "" || bare.ActorAgentName != "" {
		t.Errorf("no-actor conversation = (%q, %q), want both empty", bare.ActorAgentID, bare.ActorAgentName)
	}
}

// TestConversationQueueStore_Postgres_MintStampsTheNativeEngine pins the single fact
// that makes the SDK engine unreachable for a delegation in this mode: the
// mint names the engine, so no row is ever written that a claimant would have
// to be taught to refuse. There is no caller-passed knob and no reliance on
// the column DEFAULT, which still reads 'sdk'.
//
// Both arms, because they are separate statements: a stamp added to one and
// forgotten in the other is exactly the drift this catches, and the arm that
// forgot would take the DEFAULT silently.
//
// It reads the stored column rather than the projection on purpose. The
// question is what the write put there, and a projection that COALESCEd an
// absent value would answer it wrong.
func TestConversationQueueStore_Postgres_MintStampsTheNativeEngine(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	orgID, userID := seedPgOrgForBlueprints(t, h)
	bpID, taskID, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)

	storedRuntime := func(t *testing.T, convID string) string {
		t.Helper()
		var runtime string
		if err := h.AdminDB.QueryRow(
			`SELECT runtime FROM conversations WHERE id = $1`, convID,
		).Scan(&runtime); err != nil {
			t.Fatalf("read runtime: %v", err)
		}
		return runtime
	}

	// Step 0 is the manual arm (the schema CHECK pairs a manual trigger with
	// a creator); the event arm is the step after it, which is the only way a
	// second step is minted.
	manual := firePgStep(t, h, stores, orgID, bpID, taskID, domain.Conversation{
		PromptID: promptID, CreatorUserID: userID,
	})
	event := advancePgStep(t, stores, orgID, manual, domain.Conversation{
		PromptID: promptID, TriggerType: "event",
	})
	for trigger, convID := range map[string]string{"manual": manual.ID, "event": event.ID} {
		if got := storedRuntime(t, convID); got != domain.ConversationRuntimeNative {
			t.Errorf("%s mint runtime = %q, want %q", trigger, got, domain.ConversationRuntimeNative)
		}
	}
}

// TestConversationQueueStore_Postgres_Credentials runs the shared awaiting-credentials
// pubkey conformance suite against the Postgres impl (admin pool, matching
// production wiring). Each factory call resets the harness so subtests don't
// share state.
func TestConversationQueueStore_Postgres_Credentials(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunClaimCredentialsConformance(t, func(t *testing.T) (db.ConversationQueueStore, string, dbtest.ClaimCredentialsSeeder) {
		t.Helper()
		h.Reset(t)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
		orgID, userID := seedPgOrgForBlueprints(t, h)
		bpID, _, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)

		seed := dbtest.ClaimCredentialsSeeder{
			StageStep: func(t *testing.T) string {
				t.Helper()
				return firePgStep(t, h, stores, orgID, bpID, seedPgTask(t, h, orgID, userID), domain.Conversation{
					PromptID: promptID, CreatorUserID: userID,
				}).ID
			},
			ConversationStatus: func(t *testing.T, conversationID string) string {
				t.Helper()
				var status sql.NullString
				if err := h.AdminDB.QueryRow(`SELECT status FROM conversations WHERE id = $1`, conversationID).Scan(&status); err != nil {
					t.Fatalf("read status: %v", err)
				}
				return status.String
			},
			SetActivePhase: func(t *testing.T, conversationID, phase string) {
				t.Helper()
				if _, err := h.AdminDB.Exec(`
					UPDATE claims SET phase = NULLIF($1, '')
					WHERE conversation_id = $2 AND released_at IS NULL
				`, phase, conversationID); err != nil {
					t.Fatalf("set active phase %q: %v", phase, err)
				}
			},
		}
		return stores.ConversationQueue, orgID, seed
	})
}

// TestConversationQueueStore_Postgres_FleetQueueShares runs the shared per-org
// queue-share conformance against the Postgres impl (admin pool). Each factory
// call resets the harness so subtests don't share state.
func TestConversationQueueStore_Postgres_FleetQueueShares(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunFleetQueueSharesConformance(t, func(t *testing.T) (db.ConversationQueueStore, string, dbtest.FleetQueueSharesSeeder) {
		t.Helper()
		h.Reset(t)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
		orgID, userID := seedPgOrgForBlueprints(t, h)

		seed := dbtest.FleetQueueSharesSeeder{
			// One firing per staged run — the real model (one delegation is
			// one blueprint_run on one task), and what makes several queued
			// rows concurrently claimable: a blueprint drives its current
			// step and no other, so siblings under one blueprint could never
			// all be queued at once.
			StageStep: func(t *testing.T) string {
				t.Helper()
				bpID, taskID, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)
				return firePgStep(t, h, stores, orgID, bpID, taskID, domain.Conversation{
					PromptID: promptID, CreatorUserID: userID,
				}).ID
			},
			ForceStatus: func(t *testing.T, conversationID, status string) {
				t.Helper()
				if _, err := h.AdminDB.Exec(`UPDATE conversations SET status = $1 WHERE id = $2`, status, conversationID); err != nil {
					t.Fatalf("force status %q: %v", status, err)
				}
			},
			SetMaxConcurrentRuns: func(t *testing.T, cap *int) {
				t.Helper()
				if _, err := h.AdminDB.Exec(`
					INSERT INTO org_settings (org_id, max_concurrent_runs) VALUES ($1, $2)
					ON CONFLICT (org_id) DO UPDATE SET max_concurrent_runs = EXCLUDED.max_concurrent_runs
				`, orgID, cap); err != nil {
					t.Fatalf("set max_concurrent_runs: %v", err)
				}
			},
		}
		return stores.ConversationQueue, orgID, seed
	})
}

// pgConversationStatus reads a conversation's STORED status (SQL NULL — the mid-flight state
// — as "") and whether completed_at is stamped.
func pgConversationStatus(t *testing.T, h *pgtest.Harness, conversationID string) (status string, completed bool) {
	t.Helper()
	var stored sql.NullString
	var completedAt *string
	if err := h.AdminDB.QueryRow(`SELECT status, completed_at::text FROM conversations WHERE id = $1`, conversationID).
		Scan(&stored, &completedAt); err != nil {
		t.Fatalf("read conversation %s: %v", conversationID, err)
	}
	return stored.String, completedAt != nil
}

// pgConversationParked is pgConversationStatus's park-side sibling: the stored status plus
// whether parked_at is stamped, which is what the snapshot-retention sweep
// keys a parked conversation off.
func pgConversationParked(t *testing.T, h *pgtest.Harness, conversationID string) (status string, parked bool) {
	t.Helper()
	var stored sql.NullString
	var parkedAt *string
	if err := h.AdminDB.QueryRow(`SELECT status, parked_at::text FROM conversations WHERE id = $1`, conversationID).
		Scan(&stored, &parkedAt); err != nil {
		t.Fatalf("read conversation %s: %v", conversationID, err)
	}
	return stored.String, parkedAt != nil
}

// seedPgConversationQueueFixture mints the parents one delegation needs — a
// prompt, a blueprint and a fresh task — returning (blueprintID, taskID,
// promptID) ready to fire a step against.
func seedPgConversationQueueFixture(t *testing.T, h *pgtest.Harness, orgID, userID string) (bpID, taskID, promptID string) {
	t.Helper()
	bpID = "rq-bp-" + uuid.New().String()[:8]
	seedPgBlueprint(t, h, orgID, userID, bpID)
	promptID = "rq-p-" + uuid.New().String()[:8]
	seedPgPrompt(t, h, orgID, userID, promptID)
	return bpID, seedPgTask(t, h, orgID, userID), promptID
}

// seedPgChildlessRun writes a running blueprint_run with no step under it —
// the shape no door can produce, because a firing commits its first step in
// the same transaction as the run. The boot reconcile's checker exists to
// count exactly this, so its suite has to be able to write it.
func seedPgChildlessRun(t *testing.T, h *pgtest.Harness, orgID, userID, bpID, taskID string) string {
	t.Helper()
	brID := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO blueprint_runs (id, org_id, creator_user_id, blueprint_id, task_id, trigger_type, status, worktree_path, started_at, step_plan)
		VALUES ($1, $2, $3, $4, $5, 'manual', 'running', $6, now(), '[]')
	`, brID, orgID, userID, bpID, taskID, "/tmp/wt-"+brID); err != nil {
		t.Fatalf("seed blueprint_run: %v", err)
	}
	return brID
}

// seedPgMidFlightStep writes one step conversation under an existing run, at
// the index the caller names and with no stored status. Direct SQL for the
// same reason as the helper above: the suites that reach for it stage
// children at indices their run's pointer does not name, and under parents
// the doors would refuse to mint against at all.
func seedPgMidFlightStep(t *testing.T, h *pgtest.Harness, orgID, userID, taskID, promptID, brID string, stepIndex int) string {
	t.Helper()
	convID := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO conversations (id, org_id, type, runtime, task_id, prompt_id, model, trigger_type,
		                           team_id, visibility, creator_user_id, blueprint_run_id, blueprint_step_index, queued_at)
		VALUES ($1, $2, 'delegation', 'native', $3, $4, 'm', 'manual',
		        (SELECT team_id FROM tasks WHERE id = $3 AND org_id = $2), 'team', $5::uuid, $6, $7, now())
	`, convID, orgID, taskID, promptID, userID, brID, stepIndex); err != nil {
		t.Fatalf("seed step conversation: %v", err)
	}
	return convID
}

// TestConversationQueueStore_Postgres_QueuedAtStamps mirrors the SQLite twin: the mint
// stamps queued_at, a claim stamps claimed_at (both surfaced through
// Conversations.GetSystem), and a requeue re-stamps queued_at and clears
// claimed_at so the next dwell measures from the re-entry, not the mint.
func TestConversationQueueStore_Postgres_QueuedAtStamps(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID := seedPgOrgForBlueprints(t, h)
	bpID, taskID, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)

	conversationID := firePgStep(t, h, stores, orgID, bpID, taskID, domain.Conversation{
		PromptID: promptID, CreatorUserID: userID,
	}).ID

	queued, err := stores.Conversations.GetSystem(ctx, orgID, conversationID)
	if err != nil || queued == nil {
		t.Fatalf("GetSystem after the mint: (%v, %v)", queued, err)
	}
	if queued.QueuedAt == nil {
		t.Fatal("QueuedAt = nil after the mint; the mint must stamp queue entry")
	}
	if queued.ClaimedAt != nil {
		t.Fatalf("ClaimedAt = %v on a queued conversation, want nil", queued.ClaimedAt)
	}
	firstQueuedAt := *queued.QueuedAt

	if got, err := stores.ConversationQueue.ClaimNextConversation(ctx, pgConversationQueueExecutorID, pgConversationQueueBootEpoch, db.ClaimPlacement{}, db.DefaultClaimLease); err != nil || got == nil {
		t.Fatalf("ClaimNextConversation: (%v, %v)", got, err)
	}
	claimed, err := stores.Conversations.GetSystem(ctx, orgID, conversationID)
	if err != nil || claimed == nil {
		t.Fatalf("GetSystem after claim: (%v, %v)", claimed, err)
	}
	if claimed.ClaimedAt == nil {
		t.Fatal("ClaimedAt = nil after claim")
	}
	if claimed.ClaimedAt.Before(firstQueuedAt) {
		t.Fatalf("ClaimedAt %v precedes QueuedAt %v", claimed.ClaimedAt, firstQueuedAt)
	}

	if _, err := stores.ConversationQueue.RequeueConversation(ctx, orgID, conversationID, db.RequeueSetupFailure, "transient setup error"); err != nil {
		t.Fatalf("RequeueConversation: %v", err)
	}
	requeued, err := stores.Conversations.GetSystem(ctx, orgID, conversationID)
	if err != nil || requeued == nil {
		t.Fatalf("GetSystem after requeue: (%v, %v)", requeued, err)
	}
	if requeued.QueuedAt == nil || requeued.QueuedAt.Before(firstQueuedAt) {
		t.Fatalf("QueuedAt after requeue = %v, want re-stamped at/after the first stamp %v", requeued.QueuedAt, firstQueuedAt)
	}
	// The claims model keeps engagement history: ClaimedAt stays the released
	// claim's stamp, but the row reads unowned (no active claim).
	if requeued.ClaimedAt == nil {
		t.Fatal("ClaimedAt = nil after requeue, want the released engagement's stamp retained")
	}
	if requeued.ExecutorID != "" {
		t.Fatalf("ExecutorID = %q after requeue, want empty (a queued row has no active claim)", requeued.ExecutorID)
	}
}

// TestConversationQueueStore_Postgres_RequeueFromSetupPhase pins that RequeueConversation fires
// no matter which setup phase the conversation's active claim is in: setup progress
// lives on the claim, the conversation stays 'running' the whole time, so a
// workspace-setup failure mid-phase must still requeue the row and make it
// re-claimable. Coverage walks the canonical vocabulary, so a phase added in
// Go and not handled here fails rather than going untested.
func TestConversationQueueStore_Postgres_RequeueFromSetupPhase(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	for _, phase := range domain.AllClaimPhases() {
		t.Run(phase, func(t *testing.T) {
			h.Reset(t)
			orgID, userID := seedPgOrgForBlueprints(t, h)
			bpID, taskID, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)

			conversationID := firePgStep(t, h, stores, orgID, bpID, taskID, domain.Conversation{
				PromptID: promptID, CreatorUserID: userID,
			}).ID
			if got, err := stores.ConversationQueue.ClaimNextConversation(ctx, pgConversationQueueExecutorID, pgConversationQueueBootEpoch, db.ClaimPlacement{}, db.DefaultClaimLease); err != nil || got == nil {
				t.Fatalf("ClaimNextConversation: (%v, %v)", got, err)
			}
			// Advance the claim into the setup phase the dispatcher would
			// have recorded before the workspace-setup failure fired the
			// requeue.
			if _, err := stores.Conversations.SetActiveClaimPhaseSystem(ctx, orgID, conversationID, phase); err != nil {
				t.Fatalf("SetActiveClaimPhaseSystem(%s): %v", phase, err)
			}

			if _, err := stores.ConversationQueue.RequeueConversation(ctx, orgID, conversationID, db.RequeueSetupFailure, "workspace setup: boom"); err != nil {
				t.Fatalf("RequeueConversation: %v", err)
			}
			after, err := stores.Conversations.GetSystem(ctx, orgID, conversationID)
			if err != nil || after == nil {
				t.Fatalf("GetSystem after requeue: (%v, %v)", after, err)
			}
			if after.Status != "queued" {
				t.Fatalf("status after requeue from phase %q = %q, want queued (a mid-setup phase must not block the requeue)", phase, after.Status)
			}
			if reclaimed, err := stores.ConversationQueue.ClaimNextConversation(ctx, pgConversationQueueExecutorID, pgConversationQueueBootEpoch, db.ClaimPlacement{}, db.DefaultClaimLease); err != nil || reclaimed == nil {
				t.Fatalf("re-ClaimNextConversation after requeue from phase %q: (%v, %v)", phase, reclaimed, err)
			}
		})
	}
}

// TestConversationQueueStore_Postgres_ExecutorClaims runs the shared operator
// claim-projection conformance against the Postgres impl (admin pool, matching
// production wiring — the read is deployment-wide and RLS-bypassing by
// design). Each factory call resets the harness so subtests don't share state.
func TestConversationQueueStore_Postgres_ExecutorClaims(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunExecutorClaimsConformance(t, func(t *testing.T) (db.ConversationQueueStore, dbtest.ExecutorClaimsSeeder) {
		t.Helper()
		h.Reset(t)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
		orgID, userID := seedPgOrgForBlueprints(t, h)
		bpID, _, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)

		seed := dbtest.ExecutorClaimsSeeder{
			OrgID: orgID,
			Conversation: func(t *testing.T, status, failureKind string) string {
				t.Helper()
				conversationID := firePgStep(t, h, stores, orgID, bpID, seedPgTask(t, h, orgID, userID), domain.Conversation{
					PromptID: promptID, CreatorUserID: userID,
				}).ID
				if _, err := h.AdminDB.Exec(`
					UPDATE conversations SET status = $1, failure_kind = NULLIF($2, '') WHERE id = $3
				`, status, failureKind, conversationID); err != nil {
					t.Fatalf("force terminal state: %v", err)
				}
				return conversationID
			},
			Claim: func(t *testing.T, row dbtest.ExecutorClaimRow) string {
				t.Helper()
				claimID := uuid.New().String()
				var released any
				if row.ReleasedAt != nil {
					released = row.ReleasedAt.UTC()
				}
				var peak, cpu any
				if row.PeakMemMB != nil {
					peak = *row.PeakMemMB
				}
				if row.CPUUsec != nil {
					cpu = *row.CPUUsec
				}
				if _, err := h.AdminDB.Exec(`
					INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch,
					                    claimed_at, released_at, outcome, peak_mem_mb, cpu_usec, lease_expires_at)
					VALUES ($1, $2, $3, $4, 1, $5, $6, NULLIF($7, ''), $8, $9, now() + interval '300 seconds')
				`, claimID, orgID, row.ConversationID, row.ExecutorID, row.ClaimedAt.UTC(), released,
					row.Outcome, peak, cpu); err != nil {
					t.Fatalf("insert claim: %v", err)
				}
				return claimID
			},
		}
		return stores.ConversationQueue, seed
	})
}

// pgHasActiveClaim reports whether the conversation currently holds an
// unreleased claim — the derived "an engagement is driving this".
func pgHasActiveClaim(t *testing.T, h *pgtest.Harness, convID string) bool {
	t.Helper()
	var live bool
	if err := h.AdminDB.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM claims WHERE conversation_id = $1 AND released_at IS NULL)`, convID,
	).Scan(&live); err != nil {
		t.Fatalf("read active claim for %s: %v", convID, err)
	}
	return live
}

// TestClaimPredicate_Postgres runs the shared needs-driving-predicate +
// display-ladder conformance against the Postgres impl (admin pool, matching
// production wiring). Each factory call resets the harness so subtests don't
// share state.
func TestClaimPredicate_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	ctx := context.Background()

	dbtest.RunClaimPredicateConformance(t, func(t *testing.T) dbtest.ClaimPredicateHarness {
		t.Helper()
		h.Reset(t)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
		orgID, userID := seedPgOrgForBlueprints(t, h)
		bpID, taskID, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)

		brID, nextStep := "", 0
		return dbtest.ClaimPredicateHarness{
			Stores: stores,
			OrgID:  orgID,
			UserID: userID,
			StageDelegation: func(t *testing.T, runtime string) string {
				t.Helper()
				idx := nextStep
				nextStep++
				var convID string
				if idx == 0 {
					// Step 0 is a real firing, so the fresh-mint assertions —
					// no stored status, the dialect's own runtime stamp — are
					// about the statement production mints with.
					step0 := firePgStep(t, h, stores, orgID, bpID, taskID, domain.Conversation{
						PromptID: promptID, CreatorUserID: userID,
					})
					brID, convID = step0.BlueprintRunID, step0.ID
				} else {
					// Every step after it is written directly, pointer first,
					// then the row it names. The advance door commits those
					// two together and only from the step the pointer already
					// names — and this suite rewrites the pointer by hand
					// between stagings, parking a run behind its newest step
					// and resuming past it, so a door that refuses those
					// states is a door it cannot stage through.
					pgtest.MustExec(t, h.AdminDB,
						`UPDATE blueprint_runs SET current_step_index = $2 WHERE id = $1`, brID, idx)
					convID = seedPgMidFlightStep(t, h, orgID, userID, taskID, promptID, brID, idx)
				}
				// The dialect stamps its own runtime at mint; rewrite it so
				// one backend covers both engines. started_at is stamped in
				// the same pass, one second apart per mint: the task clause
				// picks the task's NEWEST un-ended conversation, and a suite
				// that staged its rows inside one clock tick would be asking
				// the id tiebreak — a random uuid — which one that is.
				pgtest.MustExec(t, h.AdminDB,
					`UPDATE conversations SET runtime = $2, started_at = now() + make_interval(secs => $3) WHERE id = $1`,
					convID, runtime, float64(idx))
				return convID
			},
			StageUnindexed: func(t *testing.T) error {
				t.Helper()
				_, err := tryFirePgStep(t, h, stores, orgID, bpID, seedPgTask(t, h, orgID, userID), domain.Conversation{
					PromptID: promptID, CreatorUserID: userID,
				})
				return err
			},
			SetStoredStatus: func(t *testing.T, convID, status string) {
				t.Helper()
				var stored any
				if status != "" {
					stored = status
				}
				pgtest.MustExec(t, h.AdminDB, `UPDATE conversations SET status = $2 WHERE id = $1`, convID, stored)
			},
			StoredStatus: func(t *testing.T, convID string) string {
				t.Helper()
				var status sql.NullString
				if err := h.AdminDB.QueryRow(`SELECT status FROM conversations WHERE id = $1`, convID).Scan(&status); err != nil {
					t.Fatalf("read stored status: %v", err)
				}
				return status.String
			},
			SetBlueprintState: func(t *testing.T, status string, currentStepIndex int) {
				t.Helper()
				// The sequence task carries exactly one run — the firing that
				// opened it — so the task is the address here and the harness
				// keeps no run id of its own.
				pgtest.MustExec(t, h.AdminDB,
					`UPDATE blueprint_runs SET status = $2, current_step_index = $3 WHERE task_id = $1`,
					taskID, status, currentStepIndex)
			},
			InsertRow: func(t *testing.T, convID string, msg domain.Message) int64 {
				t.Helper()
				msg.ConversationID = convID
				id, err := stores.Conversations.InsertMessageSystem(ctx, orgID, &msg)
				if err != nil {
					t.Fatalf("insert message: %v", err)
				}
				return id
			},
			SetSeq: func(t *testing.T, msgID int64, seq float64) {
				t.Helper()
				pgtest.MustExec(t, h.AdminDB, `UPDATE messages SET seq = $2 WHERE id = $1`, msgID, seq)
			},
			SetParentConversation: func(t *testing.T, convID, parentConvID string) {
				t.Helper()
				pgtest.MustExec(t, h.AdminDB,
					`UPDATE conversations SET parent_conversation_id = $2 WHERE id = $1`, convID, parentConvID)
			},
			CollapseClaimTimestamps: func(t *testing.T, convID string) {
				t.Helper()
				pgtest.MustExec(t, h.AdminDB, `
					UPDATE claims
					SET claimed_at = timestamptz '2026-01-01 00:00:00Z',
					    released_at = CASE WHEN released_at IS NULL THEN NULL ELSE timestamptz '2026-01-01 00:00:00Z' END
					WHERE conversation_id = $1`, convID)
			},
			DisplayStatus: func(t *testing.T, convID string) string {
				t.Helper()
				got, err := stores.Conversations.GetSystem(ctx, orgID, convID)
				if err != nil || got == nil {
					t.Fatalf("Conversations.GetSystem = (%v, %v)", got, err)
				}
				return got.Status
			},
		}
	})
}

// TestConversationQueueStore_Postgres_ReconcileOrphanedConversationsConformance runs the shared
// boot-reconcile conformance against the Postgres impl (admin pool, matching
// production wiring). Each factory call resets the harness, so the suite's exact
// healed counts are this subtest's alone.
func TestConversationQueueStore_Postgres_ReconcileOrphanedConversationsConformance(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunReconcileOrphanedConversationsConformance(t, func(t *testing.T) (db.ConversationQueueStore, dbtest.ReconcileOrphanSeeder) {
		t.Helper()
		h.Reset(t)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
		orgID, userID := seedPgOrgForBlueprints(t, h)
		bpID, _, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)

		// Both seeders below write their rows directly. This suite's whole
		// subject is the shapes the invariants forbid — a childless run, a
		// mid-flight child under a terminal parent, a pointer naming a step
		// nothing minted — and the doors that mint a step are the ones that
		// make those shapes unreachable, so they cannot stage them.
		//
		// One task per run either way: blueprint_runs_one_active_run_per_task
		// refuses a second 'running' row on a task, and the suite stages
		// several at once.
		runTask := map[string]string{}
		nextStep := map[string]int{}
		seed := dbtest.ReconcileOrphanSeeder{
			BlueprintRun: func(t *testing.T, age time.Duration) string {
				t.Helper()
				taskID := seedPgTask(t, h, orgID, userID)
				id := seedPgChildlessRun(t, h, orgID, userID, bpID, taskID)
				runTask[id] = taskID
				if age > 0 {
					pgtest.MustExec(t, h.AdminDB,
						`UPDATE blueprint_runs SET started_at = now() - $2::interval WHERE id = $1`, id, age.String())
				}
				return id
			},
			StageChild: func(t *testing.T, brID string) string {
				t.Helper()
				idx := nextStep[brID]
				nextStep[brID]++
				return seedPgMidFlightStep(t, h, orgID, userID, runTask[brID], promptID, brID, idx)
			},
			ForceBlueprintStatus: func(t *testing.T, brID, status, abortReason string) {
				t.Helper()
				pgtest.MustExec(t, h.AdminDB,
					`UPDATE blueprint_runs SET status = $2, abort_reason = NULLIF($3, '') WHERE id = $1`,
					brID, status, abortReason)
			},
			SetCurrentStep: func(t *testing.T, brID string, stepIndex int) {
				t.Helper()
				pgtest.MustExec(t, h.AdminDB,
					`UPDATE blueprint_runs SET current_step_index = $2 WHERE id = $1`, brID, stepIndex)
			},
			BlueprintRunState: func(t *testing.T, brID string) (string, string, bool) {
				t.Helper()
				var status string
				var reason sql.NullString
				var completed bool
				if err := h.AdminDB.QueryRow(
					`SELECT status, abort_reason, completed_at IS NOT NULL FROM blueprint_runs WHERE id = $1`, brID,
				).Scan(&status, &reason, &completed); err != nil {
					t.Fatalf("read blueprint_run %s: %v", brID, err)
				}
				return status, reason.String, completed
			},
			ConversationStatus: func(t *testing.T, convID string) string {
				t.Helper()
				var status sql.NullString
				if err := h.AdminDB.QueryRow(`SELECT status FROM conversations WHERE id = $1`, convID).Scan(&status); err != nil {
					t.Fatalf("read conversation %s status: %v", convID, err)
				}
				return status.String
			},
		}
		return stores.ConversationQueue, seed
	})
}

// TestConversationQueueStore_Postgres_ReturnedRow runs the returned-row
// conformance suite against the admin pool — see
// ConversationQueueReturnedRowFactory's doc for why there is no separate
// app-pool arm.
func TestConversationQueueStore_Postgres_ReturnedRow(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	dbtest.RunConversationQueueReturnedRowConformance(t, func(t *testing.T) (db.ConversationQueueStore, db.ConversationStore, string, dbtest.ConversationQueueReturnedRowScaffold) {
		t.Helper()
		h.Reset(t)
		orgID, userID := seedPgOrgForBlueprints(t, h)

		scaffold := func(t *testing.T) string {
			t.Helper()
			bpID := "cqrr-bp-" + uuid.New().String()[:8]
			seedPgBlueprint(t, h, orgID, userID, bpID)
			promptID := "cqrr-p-" + uuid.New().String()[:8]
			seedPgPrompt(t, h, orgID, userID, promptID)
			return firePgStep(t, h, stores, orgID, bpID, seedPgTask(t, h, orgID, userID), domain.Conversation{
				PromptID: promptID, CreatorUserID: userID,
			}).ID
		}
		return stores.ConversationQueue, stores.Conversations, orgID, scaffold
	})
}

// TestClaimLease_Postgres runs the shared claim-lease conformance against the
// Postgres impl (admin pool, matching production wiring). Each factory call
// resets the harness so subtests don't share state.
func TestClaimLease_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	dbtest.RunClaimLeaseConformance(t, func(t *testing.T) dbtest.ClaimLeaseFixture { return pgClaimLeaseFixture(t, h) })
}

// TestStopIntent_Postgres runs the shared stop-intent conformance on the same
// fixture the claim-lease suite uses.
func TestStopIntent_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	dbtest.RunStopIntentConformance(t, func(t *testing.T) dbtest.ClaimLeaseFixture { return pgClaimLeaseFixture(t, h) })
}

// TestClaimTakeover_Postgres runs the shared recovery-pass conformance — the
// takeover, the shutdown release, the boot reset, the widened settlement and
// the stranded-run read — on the claim-lease fixture.
func TestClaimTakeover_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	dbtest.RunClaimTakeoverConformance(t, func(t *testing.T) dbtest.ClaimLeaseFixture { return pgClaimLeaseFixture(t, h) })
}

func pgClaimLeaseFixture(t *testing.T, h *pgtest.Harness) dbtest.ClaimLeaseFixture {
	t.Helper()
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	orgID, userID := seedPgOrgForBlueprints(t, h)
	bpID, _, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)

	return dbtest.ClaimLeaseFixture{
		Stores: stores,
		OrgID:  orgID,
		StageStep: func(t *testing.T) (string, string) {
			t.Helper()
			taskID := seedPgTask(t, h, orgID, userID)
			conv := firePgStep(t, h, stores, orgID, bpID, taskID, domain.Conversation{
				PromptID: promptID, CreatorUserID: userID,
			})
			return conv.ID, taskID
		},
		SetLease: func(t *testing.T, claimID string, in time.Duration) {
			t.Helper()
			if _, err := h.AdminDB.Exec(
				`UPDATE claims SET lease_expires_at = statement_timestamp() + make_interval(secs => $1) WHERE id = $2`,
				in.Seconds(), claimID,
			); err != nil {
				t.Fatalf("stage lease on %s: %v", claimID, err)
			}
		},
		Lease: func(t *testing.T, claimID string) (time.Time, time.Time, bool) {
			t.Helper()
			var expiry sql.NullTime
			var now time.Time
			if err := h.AdminDB.QueryRow(
				`SELECT lease_expires_at, statement_timestamp() FROM claims WHERE id = $1`, claimID,
			).Scan(&expiry, &now); err != nil {
				t.Fatalf("read lease of %s: %v", claimID, err)
			}
			return expiry.Time, now, expiry.Valid
		},
		LiveClaimsWithoutLease: func(t *testing.T) int {
			t.Helper()
			var n int
			if err := h.AdminDB.QueryRow(
				`SELECT COUNT(*) FROM claims WHERE released_at IS NULL AND lease_expires_at IS NULL`,
			).Scan(&n); err != nil {
				t.Fatalf("count live claims without a lease: %v", err)
			}
			return n
		},
		StageStaleStopIntent: func(t *testing.T, conversationID, status, by string) {
			t.Helper()
			if _, err := h.AdminDB.Exec(
				`UPDATE conversations SET status = $1, stop_requested_at = now(), stop_requested_by = NULLIF($2, '') WHERE id = $3`,
				status, by, conversationID,
			); err != nil {
				t.Fatalf("stage stale stop intent on %s: %v", conversationID, err)
			}
		},
		SetStoredStatus: func(t *testing.T, conversationID, status string) {
			t.Helper()
			pgtest.MustExec(t, h.AdminDB, `UPDATE conversations SET status = NULLIF($1, '') WHERE id = $2`, status, conversationID)
		},
		BackdateConclusion: func(t *testing.T, conversationID string, ago time.Duration) {
			t.Helper()
			pgtest.MustExec(t, h.AdminDB,
				`UPDATE conversations SET completed_at = now() - make_interval(secs => $1) WHERE id = $2`,
				ago.Seconds(), conversationID)
			pgtest.MustExec(t, h.AdminDB,
				`UPDATE claims SET released_at = now() - make_interval(secs => $1) WHERE conversation_id = $2 AND released_at IS NOT NULL`,
				ago.Seconds(), conversationID)
		},
	}
}

// TestClaimFence_Postgres_ReadsFreshDatabaseTime pins the one property only
// this dialect can get wrong: the fence's expiry guard reads
// statement_timestamp() rather than now(), which is the transaction's start. A claim whose lease
// lapses WHILE a long transaction is open must fail that transaction's writes,
// and against a transaction-start reading it would pass them.
func TestClaimFence_Postgres_ReadsFreshDatabaseTime(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	orgID, userID := seedPgOrgForBlueprints(t, h)
	bpID, taskID, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)
	conv := firePgStep(t, h, stores, orgID, bpID, taskID, domain.Conversation{
		PromptID: promptID, CreatorUserID: userID,
	})

	claimed, err := stores.ConversationQueue.ClaimNextConversation(ctx, "fresh-time-exec", 1, db.ClaimPlacement{}, db.DefaultClaimLease)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNextConversation = (%+v, %v)", claimed, err)
	}
	if _, err := h.AdminDB.Exec(
		`UPDATE claims SET lease_expires_at = statement_timestamp() + interval '1 second' WHERE id = $1`, claimed.ClaimID,
	); err != nil {
		t.Fatalf("stage a lease about to lapse: %v", err)
	}

	// The sleep runs inside the transaction, so now() is frozen a second and
	// a half before the guard reads. Only a fresh reading refuses this.
	tx, err := h.AdminDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_sleep(1.5)`); err != nil {
		t.Fatalf("sleep inside the transaction: %v", err)
	}
	if err := pgstore.AssertClaimActiveInTx(ctx, tx, orgID, conv.ID, claimed.ClaimID); !errors.Is(err, db.ErrClaimReleased) {
		t.Fatalf("the fence admitted a lapsed lease inside a long transaction (err %v); it is reading transaction-start time", err)
	}

	// And the store's own fenced write, on its own connection, agrees.
	if _, err := stores.Conversations.SetSessionForClaimSystem(ctx, orgID, conv.ID, claimed.ClaimID, "sess-late"); !errors.Is(err, db.ErrClaimReleased) {
		t.Fatalf("SetSessionForClaimSystem after the lease lapsed = %v, want ErrClaimReleased", err)
	}
}

// TestSettleUnclaimedStops_Postgres_NoDeadlockAgainstARunsTerminal pins the
// lock order the settlement shares with a run's terminal write. The terminal
// (markBlueprintRunStatus) locks the run and then parks its children; a
// settlement that locked a stop-pending child first and then wrote its
// cancel-requested run would wait on the terminal while the terminal waited on
// the child, and Postgres would kill one of them with 40P01. Both now take the
// run first, so racing them many times produces no deadlock and no error.
func TestSettleUnclaimedStops_Postgres_NoDeadlockAgainstARunsTerminal(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	orgID, userID := seedPgOrgForBlueprints(t, h)
	bpID, _, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)

	const iterations = 50
	for i := 0; i < iterations; i++ {
		taskID := seedPgTask(t, h, orgID, userID)
		conv := firePgStep(t, h, stores, orgID, bpID, taskID, domain.Conversation{
			PromptID: promptID, CreatorUserID: userID,
		})
		if _, err := stores.Blueprints.RequestRunCancelSystem(ctx, orgID, conv.BlueprintRunID); err != nil {
			t.Fatalf("iteration %d: RequestRunCancelSystem: %v", i, err)
		}
		if ok, err := stores.Conversations.RequestStopSystem(ctx, orgID, conv.ID, userID, ""); err != nil || !ok {
			t.Fatalf("iteration %d: RequestStopSystem = (%v, %v)", i, ok, err)
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		var markErr, settleErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, markErr = stores.Blueprints.MarkRunStatusSystem(ctx, orgID, conv.BlueprintRunID, domain.BlueprintRunStatusFailed, "raced", nil)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, settleErr = stores.ConversationQueue.SettleUnclaimedStopsSystem(ctx)
		}()
		close(start)
		wg.Wait()

		for name, err := range map[string]error{"MarkRunStatusSystem": markErr, "SettleUnclaimedStopsSystem": settleErr} {
			if err == nil {
				continue
			}
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "40P01" {
				t.Fatalf("iteration %d: %s deadlocked against the other writer: %v", i, name, err)
			}
			t.Fatalf("iteration %d: %s: %v", i, name, err)
		}

		// Exactly one of the two ended the run, and either way the child is
		// parked with its intent cleared: the loser found nothing to do.
		br, err := stores.Blueprints.GetRunSystem(ctx, orgID, conv.BlueprintRunID)
		if err != nil || br == nil {
			t.Fatalf("iteration %d: GetRunSystem = (%+v, %v)", i, br, err)
		}
		if br.Status != domain.BlueprintRunStatusFailed && br.Status != domain.BlueprintRunStatusCancelled {
			t.Fatalf("iteration %d: run status = %q, want failed or cancelled", i, br.Status)
		}
		got, err := stores.Conversations.GetSystem(ctx, orgID, conv.ID)
		if err != nil || got == nil {
			t.Fatalf("iteration %d: GetSystem = (%+v, %v)", i, got, err)
		}
		if got.Status != domain.StatusOpen {
			t.Fatalf("iteration %d: child status = %q, want open", i, got.Status)
		}
	}
}
