package postgres_test

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// pgRunQueueExecutorID/pgRunQueueBootEpoch are the fixed ownership identity
// most claims in this file stamp a row with; tests specifically exercising
// the ownership-scoping predicate (TFAC-578) pass their own values inline.
const (
	pgRunQueueExecutorID = "test-executor"
	pgRunQueueBootEpoch  = int64(1)
)

// TestRunQueueStore_Postgres_EnqueueClaim exercises the basic enqueue → claim →
// requeue → reset cycle against real Postgres (admin pool).
func TestRunQueueStore_Postgres_EnqueueClaim(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID := seedPgOrgForBlueprints(t, h)
	brID, taskID, promptID := seedPgRunQueueFixture(t, h, orgID, userID)

	// Empty queue.
	if got, err := stores.RunQueue.ClaimNextRun(ctx, pgRunQueueExecutorID, pgRunQueueBootEpoch, db.ClaimPlacement{}); err != nil || got != nil {
		t.Fatalf("ClaimNextRun on empty queue = (%v, %v), want (nil, nil)", got, err)
	}

	runID := uuid.New().String()
	step0 := 0
	if err := stores.RunQueue.EnqueueRun(ctx, orgID, domain.AgentRun{
		ID: runID, TaskID: taskID, PromptID: promptID, Model: "m",
		TriggerType: "manual", CreatorUserID: userID, BlueprintRunID: brID, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}

	got, err := stores.RunQueue.ClaimNextRun(ctx, pgRunQueueExecutorID, pgRunQueueBootEpoch, db.ClaimPlacement{})
	if err != nil || got == nil {
		t.Fatalf("ClaimNextRun: (%v, %v)", got, err)
	}
	if got.ID != runID || got.Status != "running" || got.Attempts != 1 {
		t.Fatalf("claimed = %+v", got)
	}
	// team_id rides back on the claim (TFAC-458) and matches the value
	// EnqueueRun derived from the parent task — this is the construction-path
	// RunInfo.TeamID source the capture writers attribute artifacts by.
	var wantTeam string
	if err := h.AdminDB.QueryRow(`SELECT team_id::text FROM tasks WHERE id = $1`, taskID).Scan(&wantTeam); err != nil {
		t.Fatalf("read task team_id: %v", err)
	}
	if got.TeamID == "" || got.TeamID != wantTeam {
		t.Fatalf("claimed team_id = %q, want %q (task-derived)", got.TeamID, wantTeam)
	}

	// Requeue → re-claimable, attempts retained.
	if err := stores.RunQueue.RequeueRun(ctx, orgID, runID, "transient"); err != nil {
		t.Fatalf("RequeueRun: %v", err)
	}
	got2, err := stores.RunQueue.ClaimNextRun(ctx, pgRunQueueExecutorID, pgRunQueueBootEpoch, db.ClaimPlacement{})
	if err != nil || got2 == nil || got2.Attempts != 2 {
		t.Fatalf("re-claim = (%+v, %v), want attempts=2", got2, err)
	}

	// ResetProcessingRuns flips the mid-flight 'running' row back to queued —
	// a later boot epoch of the same executor (a restart) sweeps it.
	n, err := stores.RunQueue.ResetProcessingRuns(ctx, pgRunQueueExecutorID, pgRunQueueBootEpoch+1)
	if err != nil || n != 1 {
		t.Fatalf("ResetProcessingRuns = (%d, %v), want (1, nil)", n, err)
	}
}

// TestRunQueueStore_Postgres_ResetProcessingRuns_ScopedToOwner is the core
// TFAC-578 hazard test: two processes (distinct persistent executor
// identities) against one Postgres. Process B booting must NOT reset process
// A's claimed/running row — A's work would otherwise be re-queued and
// re-executed by A while it's still live, a duplicate-execution hazard on a
// rolling deploy or any N>1 topology.
func TestRunQueueStore_Postgres_ResetProcessingRuns_ScopedToOwner(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID := seedPgOrgForBlueprints(t, h)
	brID, taskID, promptID := seedPgRunQueueFixture(t, h, orgID, userID)
	step0 := 0
	runID := uuid.New().String()
	if err := stores.RunQueue.EnqueueRun(ctx, orgID, domain.AgentRun{
		ID: runID, TaskID: taskID, PromptID: promptID, Model: "m",
		TriggerType: "manual", CreatorUserID: userID, BlueprintRunID: brID, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}

	// Process A claims and is still live (never crashed).
	claimed, err := stores.RunQueue.ClaimNextRun(ctx, "process-a", 1, db.ClaimPlacement{})
	if err != nil || claimed == nil || claimed.ID != runID {
		t.Fatalf("process-a claim: got=%v err=%v", claimed, err)
	}

	// Process B boots (a distinct instance, not a restart of A) and runs its
	// own boot reconcile. A's row must be untouched — different executor_id.
	n, err := stores.RunQueue.ResetProcessingRuns(ctx, "process-b", 1)
	if err != nil {
		t.Fatalf("process-b ResetProcessingRuns: %v", err)
	}
	if n != 0 {
		t.Errorf("process-b's boot reset %d rows, want 0 (must not touch process-a's live claim)", n)
	}
	if st, _ := pgRunStatus(t, h, runID); st != "running" {
		t.Errorf("run status = %q, want running (untouched by process-b's boot reset)", st)
	}

	// A itself restarting (a later boot epoch of the SAME executor_id) DOES
	// sweep its own orphan — this is the "kill -9 mid-run, restart" path.
	n2, err := stores.RunQueue.ResetProcessingRuns(ctx, "process-a", 2)
	if err != nil {
		t.Fatalf("process-a restart ResetProcessingRuns: %v", err)
	}
	if n2 != 1 {
		t.Errorf("process-a's restart reset %d rows, want 1 (its own prior-boot orphan)", n2)
	}
	if st, _ := pgRunStatus(t, h, runID); st != "queued" {
		t.Errorf("run status = %q, want queued after process-a's own restart swept it", st)
	}
}

// TestRunQueueStore_Postgres_ResetProcessingRuns_NeverResetsCurrentEpoch pins
// that a boot's own reconcile never touches rows claimed under its OWN
// current epoch — only strictly earlier boots of itself are orphans. This is
// what keeps ResetProcessingRuns safe to call unconditionally at every boot.
func TestRunQueueStore_Postgres_ResetProcessingRuns_NeverResetsCurrentEpoch(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID := seedPgOrgForBlueprints(t, h)
	brID, taskID, promptID := seedPgRunQueueFixture(t, h, orgID, userID)
	step0 := 0
	runID := uuid.New().String()
	if err := stores.RunQueue.EnqueueRun(ctx, orgID, domain.AgentRun{
		ID: runID, TaskID: taskID, PromptID: promptID, Model: "m",
		TriggerType: "manual", CreatorUserID: userID, BlueprintRunID: brID, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}

	if _, err := stores.RunQueue.ClaimNextRun(ctx, "process-self", 5, db.ClaimPlacement{}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Same executor, same epoch: must not reset (not an earlier boot).
	if n, err := stores.RunQueue.ResetProcessingRuns(ctx, "process-self", 5); err != nil {
		t.Fatalf("ResetProcessingRuns at same epoch: %v", err)
	} else if n != 0 {
		t.Errorf("ResetProcessingRuns at the SAME epoch reset %d rows, want 0", n)
	}
	if st, _ := pgRunStatus(t, h, runID); st != "running" {
		t.Errorf("run status = %q, want running (untouched by a same-epoch reset)", st)
	}
}

// TestRunQueueStore_Postgres_CancelRequestedNotClaimed pins that a queued step
// of a cancel-requested blueprint is never claimed.
func TestRunQueueStore_Postgres_CancelRequestedNotClaimed(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID := seedPgOrgForBlueprints(t, h)
	brID, taskID, promptID := seedPgRunQueueFixture(t, h, orgID, userID)

	step0 := 0
	if err := stores.RunQueue.EnqueueRun(ctx, orgID, domain.AgentRun{
		ID: uuid.New().String(), TaskID: taskID, PromptID: promptID, Model: "m",
		TriggerType: "manual", CreatorUserID: userID, BlueprintRunID: brID, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}
	if changed, err := stores.Blueprints.RequestRunCancelSystem(ctx, orgID, brID); err != nil || !changed {
		t.Fatalf("RequestRunCancelSystem = (%v, %v)", changed, err)
	}
	if got, err := stores.RunQueue.ClaimNextRun(ctx, pgRunQueueExecutorID, pgRunQueueBootEpoch, db.ClaimPlacement{}); err != nil || got != nil {
		t.Fatalf("ClaimNextRun on cancel-requested blueprint = (%v, %v), want (nil, nil)", got, err)
	}
}

// TestRunQueueStore_Postgres_ConcurrentClaim proves the FOR UPDATE SKIP LOCKED
// claim never hands the same queued run to two claimers: it enqueues N runs and
// drains them from G goroutines, asserting every run is claimed exactly once.
func TestRunQueueStore_Postgres_ConcurrentClaim(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID := seedPgOrgForBlueprints(t, h)
	brID, taskID, promptID := seedPgRunQueueFixture(t, h, orgID, userID)

	const n = 40
	want := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		runID := uuid.New().String()
		idx := i
		if err := stores.RunQueue.EnqueueRun(ctx, orgID, domain.AgentRun{
			ID: runID, TaskID: taskID, PromptID: promptID, Model: "m",
			TriggerType: "manual", CreatorUserID: userID, BlueprintRunID: brID, BlueprintStepIndex: &idx,
		}); err != nil {
			t.Fatalf("EnqueueRun %d: %v", i, err)
		}
		want[runID] = true
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
				run, err := stores.RunQueue.ClaimNextRun(ctx, pgRunQueueExecutorID, pgRunQueueBootEpoch, db.ClaimPlacement{})
				if err != nil {
					t.Errorf("ClaimNextRun: %v", err)
					return
				}
				if run == nil {
					return // queue drained
				}
				mu.Lock()
				claimed[run.ID]++
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

// TestRunQueueStore_Postgres_ReconcileOrphanedRuns heals the desync where a
// child run is left non-terminal under an already-terminal blueprint_run: the
// boot sweep cancels it (stamping completed_at) and only it, leaving a child
// under a still-running parent untouched.
func TestRunQueueStore_Postgres_ReconcileOrphanedRuns(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID := seedPgOrgForBlueprints(t, h)

	// Orphan: terminal (cancelled) parent, child still running.
	brA, taskA, promptA := seedPgRunQueueFixture(t, h, orgID, userID)
	orphanID := uuid.New().String()
	step0 := 0
	if err := stores.RunQueue.EnqueueRun(ctx, orgID, domain.AgentRun{
		ID: orphanID, TaskID: taskA, PromptID: promptA, Model: "m",
		TriggerType: "manual", CreatorUserID: userID, BlueprintRunID: brA, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("EnqueueRun orphan: %v", err)
	}
	if _, err := h.AdminDB.Exec(`UPDATE runs SET status = 'running' WHERE id = $1`, orphanID); err != nil {
		t.Fatalf("set orphan running: %v", err)
	}
	// A queued orphan under the same terminal parent (never claimed) must also
	// be cancelled — a queued step under a non-running parent is never claimable.
	queuedOrphanID := uuid.New().String()
	if err := stores.RunQueue.EnqueueRun(ctx, orgID, domain.AgentRun{
		ID: queuedOrphanID, TaskID: taskA, PromptID: promptA, Model: "m",
		TriggerType: "manual", CreatorUserID: userID, BlueprintRunID: brA, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("EnqueueRun queued orphan: %v", err)
	}
	if _, err := h.AdminDB.Exec(`UPDATE blueprint_runs SET status = 'cancelled', cancel_requested = false WHERE id = $1`, brA); err != nil {
		t.Fatalf("set parent cancelled: %v", err)
	}

	// Healthy: running parent, child running — must be left alone.
	brB, taskB, promptB := seedPgRunQueueFixture(t, h, orgID, userID)
	healthyID := uuid.New().String()
	if err := stores.RunQueue.EnqueueRun(ctx, orgID, domain.AgentRun{
		ID: healthyID, TaskID: taskB, PromptID: promptB, Model: "m",
		TriggerType: "manual", CreatorUserID: userID, BlueprintRunID: brB, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("EnqueueRun healthy: %v", err)
	}
	if _, err := h.AdminDB.Exec(`UPDATE runs SET status = 'running' WHERE id = $1`, healthyID); err != nil {
		t.Fatalf("set healthy running: %v", err)
	}

	n, err := stores.RunQueue.ReconcileOrphanedRuns(ctx)
	if err != nil || n != 2 {
		t.Fatalf("ReconcileOrphanedRuns = (%d, %v), want (2, nil)", n, err)
	}
	if st, completed := pgRunStatus(t, h, orphanID); st != "cancelled" || !completed {
		t.Errorf("orphan = (%q, completed=%v), want (cancelled, true)", st, completed)
	}
	if st, completed := pgRunStatus(t, h, queuedOrphanID); st != "cancelled" || !completed {
		t.Errorf("queued orphan = (%q, completed=%v), want (cancelled, true)", st, completed)
	}
	if st, _ := pgRunStatus(t, h, healthyID); st != "running" {
		t.Errorf("healthy run status = %q, want running (must not touch a child under a running parent)", st)
	}

	// Idempotent: a second sweep finds nothing.
	if n2, err := stores.RunQueue.ReconcileOrphanedRuns(ctx); err != nil || n2 != 0 {
		t.Errorf("second ReconcileOrphanedRuns = (%d, %v), want (0, nil)", n2, err)
	}
}

// TestRunQueueStore_Postgres_EnqueueStampsActorAgent is the Postgres parity of
// the SQLite actor-stamp test: EnqueueRun (both the manual and event branches)
// persists runs.actor_agent_id, and AgentRunStore.GetSystem JOINs agents to
// surface the display name as ActorAgentName. A run with no actor reads back
// with both fields empty.
func TestRunQueueStore_Postgres_EnqueueStampsActorAgent(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID := seedPgOrgForBlueprints(t, h)
	brID, taskID, promptID := seedPgRunQueueFixture(t, h, orgID, userID)

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
	if err := stores.RunQueue.EnqueueRun(ctx, orgID, domain.AgentRun{
		ID: manualID, TaskID: taskID, PromptID: promptID, Model: "m",
		TriggerType: "manual", CreatorUserID: userID, ActorAgentID: agentID,
		BlueprintRunID: brID, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("EnqueueRun (manual): %v", err)
	}
	got, err := stores.AgentRuns.GetSystem(ctx, orgID, manualID)
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
	if err := stores.RunQueue.EnqueueRun(ctx, orgID, domain.AgentRun{
		ID: eventID, TaskID: taskID, PromptID: promptID, Model: "m",
		TriggerType: "event", ActorAgentID: agentID,
		BlueprintRunID: brID, BlueprintStepIndex: &step1,
	}); err != nil {
		t.Fatalf("EnqueueRun (event): %v", err)
	}
	ev, err := stores.AgentRuns.GetSystem(ctx, orgID, eventID)
	if err != nil || ev == nil {
		t.Fatalf("GetSystem (event): (%v, %v)", ev, err)
	}
	if ev.ActorAgentID != agentID || ev.ActorAgentName != "Triage Bot" {
		t.Errorf("event run actor = (%q, %q), want (%q, Triage Bot)", ev.ActorAgentID, ev.ActorAgentName, agentID)
	}

	// No actor → both fields empty (nullable column + LEFT JOIN).
	bareID := uuid.New().String()
	step2 := 2
	if err := stores.RunQueue.EnqueueRun(ctx, orgID, domain.AgentRun{
		ID: bareID, TaskID: taskID, PromptID: promptID, Model: "m",
		TriggerType: "manual", CreatorUserID: userID,
		BlueprintRunID: brID, BlueprintStepIndex: &step2,
	}); err != nil {
		t.Fatalf("EnqueueRun (no actor): %v", err)
	}
	bare, err := stores.AgentRuns.GetSystem(ctx, orgID, bareID)
	if err != nil || bare == nil {
		t.Fatalf("GetSystem (no actor): (%v, %v)", bare, err)
	}
	if bare.ActorAgentID != "" || bare.ActorAgentName != "" {
		t.Errorf("no-actor run = (%q, %q), want both empty", bare.ActorAgentID, bare.ActorAgentName)
	}
}

// TestRunQueueStore_Postgres_Credentials runs the shared awaiting-credentials
// pubkey conformance suite against the Postgres impl (admin pool, matching
// production wiring). Each factory call resets the harness so subtests don't
// share state.
func TestRunQueueStore_Postgres_Credentials(t *testing.T) {
	h := pgtest.Shared(t)
	ctx := context.Background()

	dbtest.RunRunQueueCredentialsConformance(t, func(t *testing.T) (db.RunQueueStore, string, dbtest.RunQueueCredentialsSeeder) {
		t.Helper()
		h.Reset(t)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
		orgID, userID := seedPgOrgForBlueprints(t, h)
		brID, taskID, promptID := seedPgRunQueueFixture(t, h, orgID, userID)

		nextStep := 0
		seed := dbtest.RunQueueCredentialsSeeder{
			EnqueueRun: func(t *testing.T) string {
				t.Helper()
				idx := nextStep
				nextStep++
				runID := uuid.New().String()
				if err := stores.RunQueue.EnqueueRun(ctx, orgID, domain.AgentRun{
					ID: runID, TaskID: taskID, PromptID: promptID, Model: "m",
					TriggerType: "manual", CreatorUserID: userID, BlueprintRunID: brID, BlueprintStepIndex: &idx,
				}); err != nil {
					t.Fatalf("EnqueueRun: %v", err)
				}
				return runID
			},
			ForceStatus: func(t *testing.T, runID, status string) {
				t.Helper()
				if _, err := h.AdminDB.Exec(`UPDATE runs SET status = $1 WHERE id = $2`, status, runID); err != nil {
					t.Fatalf("force status %q: %v", status, err)
				}
			},
		}
		return stores.RunQueue, orgID, seed
	})
}

// pgRunStatus reads a run's status and whether completed_at is stamped.
func pgRunStatus(t *testing.T, h *pgtest.Harness, runID string) (status string, completed bool) {
	t.Helper()
	var completedAt *string
	if err := h.AdminDB.QueryRow(`SELECT status, completed_at::text FROM runs WHERE id = $1`, runID).
		Scan(&status, &completedAt); err != nil {
		t.Fatalf("read run %s: %v", runID, err)
	}
	return status, completedAt != nil
}

// seedPgRunQueueFixture mints a prompt + blueprint + a running blueprint_run on
// a fresh task, returning (blueprintRunID, taskID, promptID) ready for enqueue.
func seedPgRunQueueFixture(t *testing.T, h *pgtest.Harness, orgID, userID string) (brID, taskID, promptID string) {
	t.Helper()
	bpID := "rq-bp-" + uuid.New().String()[:8]
	seedPgBlueprint(t, h, orgID, userID, bpID)
	promptID = "rq-p-" + uuid.New().String()[:8]
	seedPgPrompt(t, h, orgID, userID, promptID)
	taskID = seedPgTask(t, h, orgID, userID)
	brID = uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO blueprint_runs (id, org_id, creator_user_id, blueprint_id, task_id, trigger_type, status, worktree_path, started_at, step_plan)
		VALUES ($1, $2, $3, $4, $5, 'manual', 'running', $6, now(), '[]')
	`, brID, orgID, userID, bpID, taskID, "/tmp/wt-"+brID); err != nil {
		t.Fatalf("seed blueprint_run: %v", err)
	}
	return brID, taskID, promptID
}
