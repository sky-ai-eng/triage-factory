package postgres_test

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestRunQueueStore_Postgres_EnqueueClaim exercises the basic enqueue → claim →
// requeue → reset cycle against real Postgres (admin pool).
func TestRunQueueStore_Postgres_EnqueueClaim(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB)
	ctx := context.Background()

	orgID, userID := seedPgOrgForBlueprints(t, h)
	brID, taskID, promptID := seedPgRunQueueFixture(t, h, orgID, userID)

	// Empty queue.
	if got, err := stores.RunQueue.ClaimNextRun(ctx); err != nil || got != nil {
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

	got, err := stores.RunQueue.ClaimNextRun(ctx)
	if err != nil || got == nil {
		t.Fatalf("ClaimNextRun: (%v, %v)", got, err)
	}
	if got.ID != runID || got.Status != "running" || got.Attempts != 1 {
		t.Fatalf("claimed = %+v", got)
	}

	// Requeue → re-claimable, attempts retained.
	if err := stores.RunQueue.RequeueRun(ctx, orgID, runID, "transient"); err != nil {
		t.Fatalf("RequeueRun: %v", err)
	}
	got2, err := stores.RunQueue.ClaimNextRun(ctx)
	if err != nil || got2 == nil || got2.Attempts != 2 {
		t.Fatalf("re-claim = (%+v, %v), want attempts=2", got2, err)
	}

	// ResetProcessingRuns flips the mid-flight 'running' row back to queued.
	n, err := stores.RunQueue.ResetProcessingRuns(ctx)
	if err != nil || n != 1 {
		t.Fatalf("ResetProcessingRuns = (%d, %v), want (1, nil)", n, err)
	}
}

// TestRunQueueStore_Postgres_CancelRequestedNotClaimed pins that a queued step
// of a cancel-requested blueprint is never claimed.
func TestRunQueueStore_Postgres_CancelRequestedNotClaimed(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB)
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
	if got, err := stores.RunQueue.ClaimNextRun(ctx); err != nil || got != nil {
		t.Fatalf("ClaimNextRun on cancel-requested blueprint = (%v, %v), want (nil, nil)", got, err)
	}
}

// TestRunQueueStore_Postgres_ConcurrentClaim proves the FOR UPDATE SKIP LOCKED
// claim never hands the same queued run to two claimers: it enqueues N runs and
// drains them from G goroutines, asserting every run is claimed exactly once.
func TestRunQueueStore_Postgres_ConcurrentClaim(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB)
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
				run, err := stores.RunQueue.ClaimNextRun(ctx)
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
