package sqlite_test

import (
	"context"
	"database/sql"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// childRunStatusDB reads a single runs row's status.
func childRunStatusDB(t *testing.T, conn *sql.DB, id string) string {
	t.Helper()
	var s string
	if err := conn.QueryRow(`SELECT status FROM runs WHERE id = ?`, id).Scan(&s); err != nil {
		t.Fatalf("read run %s status: %v", id, err)
	}
	return s
}

// TestMarkRunStatus_CancelsOrphanedChild_OnTerminal pins the TFAC-441 atomic
// guarantee: flipping a blueprint_run to a terminal status must cancel any
// still-active child run in the same call, so a cancel that raced the
// dispatcher can't strand a child 'running' (which keeps the dispatcher on
// phantom work and pins its feature branch in a worktree, requeuing forever).
func TestMarkRunStatus_CancelsOrphanedChild_OnTerminal(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	task := seedEntityEventTask(t, conn, "orphan-atomic")
	insertPromptForBlueprintTest(t, conn, domain.Prompt{ID: "oa-p0", Name: "p0", Body: "b", Source: "user"})
	insertBlueprintForTest(t, conn, "oa-bp", "Orphan Atomic BP")
	if err := stores.Blueprints.ReplaceSteps(ctx, org, "oa-bp", []string{"oa-p0"}, nil); err != nil {
		t.Fatalf("ReplaceSteps: %v", err)
	}
	brID, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "oa-br", BlueprintID: "oa-bp", TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
		WorktreePath: "/tmp/wt-oa",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	step0 := 0
	if err := stores.AgentRuns.Create(ctx, org, domain.AgentRun{
		ID: "oa-child", TaskID: task.ID, PromptID: "oa-p0", Status: "running",
		Model: "claude-sonnet-4-6", BlueprintRunID: brID, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("create child run: %v", err)
	}
	if _, err := conn.Exec(`UPDATE runs SET status = 'running' WHERE id = 'oa-child'`); err != nil {
		t.Fatalf("set child running: %v", err)
	}

	changed, err := stores.Blueprints.MarkRunStatus(ctx, org, brID, domain.BlueprintRunStatusCancelled, "user_cancelled", nil)
	if err != nil {
		t.Fatalf("MarkRunStatus: %v", err)
	}
	if !changed {
		t.Fatal("MarkRunStatus reported no change")
	}

	if got := childRunStatusDB(t, conn, "oa-child"); got != "cancelled" {
		t.Errorf("child run status = %q, want cancelled (must not strand a child under a terminal parent)", got)
	}
	var completedAt any
	if err := conn.QueryRow(`SELECT completed_at FROM runs WHERE id = 'oa-child'`).Scan(&completedAt); err != nil {
		t.Fatalf("read completed_at: %v", err)
	}
	if completedAt == nil {
		t.Error("cancelled child run has NULL completed_at; want a stamp")
	}
}

// TestMarkRunStatus_LeavesTerminalChild pins that a clean finish (the common
// path, where the finishing step is already 'completed') does not clobber the
// child's terminal status/outcome.
func TestMarkRunStatus_LeavesTerminalChild(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	task := seedEntityEventTask(t, conn, "orphan-finish")
	insertPromptForBlueprintTest(t, conn, domain.Prompt{ID: "of-p0", Name: "p0", Body: "b", Source: "user"})
	insertBlueprintForTest(t, conn, "of-bp", "Orphan Finish BP")
	if err := stores.Blueprints.ReplaceSteps(ctx, org, "of-bp", []string{"of-p0"}, nil); err != nil {
		t.Fatalf("ReplaceSteps: %v", err)
	}
	brID, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "of-br", BlueprintID: "of-bp", TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
		WorktreePath: "/tmp/wt-of",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	step0 := 0
	if err := stores.AgentRuns.Create(ctx, org, domain.AgentRun{
		ID: "of-child", TaskID: task.ID, PromptID: "of-p0", Status: "completed",
		Model: "claude-sonnet-4-6", BlueprintRunID: brID, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("create child run: %v", err)
	}
	if _, err := conn.Exec(`UPDATE runs SET status = 'completed', outcome = 'finish' WHERE id = 'of-child'`); err != nil {
		t.Fatalf("set child completed: %v", err)
	}

	if _, err := stores.Blueprints.MarkRunStatus(ctx, org, brID, domain.BlueprintRunStatusCompleted, "", nil); err != nil {
		t.Fatalf("MarkRunStatus: %v", err)
	}

	if got := childRunStatusDB(t, conn, "of-child"); got != "completed" {
		t.Errorf("child run status = %q, want completed (a terminal child must not be re-cancelled)", got)
	}
	var outcome string
	if err := conn.QueryRow(`SELECT COALESCE(outcome,'') FROM runs WHERE id = 'of-child'`).Scan(&outcome); err != nil {
		t.Fatalf("read outcome: %v", err)
	}
	if outcome != "finish" {
		t.Errorf("child outcome = %q, want finish (must not be clobbered)", outcome)
	}
}

// TestReconcileOrphanedRuns heals the exact desync from TFAC-441: a child run
// left 'running' under an already-terminal blueprint_run. The boot sweep must
// cancel it (and only it), leaving children under a still-running parent alone.
func TestReconcileOrphanedRuns(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	// Desynced: terminal (cancelled) blueprint_run, child still running.
	taskA := seedEntityEventTask(t, conn, "recon-a")
	insertPromptForBlueprintTest(t, conn, domain.Prompt{ID: "ra-p0", Name: "p0", Body: "b", Source: "user"})
	insertBlueprintForTest(t, conn, "ra-bp", "Recon A BP")
	if err := stores.Blueprints.ReplaceSteps(ctx, org, "ra-bp", []string{"ra-p0"}, nil); err != nil {
		t.Fatalf("ReplaceSteps A: %v", err)
	}
	brA, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "ra-br", BlueprintID: "ra-bp", TaskID: taskA.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning, WorktreePath: "/tmp/wt-ra",
	})
	if err != nil {
		t.Fatalf("CreateRun A: %v", err)
	}
	step0 := 0
	if err := stores.AgentRuns.Create(ctx, org, domain.AgentRun{
		ID: "ra-child", TaskID: taskA.ID, PromptID: "ra-p0", Status: "running",
		Model: "claude-sonnet-4-6", BlueprintRunID: brA, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("create child A: %v", err)
	}
	// Force the desync directly (bypassing MarkRunStatus's atomic cancel) to
	// mimic a DB broken before the fix landed.
	if _, err := conn.Exec(`UPDATE runs SET status = 'running' WHERE id = 'ra-child'`); err != nil {
		t.Fatalf("set child A running: %v", err)
	}
	if _, err := conn.Exec(`UPDATE blueprint_runs SET status = 'cancelled', cancel_requested = 0 WHERE id = 'ra-br'`); err != nil {
		t.Fatalf("set br A cancelled: %v", err)
	}

	// Healthy: running blueprint_run, child running — must be left alone.
	taskB := seedEntityEventTask(t, conn, "recon-b")
	insertPromptForBlueprintTest(t, conn, domain.Prompt{ID: "rb-p0", Name: "p0", Body: "b", Source: "user"})
	insertBlueprintForTest(t, conn, "rb-bp", "Recon B BP")
	if err := stores.Blueprints.ReplaceSteps(ctx, org, "rb-bp", []string{"rb-p0"}, nil); err != nil {
		t.Fatalf("ReplaceSteps B: %v", err)
	}
	brB, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "rb-br", BlueprintID: "rb-bp", TaskID: taskB.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning, WorktreePath: "/tmp/wt-rb",
	})
	if err != nil {
		t.Fatalf("CreateRun B: %v", err)
	}
	if err := stores.AgentRuns.Create(ctx, org, domain.AgentRun{
		ID: "rb-child", TaskID: taskB.ID, PromptID: "rb-p0", Status: "running",
		Model: "claude-sonnet-4-6", BlueprintRunID: brB, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("create child B: %v", err)
	}
	if _, err := conn.Exec(`UPDATE runs SET status = 'running' WHERE id = 'rb-child'`); err != nil {
		t.Fatalf("set child B running: %v", err)
	}

	n, err := stores.RunQueue.ReconcileOrphanedRuns(ctx)
	if err != nil {
		t.Fatalf("ReconcileOrphanedRuns: %v", err)
	}
	if n != 1 {
		t.Errorf("reconciled count = %d, want 1 (only the orphan under the terminal parent)", n)
	}
	if got := childRunStatusDB(t, conn, "ra-child"); got != "cancelled" {
		t.Errorf("orphan child status = %q, want cancelled", got)
	}
	if got := childRunStatusDB(t, conn, "rb-child"); got != "running" {
		t.Errorf("healthy child status = %q, want running (must not touch children under a running parent)", got)
	}

	// Idempotent: a second sweep finds nothing.
	if n2, err := stores.RunQueue.ReconcileOrphanedRuns(ctx); err != nil || n2 != 0 {
		t.Errorf("second ReconcileOrphanedRuns = (%d, %v), want (0, nil)", n2, err)
	}
}
