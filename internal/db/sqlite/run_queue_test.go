package sqlite_test

import (
	"context"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func TestRunQueueStore_SQLite_EnqueueClaim(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrg

	task := seedEntityEventTask(t, conn, "rq-claim")
	insertPromptForBlueprintTest(t, conn, domain.Prompt{ID: "rq-p0", Name: "Step 0", Body: "b", Source: "user"})
	insertBlueprintForTest(t, conn, "rq-bp", "RQ Blueprint")
	if err := stores.Blueprints.ReplaceSteps(ctx, org, "rq-bp", []string{"rq-p0"}, nil); err != nil {
		t.Fatalf("ReplaceSteps: %v", err)
	}
	brID, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "rq-br", BlueprintID: "rq-bp", TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
		WorktreePath: "/tmp/wt-rq",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// Empty queue: nothing claimable.
	if got, err := stores.RunQueue.ClaimNextRun(ctx); err != nil || got != nil {
		t.Fatalf("ClaimNextRun on empty queue = (%v, %v), want (nil, nil)", got, err)
	}

	step0 := 0
	if err := stores.RunQueue.EnqueueRun(ctx, org, domain.AgentRun{
		ID: "rq-run-0", TaskID: task.ID, PromptID: "rq-p0", Model: "claude-sonnet-4-6",
		TriggerType: "manual", BlueprintRunID: brID, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}

	got, err := stores.RunQueue.ClaimNextRun(ctx)
	if err != nil {
		t.Fatalf("ClaimNextRun: %v", err)
	}
	if got == nil {
		t.Fatal("ClaimNextRun returned nil for a queued run")
	}
	if got.ID != "rq-run-0" || got.BlueprintRunID != brID || got.Status != "running" {
		t.Fatalf("claimed run = %+v", got)
	}
	if got.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", got.Attempts)
	}
	if got.BlueprintStepIndex == nil || *got.BlueprintStepIndex != 0 {
		t.Fatalf("step index = %v, want 0", got.BlueprintStepIndex)
	}

	// Claimed → no longer queued, so a second claim finds nothing.
	if got2, err := stores.RunQueue.ClaimNextRun(ctx); err != nil || got2 != nil {
		t.Fatalf("second ClaimNextRun = (%v, %v), want (nil, nil)", got2, err)
	}
}

func TestRunQueueStore_SQLite_CancelRequestedNotClaimed(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrg

	task := seedEntityEventTask(t, conn, "rq-cancel")
	insertPromptForBlueprintTest(t, conn, domain.Prompt{ID: "rqc-p0", Name: "Step 0", Body: "b", Source: "user"})
	insertBlueprintForTest(t, conn, "rqc-bp", "RQ Cancel Blueprint")
	if err := stores.Blueprints.ReplaceSteps(ctx, org, "rqc-bp", []string{"rqc-p0"}, nil); err != nil {
		t.Fatalf("ReplaceSteps: %v", err)
	}
	brID, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "rqc-br", BlueprintID: "rqc-bp", TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
		WorktreePath: "/tmp/wt-rqc",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	step0 := 0
	if err := stores.RunQueue.EnqueueRun(ctx, org, domain.AgentRun{
		ID: "rqc-run-0", TaskID: task.ID, PromptID: "rqc-p0", Model: "m",
		TriggerType: "manual", BlueprintRunID: brID, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}

	// Raise the sequence-cancel signal: the queued step must not be claimed.
	changed, err := stores.Blueprints.RequestRunCancelSystem(ctx, org, brID)
	if err != nil || !changed {
		t.Fatalf("RequestRunCancelSystem = (%v, %v), want (true, nil)", changed, err)
	}
	if got, err := stores.RunQueue.ClaimNextRun(ctx); err != nil || got != nil {
		t.Fatalf("ClaimNextRun on cancel-requested blueprint = (%v, %v), want (nil, nil)", got, err)
	}
	// Idempotent: re-requesting on an already-flagged running row reports no change.
	if changed, err := stores.Blueprints.RequestRunCancelSystem(ctx, org, brID); err != nil || changed {
		t.Fatalf("second RequestRunCancelSystem = (%v, %v), want (false, nil)", changed, err)
	}
	// The flag is readable back through GetRun.
	br, err := stores.Blueprints.GetRunSystem(ctx, org, brID)
	if err != nil || br == nil || !br.CancelRequested {
		t.Fatalf("GetRunSystem cancel_requested = %v (br=%+v, err=%v)", br != nil && br.CancelRequested, br, err)
	}
}

func TestRunQueueStore_SQLite_RequeueAndReset(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrg

	task := seedEntityEventTask(t, conn, "rq-reset")
	insertPromptForBlueprintTest(t, conn, domain.Prompt{ID: "rqr-p0", Name: "Step 0", Body: "b", Source: "user"})
	insertBlueprintForTest(t, conn, "rqr-bp", "RQ Reset Blueprint")
	if err := stores.Blueprints.ReplaceSteps(ctx, org, "rqr-bp", []string{"rqr-p0"}, nil); err != nil {
		t.Fatalf("ReplaceSteps: %v", err)
	}
	brID, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "rqr-br", BlueprintID: "rqr-bp", TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
		WorktreePath: "/tmp/wt-rqr",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	step0 := 0
	if err := stores.RunQueue.EnqueueRun(ctx, org, domain.AgentRun{
		ID: "rqr-run-0", TaskID: task.ID, PromptID: "rqr-p0", Model: "m",
		TriggerType: "manual", BlueprintRunID: brID, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}

	// Claim it → running, attempts=1.
	claimed, err := stores.RunQueue.ClaimNextRun(ctx)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNextRun: (%v, %v)", claimed, err)
	}

	// RequeueRun puts it back to queued (attempts retained), re-claimable.
	if err := stores.RunQueue.RequeueRun(ctx, org, "rqr-run-0", "transient setup error"); err != nil {
		t.Fatalf("RequeueRun: %v", err)
	}
	reclaimed, err := stores.RunQueue.ClaimNextRun(ctx)
	if err != nil || reclaimed == nil {
		t.Fatalf("re-ClaimNextRun: (%v, %v)", reclaimed, err)
	}
	if reclaimed.Attempts != 2 {
		t.Fatalf("attempts after requeue+reclaim = %d, want 2", reclaimed.Attempts)
	}

	// Now the run is 'running' again (mid-flight). ResetProcessingRuns should
	// flip it back to 'queued'.
	n, err := stores.RunQueue.ResetProcessingRuns(ctx)
	if err != nil {
		t.Fatalf("ResetProcessingRuns: %v", err)
	}
	if n != 1 {
		t.Fatalf("ResetProcessingRuns reset %d rows, want 1", n)
	}
	afterReset, err := stores.RunQueue.ClaimNextRun(ctx)
	if err != nil || afterReset == nil {
		t.Fatalf("ClaimNextRun after reset: (%v, %v)", afterReset, err)
	}
	if afterReset.Attempts != 3 {
		t.Fatalf("attempts after reset+reclaim = %d, want 3 (reset retains attempts)", afterReset.Attempts)
	}
}

func TestRunQueueStore_SQLite_ResetLeavesDormantAlone(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrg

	task := seedEntityEventTask(t, conn, "rq-dormant")
	insertPromptForBlueprintTest(t, conn, domain.Prompt{ID: "rqd-p0", Name: "Step 0", Body: "b", Source: "user"})
	insertBlueprintForTest(t, conn, "rqd-bp", "RQ Dormant Blueprint")
	if err := stores.Blueprints.ReplaceSteps(ctx, org, "rqd-bp", []string{"rqd-p0"}, nil); err != nil {
		t.Fatalf("ReplaceSteps: %v", err)
	}
	brID, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "rqd-br", BlueprintID: "rqd-bp", TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
		WorktreePath: "/tmp/wt-rqd",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// A parked (dormant) run — directly insert with status awaiting_input.
	step0 := 0
	if err := stores.AgentRuns.Create(ctx, org, domain.AgentRun{
		ID: "rqd-run-0", TaskID: task.ID, PromptID: "rqd-p0", Status: "awaiting_input",
		Model: "m", TriggerType: "manual", BlueprintRunID: brID, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("Create dormant run: %v", err)
	}

	n, err := stores.RunQueue.ResetProcessingRuns(ctx)
	if err != nil {
		t.Fatalf("ResetProcessingRuns: %v", err)
	}
	if n != 0 {
		t.Fatalf("ResetProcessingRuns reset %d rows, want 0 (dormant run must stay parked)", n)
	}
	// And it is not claimable (it's not 'queued').
	if got, err := stores.RunQueue.ClaimNextRun(ctx); err != nil || got != nil {
		t.Fatalf("ClaimNextRun = (%v, %v), want (nil, nil)", got, err)
	}
}

func TestRunQueueStore_SQLite_SetCurrentStep(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrg

	task := seedEntityEventTask(t, conn, "rq-step")
	insertBlueprintForTest(t, conn, "rqs-bp", "RQ Step Blueprint")
	brID, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "rqs-br", BlueprintID: "rqs-bp", TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
		WorktreePath: "/tmp/wt-rqs",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := stores.Blueprints.SetRunCurrentStepSystem(ctx, org, brID, 3); err != nil {
		t.Fatalf("SetRunCurrentStepSystem: %v", err)
	}
	br, err := stores.Blueprints.GetRunSystem(ctx, org, brID)
	if err != nil || br == nil {
		t.Fatalf("GetRunSystem: (%v, %v)", br, err)
	}
	if br.CurrentStepIndex != 3 {
		t.Fatalf("CurrentStepIndex = %d, want 3", br.CurrentStepIndex)
	}
}

func TestRunQueueStore_SQLite_RejectsNonLocalOrg(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	const bogusOrg = "11111111-1111-1111-1111-111111111111"

	if err := stores.RunQueue.EnqueueRun(ctx, bogusOrg, domain.AgentRun{}); err == nil {
		t.Errorf("EnqueueRun with non-local orgID should error")
	}
	if err := stores.RunQueue.RequeueRun(ctx, bogusOrg, "r", "x"); err == nil {
		t.Errorf("RequeueRun with non-local orgID should error")
	}
}
