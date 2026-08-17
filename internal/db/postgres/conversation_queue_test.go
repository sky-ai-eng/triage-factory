package postgres_test

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

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

// TestConversationQueueStore_Postgres_EnqueueClaim exercises the basic enqueue → claim →
// requeue → reset cycle against real Postgres (admin pool).
func TestConversationQueueStore_Postgres_EnqueueClaim(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID := seedPgOrgForBlueprints(t, h)
	brID, taskID, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)

	// Empty queue.
	if got, err := stores.ConversationQueue.ClaimNextConversation(ctx, pgConversationQueueExecutorID, pgConversationQueueBootEpoch, db.ClaimPlacement{}); err != nil || got != nil {
		t.Fatalf("ClaimNextConversation on empty queue = (%v, %v), want (nil, nil)", got, err)
	}

	conversationID := uuid.New().String()
	step0 := 0
	if err := stores.ConversationQueue.EnqueueConversation(ctx, orgID, domain.Conversation{
		ID: conversationID, TaskID: taskID, PromptID: promptID, Model: "m",
		TriggerType: "manual", CreatorUserID: userID, BlueprintRunID: brID, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("EnqueueConversation: %v", err)
	}

	got, err := stores.ConversationQueue.ClaimNextConversation(ctx, pgConversationQueueExecutorID, pgConversationQueueBootEpoch, db.ClaimPlacement{})
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
	// team_id rides back on the claim (TFAC-458) and matches the value
	// EnqueueConversation derived from the parent task — this is the construction-path
	// ConversationInfo.TeamID source the capture writers attribute artifacts by.
	var wantTeam string
	if err := h.AdminDB.QueryRow(`SELECT team_id::text FROM tasks WHERE id = $1`, taskID).Scan(&wantTeam); err != nil {
		t.Fatalf("read task team_id: %v", err)
	}
	if got.TeamID == "" || got.TeamID != wantTeam {
		t.Fatalf("claimed team_id = %q, want %q (task-derived)", got.TeamID, wantTeam)
	}

	// Requeue → re-claimable, attempts retained.
	if err := stores.ConversationQueue.RequeueConversation(ctx, orgID, conversationID, "transient"); err != nil {
		t.Fatalf("RequeueConversation: %v", err)
	}
	got2, err := stores.ConversationQueue.ClaimNextConversation(ctx, pgConversationQueueExecutorID, pgConversationQueueBootEpoch, db.ClaimPlacement{})
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

// TestConversationQueueStore_Postgres_ResetProcessingRuns_ScopedToOwner is the core
// TFAC-578 hazard test: two processes (distinct persistent executor
// identities) against one Postgres. Process B booting must NOT reset process
// A's claimed/running row — A's work would otherwise be re-queued and
// re-executed by A while it's still live, a duplicate-execution hazard on a
// rolling deploy or any N>1 topology.
func TestConversationQueueStore_Postgres_ResetProcessingRuns_ScopedToOwner(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID := seedPgOrgForBlueprints(t, h)
	brID, taskID, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)
	step0 := 0
	conversationID := uuid.New().String()
	if err := stores.ConversationQueue.EnqueueConversation(ctx, orgID, domain.Conversation{
		ID: conversationID, TaskID: taskID, PromptID: promptID, Model: "m",
		TriggerType: "manual", CreatorUserID: userID, BlueprintRunID: brID, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("EnqueueConversation: %v", err)
	}

	// Process A claims and is still live (never crashed).
	claimed, err := stores.ConversationQueue.ClaimNextConversation(ctx, "process-a", 1, db.ClaimPlacement{})
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
		t.Errorf("run status = %q, want none (untouched by process-b's boot reset)", st)
	}
	if !pgHasActiveClaim(t, h, conversationID) {
		t.Error("process-b's boot reset released process-a's live claim")
	}

	// A itself restarting (a later boot epoch of the SAME executor_id) DOES
	// sweep its own orphan — this is the "kill -9 mid-run, restart" path.
	n2, err := stores.ConversationQueue.ResetProcessingConversations(ctx, "process-a", 2)
	if err != nil {
		t.Fatalf("process-a restart ResetProcessingConversations: %v", err)
	}
	if n2 != 1 {
		t.Errorf("process-a's restart reset %d rows, want 1 (its own prior-boot orphan)", n2)
	}
	if st, _ := pgConversationStatus(t, h, conversationID); st != "" {
		t.Errorf("run status = %q, want none after process-a's own restart swept it", st)
	}
	if pgHasActiveClaim(t, h, conversationID) {
		t.Error("process-a's restart left its own prior-boot claim active")
	}
}

// TestConversationQueueStore_Postgres_ResetProcessingRuns_NeverResetsCurrentEpoch pins
// that a boot's own reconcile never touches rows claimed under its OWN
// current epoch — only strictly earlier boots of itself are orphans. This is
// what keeps ResetProcessingConversations safe to call unconditionally at every boot.
func TestConversationQueueStore_Postgres_ResetProcessingRuns_NeverResetsCurrentEpoch(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID := seedPgOrgForBlueprints(t, h)
	brID, taskID, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)
	step0 := 0
	conversationID := uuid.New().String()
	if err := stores.ConversationQueue.EnqueueConversation(ctx, orgID, domain.Conversation{
		ID: conversationID, TaskID: taskID, PromptID: promptID, Model: "m",
		TriggerType: "manual", CreatorUserID: userID, BlueprintRunID: brID, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("EnqueueConversation: %v", err)
	}

	if _, err := stores.ConversationQueue.ClaimNextConversation(ctx, "process-self", 5, db.ClaimPlacement{}); err != nil {
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
	brID, taskID, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)

	step0 := 0
	if err := stores.ConversationQueue.EnqueueConversation(ctx, orgID, domain.Conversation{
		ID: uuid.New().String(), TaskID: taskID, PromptID: promptID, Model: "m",
		TriggerType: "manual", CreatorUserID: userID, BlueprintRunID: brID, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("EnqueueConversation: %v", err)
	}
	if changed, err := stores.Blueprints.RequestRunCancelSystem(ctx, orgID, brID); err != nil || !changed {
		t.Fatalf("RequestRunCancelSystem = (%v, %v)", changed, err)
	}
	if got, err := stores.ConversationQueue.ClaimNextConversation(ctx, pgConversationQueueExecutorID, pgConversationQueueBootEpoch, db.ClaimPlacement{}); err != nil || got != nil {
		t.Fatalf("ClaimNextConversation on cancel-requested blueprint = (%v, %v), want (nil, nil)", got, err)
	}
}

// TestConversationQueueStore_Postgres_ConcurrentClaim proves the FOR UPDATE SKIP LOCKED
// claim never hands the same queued run to two claimers: it enqueues N runs and
// drains them from G goroutines, asserting every run is claimed exactly once.
func TestConversationQueueStore_Postgres_ConcurrentClaim(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID := seedPgOrgForBlueprints(t, h)
	brID, taskID, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)
	bpID := pgBlueprintIDOfRun(t, h, brID)

	// N runs queued at once = N blueprint_runs, each on its own step 0.
	const n = 40
	want := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		conversationID := uuid.New().String()
		step0 := 0
		if err := stores.ConversationQueue.EnqueueConversation(ctx, orgID, domain.Conversation{
			ID: conversationID, TaskID: taskID, PromptID: promptID, Model: "m",
			TriggerType: "manual", CreatorUserID: userID,
			BlueprintRunID:     seedPgBlueprintRunOn(t, h, orgID, userID, bpID, taskID),
			BlueprintStepIndex: &step0,
		}); err != nil {
			t.Fatalf("EnqueueConversation %d: %v", i, err)
		}
		want[conversationID] = true
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
				conv, err := stores.ConversationQueue.ClaimNextConversation(ctx, pgConversationQueueExecutorID, pgConversationQueueBootEpoch, db.ClaimPlacement{})
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
		t.Fatalf("claimed %d distinct runs, want %d", len(claimed), n)
	}
	for id, c := range claimed {
		if c != 1 {
			t.Errorf("run %s claimed %d times (double-claim)", id, c)
		}
		if !want[id] {
			t.Errorf("claimed unknown run %s", id)
		}
	}
}

// TestConversationQueueStore_Postgres_ReconcileOrphanedRuns heals the desync where a
// child run is left non-terminal under an already-terminal blueprint_run: the
// boot sweep cancels it (stamping completed_at) and only it, leaving a child
// under a still-running parent untouched.
func TestConversationQueueStore_Postgres_ReconcileOrphanedRuns(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID := seedPgOrgForBlueprints(t, h)

	// Orphan: terminal (cancelled) parent, child still running.
	brA, taskA, promptA := seedPgConversationQueueFixture(t, h, orgID, userID)
	orphanID := uuid.New().String()
	step0 := 0
	if err := stores.ConversationQueue.EnqueueConversation(ctx, orgID, domain.Conversation{
		ID: orphanID, TaskID: taskA, PromptID: promptA, Model: "m",
		TriggerType: "manual", CreatorUserID: userID, BlueprintRunID: brA, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("EnqueueConversation orphan: %v", err)
	}
	if _, err := h.AdminDB.Exec(`UPDATE conversations SET status = NULL WHERE id = $1`, orphanID); err != nil {
		t.Fatalf("set orphan running: %v", err)
	}
	// A second mid-flight orphan under the same terminal parent that no claim
	// ever picked up must also be parked — a claimable step under a non-running
	// parent is never actually claimed, so it would sit in the queue forever.
	queuedOrphanID := uuid.New().String()
	if err := stores.ConversationQueue.EnqueueConversation(ctx, orgID, domain.Conversation{
		ID: queuedOrphanID, TaskID: taskA, PromptID: promptA, Model: "m",
		TriggerType: "manual", CreatorUserID: userID, BlueprintRunID: brA, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("EnqueueConversation queued orphan: %v", err)
	}
	if _, err := h.AdminDB.Exec(`UPDATE blueprint_runs SET status = 'cancelled', cancel_requested = false WHERE id = $1`, brA); err != nil {
		t.Fatalf("set parent cancelled: %v", err)
	}

	// Healthy: running parent, child running — must be left alone.
	brB, taskB, promptB := seedPgConversationQueueFixture(t, h, orgID, userID)
	healthyID := uuid.New().String()
	if err := stores.ConversationQueue.EnqueueConversation(ctx, orgID, domain.Conversation{
		ID: healthyID, TaskID: taskB, PromptID: promptB, Model: "m",
		TriggerType: "manual", CreatorUserID: userID, BlueprintRunID: brB, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("EnqueueConversation healthy: %v", err)
	}
	if _, err := h.AdminDB.Exec(`UPDATE conversations SET status = NULL WHERE id = $1`, healthyID); err != nil {
		t.Fatalf("set healthy running: %v", err)
	}
	// A genuinely running child holds an active claim (ClaimNextConversation mints
	// it); without one, the claim-desync requeue arm would rightly treat the
	// row as stranded.
	if _, err := h.AdminDB.Exec(`
		INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch)
		VALUES ($1, $2, $3, 'exec-healthy', 1)
	`, uuid.New().String(), orgID, healthyID); err != nil {
		t.Fatalf("seed healthy claim: %v", err)
	}

	n, err := stores.ConversationQueue.ReconcileOrphanedConversations(ctx)
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
		t.Errorf("healthy run status = %q, want none (must not touch a mid-flight child under a running parent)", st)
	}

	// Idempotent: a second sweep finds nothing.
	if n2, err := stores.ConversationQueue.ReconcileOrphanedConversations(ctx); err != nil || n2 != 0 {
		t.Errorf("second ReconcileOrphanedConversations = (%d, %v), want (0, nil)", n2, err)
	}
}

// TestConversationQueueStore_Postgres_ReconcileHealsClaimDesyncs pins the janitor arms
// for the two shapes the non-atomic app-pool terminal writes can strand: a
// terminal conversation with a dangling active claim gets the claim released
// (outcome mapped from status), and an in-flight
// delegation conversation with no active claim under a running parent goes
// back to 'queued' with its placement stamp cleared — while a running row
// with a live claim and a claimless queued row are untouched.
func TestConversationQueueStore_Postgres_ReconcileHealsClaimDesyncs(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID := seedPgOrgForBlueprints(t, h)
	brID, taskID, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)
	step0 := 0

	seedChild := func(status string) string {
		t.Helper()
		id := uuid.New().String()
		if err := stores.ConversationQueue.EnqueueConversation(ctx, orgID, domain.Conversation{
			ID: id, TaskID: taskID, PromptID: promptID, Model: "m",
			TriggerType: "manual", CreatorUserID: userID, BlueprintRunID: brID, BlueprintStepIndex: &step0,
		}); err != nil {
			t.Fatalf("EnqueueConversation: %v", err)
		}
		if status != "" {
			pgtest.MustExec(t, h.AdminDB, `UPDATE conversations SET status = $2 WHERE id = $1`, id, status)
		}
		return id
	}
	activeClaim := func(convID string) string {
		t.Helper()
		claimID := uuid.New().String()
		pgtest.MustExec(t, h.AdminDB, `
			INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch)
			VALUES ($1, $2, $3, 'exec-ds', 1)
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

	n, err := stores.ConversationQueue.ReconcileOrphanedConversations(ctx)
	if err != nil {
		t.Fatalf("ReconcileOrphanedConversations: %v", err)
	}
	if n != 2 {
		t.Errorf("healed count = %d, want 2 (two released claims)", n)
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
	if rel, out := claimState(doneClaim); !rel || out != "completed" {
		t.Errorf("completed row's claim = (released=%v, outcome=%q), want (true, completed)", rel, out)
	}
	if rel, out := claimState(failedClaim); !rel || out != "failed" {
		t.Errorf("failed row's claim = (released=%v, outcome=%q), want (true, failed)", rel, out)
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

	// Idempotent: a second sweep finds nothing.
	if n2, err := stores.ConversationQueue.ReconcileOrphanedConversations(ctx); err != nil || n2 != 0 {
		t.Errorf("second sweep = (%d, %v), want (0, nil)", n2, err)
	}
}

// TestConversationQueueStore_Postgres_EnqueueStampsActorAgent is the Postgres parity of
// the SQLite actor-stamp test: EnqueueConversation (both the manual and event branches)
// persists conversations.actor_agent_id, and ConversationStore.GetSystem
// JOINs agents to surface the display name as ActorAgentName. A run with
// no actor reads back with both fields empty.
func TestConversationQueueStore_Postgres_EnqueueStampsActorAgent(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID := seedPgOrgForBlueprints(t, h)
	brID, taskID, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)

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
	manualID := uuid.New().String()
	step0 := 0
	if err := stores.ConversationQueue.EnqueueConversation(ctx, orgID, domain.Conversation{
		ID: manualID, TaskID: taskID, PromptID: promptID, Model: "m",
		TriggerType: "manual", CreatorUserID: userID, ActorAgentID: agentID,
		BlueprintRunID: brID, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("EnqueueConversation (manual): %v", err)
	}
	got, err := stores.Conversations.GetSystem(ctx, orgID, manualID)
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
	eventID := uuid.New().String()
	step1 := 1
	if err := stores.ConversationQueue.EnqueueConversation(ctx, orgID, domain.Conversation{
		ID: eventID, TaskID: taskID, PromptID: promptID, Model: "m",
		TriggerType: "event", ActorAgentID: agentID,
		BlueprintRunID: brID, BlueprintStepIndex: &step1,
	}); err != nil {
		t.Fatalf("EnqueueConversation (event): %v", err)
	}
	ev, err := stores.Conversations.GetSystem(ctx, orgID, eventID)
	if err != nil || ev == nil {
		t.Fatalf("GetSystem (event): (%v, %v)", ev, err)
	}
	if ev.ActorAgentID != agentID || ev.ActorAgentName != "Triage Bot" {
		t.Errorf("event run actor = (%q, %q), want (%q, Triage Bot)", ev.ActorAgentID, ev.ActorAgentName, agentID)
	}

	// No actor → both fields empty (nullable column + LEFT JOIN).
	bareID := uuid.New().String()
	step2 := 2
	if err := stores.ConversationQueue.EnqueueConversation(ctx, orgID, domain.Conversation{
		ID: bareID, TaskID: taskID, PromptID: promptID, Model: "m",
		TriggerType: "manual", CreatorUserID: userID,
		BlueprintRunID: brID, BlueprintStepIndex: &step2,
	}); err != nil {
		t.Fatalf("EnqueueConversation (no actor): %v", err)
	}
	bare, err := stores.Conversations.GetSystem(ctx, orgID, bareID)
	if err != nil || bare == nil {
		t.Fatalf("GetSystem (no actor): (%v, %v)", bare, err)
	}
	if bare.ActorAgentID != "" || bare.ActorAgentName != "" {
		t.Errorf("no-actor run = (%q, %q), want both empty", bare.ActorAgentID, bare.ActorAgentName)
	}
}

// TestConversationQueueStore_Postgres_EnqueueStampsTheNativeEngine pins the single fact
// that makes the SDK engine unreachable for a delegation in this mode: the
// mint names the engine, so no row is ever written that a claimant would have
// to be taught to refuse. There is no caller-passed knob and no reliance on
// the column DEFAULT, which still reads 'sdk' for the curator's sake.
//
// Both arms, because they are separate statements: a stamp added to one and
// forgotten in the other is exactly the drift this catches, and the arm that
// forgot would take the DEFAULT silently.
//
// It reads the stored column rather than the projection on purpose. The
// question is what the write put there, and a projection that COALESCEd an
// absent value would answer it wrong.
func TestConversationQueueStore_Postgres_EnqueueStampsTheNativeEngine(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID := seedPgOrgForBlueprints(t, h)
	brID, taskID, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)

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

	for i, trigger := range []string{"manual", "event"} {
		step := i
		convID := uuid.New().String()
		conv := domain.Conversation{
			ID: convID, TaskID: taskID, PromptID: promptID, Model: "m",
			TriggerType: trigger, BlueprintRunID: brID, BlueprintStepIndex: &step,
		}
		if trigger == "manual" {
			// The schema CHECK pairs a manual trigger with a creator.
			conv.CreatorUserID = userID
		}
		if err := stores.ConversationQueue.EnqueueConversation(ctx, orgID, conv); err != nil {
			t.Fatalf("EnqueueConversation (%s): %v", trigger, err)
		}
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
	ctx := context.Background()

	dbtest.RunClaimCredentialsConformance(t, func(t *testing.T) (db.ConversationQueueStore, string, dbtest.ClaimCredentialsSeeder) {
		t.Helper()
		h.Reset(t)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
		orgID, userID := seedPgOrgForBlueprints(t, h)
		brID, taskID, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)

		nextStep := 0
		seed := dbtest.ClaimCredentialsSeeder{
			EnqueueConversation: func(t *testing.T) string {
				t.Helper()
				idx := nextStep
				nextStep++
				conversationID := uuid.New().String()
				if err := stores.ConversationQueue.EnqueueConversation(ctx, orgID, domain.Conversation{
					ID: conversationID, TaskID: taskID, PromptID: promptID, Model: "m",
					TriggerType: "manual", CreatorUserID: userID, BlueprintRunID: brID, BlueprintStepIndex: &idx,
				}); err != nil {
					t.Fatalf("EnqueueConversation: %v", err)
				}
				return conversationID
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
	ctx := context.Background()

	dbtest.RunFleetQueueSharesConformance(t, func(t *testing.T) (db.ConversationQueueStore, string, dbtest.FleetQueueSharesSeeder) {
		t.Helper()
		h.Reset(t)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
		orgID, userID := seedPgOrgForBlueprints(t, h)

		seed := dbtest.FleetQueueSharesSeeder{
			// One blueprint_run per staged run — the real firing model (one
			// delegation = one blueprint_run), and what makes several queued
			// rows concurrently claimable: a blueprint drives its current
			// step and no other, so siblings under one blueprint could never
			// all be queued at once.
			EnqueueConversation: func(t *testing.T) string {
				t.Helper()
				brID, taskID, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)
				conversationID := uuid.New().String()
				step := 0
				if err := stores.ConversationQueue.EnqueueConversation(ctx, orgID, domain.Conversation{
					ID: conversationID, TaskID: taskID, PromptID: promptID, Model: "m",
					TriggerType: "manual", CreatorUserID: userID, BlueprintRunID: brID, BlueprintStepIndex: &step,
				}); err != nil {
					t.Fatalf("EnqueueConversation: %v", err)
				}
				return conversationID
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

// pgConversationStatus reads a run's STORED status (SQL NULL — the mid-flight state
// — as "") and whether completed_at is stamped.
func pgConversationStatus(t *testing.T, h *pgtest.Harness, conversationID string) (status string, completed bool) {
	t.Helper()
	var stored sql.NullString
	var completedAt *string
	if err := h.AdminDB.QueryRow(`SELECT status, completed_at::text FROM conversations WHERE id = $1`, conversationID).
		Scan(&stored, &completedAt); err != nil {
		t.Fatalf("read run %s: %v", conversationID, err)
	}
	return stored.String, completedAt != nil
}

// pgConversationParked is pgConversationStatus's park-side sibling: the stored status plus
// whether parked_at is stamped, which is what the snapshot-retention sweep
// keys a parked run off.
func pgConversationParked(t *testing.T, h *pgtest.Harness, conversationID string) (status string, parked bool) {
	t.Helper()
	var stored sql.NullString
	var parkedAt *string
	if err := h.AdminDB.QueryRow(`SELECT status, parked_at::text FROM conversations WHERE id = $1`, conversationID).
		Scan(&stored, &parkedAt); err != nil {
		t.Fatalf("read run %s: %v", conversationID, err)
	}
	return stored.String, parkedAt != nil
}

// seedPgConversationQueueFixture mints a prompt + blueprint + a running blueprint_run on
// a fresh task, returning (blueprintRunID, taskID, promptID) ready for enqueue.
func seedPgConversationQueueFixture(t *testing.T, h *pgtest.Harness, orgID, userID string) (brID, taskID, promptID string) {
	t.Helper()
	bpID := "rq-bp-" + uuid.New().String()[:8]
	seedPgBlueprint(t, h, orgID, userID, bpID)
	promptID = "rq-p-" + uuid.New().String()[:8]
	seedPgPrompt(t, h, orgID, userID, promptID)
	taskID = seedPgTask(t, h, orgID, userID)
	return seedPgBlueprintRunOn(t, h, orgID, userID, bpID, taskID), taskID, promptID
}

// seedPgBlueprintRunOn mints one more running blueprint_run against an existing
// blueprint + task. A fixture staging several CONCURRENTLY queued runs needs
// one of these per run: a blueprint drives the single step its
// current_step_index names, so sibling steps of one blueprint are never
// claimable at the same moment — which is the real firing model anyway (one
// delegation = one blueprint_run).
func seedPgBlueprintRunOn(t *testing.T, h *pgtest.Harness, orgID, userID, bpID, taskID string) string {
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

// pgBlueprintIDOfRun reads the blueprint a blueprint_run was minted from, so a
// fixture holding only a run id can stage more runs beside it.
func pgBlueprintIDOfRun(t *testing.T, h *pgtest.Harness, brID string) string {
	t.Helper()
	var bpID string
	if err := h.AdminDB.QueryRow(`SELECT blueprint_id FROM blueprint_runs WHERE id = $1`, brID).Scan(&bpID); err != nil {
		t.Fatalf("read blueprint_id of %s: %v", brID, err)
	}
	return bpID
}

// TestConversationQueueStore_Postgres_QueuedAtStamps mirrors the SQLite twin: enqueue
// stamps queued_at, a claim stamps claimed_at (both surfaced through
// Conversations.GetSystem), and a requeue re-stamps queued_at and clears
// claimed_at so the next dwell measures from the re-entry, not the mint.
func TestConversationQueueStore_Postgres_QueuedAtStamps(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID := seedPgOrgForBlueprints(t, h)
	brID, taskID, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)

	conversationID := uuid.New().String()
	step0 := 0
	if err := stores.ConversationQueue.EnqueueConversation(ctx, orgID, domain.Conversation{
		ID: conversationID, TaskID: taskID, PromptID: promptID, Model: "m",
		TriggerType: "manual", CreatorUserID: userID, BlueprintRunID: brID, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("EnqueueConversation: %v", err)
	}

	queued, err := stores.Conversations.GetSystem(ctx, orgID, conversationID)
	if err != nil || queued == nil {
		t.Fatalf("GetSystem after enqueue: (%v, %v)", queued, err)
	}
	if queued.QueuedAt == nil {
		t.Fatal("QueuedAt = nil after enqueue; the enqueue must stamp queue entry")
	}
	if queued.ClaimedAt != nil {
		t.Fatalf("ClaimedAt = %v on a queued run, want nil", queued.ClaimedAt)
	}
	firstQueuedAt := *queued.QueuedAt

	if got, err := stores.ConversationQueue.ClaimNextConversation(ctx, pgConversationQueueExecutorID, pgConversationQueueBootEpoch, db.ClaimPlacement{}); err != nil || got == nil {
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

	if err := stores.ConversationQueue.RequeueConversation(ctx, orgID, conversationID, "transient setup error"); err != nil {
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
// no matter which setup phase the run's active claim is in: setup progress
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
			brID, taskID, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)

			conversationID := uuid.New().String()
			step0 := 0
			if err := stores.ConversationQueue.EnqueueConversation(ctx, orgID, domain.Conversation{
				ID: conversationID, TaskID: taskID, PromptID: promptID, Model: "m",
				TriggerType: "manual", CreatorUserID: userID, BlueprintRunID: brID, BlueprintStepIndex: &step0,
			}); err != nil {
				t.Fatalf("EnqueueConversation: %v", err)
			}
			if got, err := stores.ConversationQueue.ClaimNextConversation(ctx, pgConversationQueueExecutorID, pgConversationQueueBootEpoch, db.ClaimPlacement{}); err != nil || got == nil {
				t.Fatalf("ClaimNextConversation: (%v, %v)", got, err)
			}
			// Advance the claim into the setup phase the dispatcher would
			// have recorded before the workspace-setup failure fired the
			// requeue.
			if err := stores.Conversations.SetActiveClaimPhaseSystem(ctx, orgID, conversationID, phase); err != nil {
				t.Fatalf("SetActiveClaimPhaseSystem(%s): %v", phase, err)
			}

			if err := stores.ConversationQueue.RequeueConversation(ctx, orgID, conversationID, "workspace setup: boom"); err != nil {
				t.Fatalf("RequeueConversation: %v", err)
			}
			after, err := stores.Conversations.GetSystem(ctx, orgID, conversationID)
			if err != nil || after == nil {
				t.Fatalf("GetSystem after requeue: (%v, %v)", after, err)
			}
			if after.Status != "queued" {
				t.Fatalf("status after requeue from phase %q = %q, want queued (a mid-setup phase must not block the requeue)", phase, after.Status)
			}
			if reclaimed, err := stores.ConversationQueue.ClaimNextConversation(ctx, pgConversationQueueExecutorID, pgConversationQueueBootEpoch, db.ClaimPlacement{}); err != nil || reclaimed == nil {
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
	ctx := context.Background()

	dbtest.RunExecutorClaimsConformance(t, func(t *testing.T) (db.ConversationQueueStore, dbtest.ExecutorClaimsSeeder) {
		t.Helper()
		h.Reset(t)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
		orgID, userID := seedPgOrgForBlueprints(t, h)
		brID, taskID, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)

		nextStep := 0
		seed := dbtest.ExecutorClaimsSeeder{
			Conversation: func(t *testing.T, status, failureKind string) string {
				t.Helper()
				idx := nextStep
				nextStep++
				conversationID := uuid.New().String()
				if err := stores.ConversationQueue.EnqueueConversation(ctx, orgID, domain.Conversation{
					ID: conversationID, TaskID: taskID, PromptID: promptID, Model: "m",
					TriggerType: "manual", CreatorUserID: userID,
					BlueprintRunID: brID, BlueprintStepIndex: &idx,
				}); err != nil {
					t.Fatalf("EnqueueConversation: %v", err)
				}
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
					                    claimed_at, released_at, outcome, peak_mem_mb, cpu_usec)
					VALUES ($1, $2, $3, $4, 1, $5, $6, NULLIF($7, ''), $8, $9)
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
		brID, taskID, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)

		nextStep := 0
		return dbtest.ClaimPredicateHarness{
			Stores: stores,
			OrgID:  orgID,
			UserID: userID,
			EnqueueDelegation: func(t *testing.T, runtime string) string {
				t.Helper()
				idx := nextStep
				nextStep++
				// Pointer first, then the row it names — the order the
				// reactor writes in, and the reason the claim gate can be a
				// plain equality.
				pgtest.MustExec(t, h.AdminDB, `UPDATE blueprint_runs SET current_step_index = $2 WHERE id = $1`, brID, idx)
				convID := uuid.New().String()
				if err := stores.ConversationQueue.EnqueueConversation(ctx, orgID, domain.Conversation{
					ID: convID, TaskID: taskID, PromptID: promptID, Model: "m",
					TriggerType: "manual", CreatorUserID: userID, BlueprintRunID: brID, BlueprintStepIndex: &idx,
				}); err != nil {
					t.Fatalf("EnqueueConversation: %v", err)
				}
				// The dialect stamps its own runtime at mint; rewrite it so
				// one backend covers both engines.
				pgtest.MustExec(t, h.AdminDB, `UPDATE conversations SET runtime = $2 WHERE id = $1`, convID, runtime)
				return convID
			},
			EnqueueUnindexed: func(t *testing.T) error {
				t.Helper()
				return stores.ConversationQueue.EnqueueConversation(ctx, orgID, domain.Conversation{
					ID: uuid.New().String(), TaskID: taskID, PromptID: promptID, Model: "m",
					TriggerType: "manual", CreatorUserID: userID, BlueprintRunID: brID,
				})
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
				pgtest.MustExec(t, h.AdminDB,
					`UPDATE blueprint_runs SET status = $2, current_step_index = $3 WHERE id = $1`,
					brID, status, currentStepIndex)
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

// TestConversationQueueStore_Postgres_ReconcileOrphanedRunsConformance runs the shared
// boot-reconcile conformance against the Postgres impl (admin pool, matching
// production wiring). Each factory call resets the harness, so the suite's exact
// healed counts are this subtest's alone.
func TestConversationQueueStore_Postgres_ReconcileOrphanedRunsConformance(t *testing.T) {
	h := pgtest.Shared(t)
	ctx := context.Background()

	dbtest.RunReconcileOrphanedConversationsConformance(t, func(t *testing.T) (db.ConversationQueueStore, dbtest.ReconcileOrphanSeeder) {
		t.Helper()
		h.Reset(t)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
		orgID, userID := seedPgOrgForBlueprints(t, h)
		brID, taskID, promptID := seedPgConversationQueueFixture(t, h, orgID, userID)
		bpID := pgBlueprintIDOfRun(t, h, brID)

		nextStep := 0
		// The fixture already minted one running blueprint_run; hand that one
		// out first so the suite's exact counts aren't thrown off by a spare
		// childless orphan sitting beside the one it staged.
		spare := brID
		seed := dbtest.ReconcileOrphanSeeder{
			BlueprintRun: func(t *testing.T, age time.Duration) string {
				t.Helper()
				id := spare
				if id == "" {
					id = seedPgBlueprintRunOn(t, h, orgID, userID, bpID, taskID)
				}
				spare = ""
				if age > 0 {
					pgtest.MustExec(t, h.AdminDB,
						`UPDATE blueprint_runs SET started_at = now() - $2::interval WHERE id = $1`, id, age.String())
				}
				return id
			},
			EnqueueChild: func(t *testing.T, brID string) string {
				t.Helper()
				idx := nextStep
				nextStep++
				convID := uuid.New().String()
				if err := stores.ConversationQueue.EnqueueConversation(ctx, orgID, domain.Conversation{
					ID: convID, TaskID: taskID, PromptID: promptID, Model: "m",
					TriggerType: "manual", CreatorUserID: userID, BlueprintRunID: brID, BlueprintStepIndex: &idx,
				}); err != nil {
					t.Fatalf("EnqueueConversation: %v", err)
				}
				return convID
			},
			ForceBlueprintStatus: func(t *testing.T, brID, status, abortReason string) {
				t.Helper()
				pgtest.MustExec(t, h.AdminDB,
					`UPDATE blueprint_runs SET status = $2, abort_reason = NULLIF($3, '') WHERE id = $1`,
					brID, status, abortReason)
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
