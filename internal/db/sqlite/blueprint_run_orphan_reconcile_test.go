package sqlite_test

import (
	"context"
	"database/sql"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// childRunStatusDB reads a single conversations row's status.
func childRunStatusDB(t *testing.T, conn *sql.DB, id string) string {
	t.Helper()
	var s string
	if err := conn.QueryRow(`SELECT status FROM conversations WHERE id = ?`, id).Scan(&s); err != nil {
		t.Fatalf("read run %s status: %v", id, err)
	}
	return s
}

// TestMarkRunStatus_CancelsOrphanedChild_OnTerminal pins the atomic
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
	insertConversationForTest(t, conn, domain.AgentRun{
		ID: "oa-child", TaskID: task.ID, PromptID: "oa-p0", Status: "running",
		Model: "claude-sonnet-4-6", BlueprintRunID: brID, BlueprintStepIndex: &step0,
	})
	if _, err := conn.Exec(`UPDATE conversations SET status = 'running' WHERE id = 'oa-child'`); err != nil {
		t.Fatalf("set child running: %v", err)
	}
	// The racing dispatcher already claimed the child; the cancel must
	// release the engagement along with the status flip, not leave it for
	// the janitor.
	if _, err := conn.Exec(`
		INSERT INTO claims (id, conversation_id, executor_id) VALUES ('oa-claim', 'oa-child', 'exec-oa')
	`); err != nil {
		t.Fatalf("seed oa-child claim: %v", err)
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
	if err := conn.QueryRow(`SELECT completed_at FROM conversations WHERE id = 'oa-child'`).Scan(&completedAt); err != nil {
		t.Fatalf("read completed_at: %v", err)
	}
	if completedAt == nil {
		t.Error("cancelled child run has NULL completed_at; want a stamp")
	}
	var releasedAt any
	var outcome string
	if err := conn.QueryRow(`SELECT released_at, COALESCE(outcome, '') FROM claims WHERE id = 'oa-claim'`).Scan(&releasedAt, &outcome); err != nil {
		t.Fatalf("read oa-claim: %v", err)
	}
	if releasedAt == nil || outcome != "cancelled" {
		t.Errorf("cancelled child's claim = (released=%v, outcome=%q), want (released, cancelled)", releasedAt != nil, outcome)
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
	insertConversationForTest(t, conn, domain.AgentRun{
		ID: "of-child", TaskID: task.ID, PromptID: "of-p0", Status: "completed",
		Model: "claude-sonnet-4-6", BlueprintRunID: brID, BlueprintStepIndex: &step0,
	})
	if _, err := conn.Exec(`UPDATE conversations SET status = 'completed', outcome = 'finish' WHERE id = 'of-child'`); err != nil {
		t.Fatalf("set child completed: %v", err)
	}

	if _, err := stores.Blueprints.MarkRunStatus(ctx, org, brID, domain.BlueprintRunStatusCompleted, "", nil); err != nil {
		t.Fatalf("MarkRunStatus: %v", err)
	}

	if got := childRunStatusDB(t, conn, "of-child"); got != "completed" {
		t.Errorf("child run status = %q, want completed (a terminal child must not be re-cancelled)", got)
	}
	var outcome string
	if err := conn.QueryRow(`SELECT COALESCE(outcome,'') FROM conversations WHERE id = 'of-child'`).Scan(&outcome); err != nil {
		t.Fatalf("read outcome: %v", err)
	}
	if outcome != "finish" {
		t.Errorf("child outcome = %q, want finish (must not be clobbered)", outcome)
	}
}

// TestReconcileOrphanedRuns heals the exact desync: a child run
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
	insertConversationForTest(t, conn, domain.AgentRun{
		ID: "ra-child", TaskID: taskA.ID, PromptID: "ra-p0", Status: "running",
		Model: "claude-sonnet-4-6", BlueprintRunID: brA, BlueprintStepIndex: &step0,
	})
	// Force the desync directly (bypassing MarkRunStatus's atomic cancel) to
	// mimic a DB broken before the fix landed.
	if _, err := conn.Exec(`UPDATE conversations SET status = 'running' WHERE id = 'ra-child'`); err != nil {
		t.Fatalf("set child A running: %v", err)
	}
	if _, err := conn.Exec(`UPDATE blueprint_runs SET status = 'cancelled', cancel_requested = 0 WHERE id = 'ra-br'`); err != nil {
		t.Fatalf("set br A cancelled: %v", err)
	}
	// ra-child streamed two token-bearing messages before the crash; the boot
	// sweep must roll them up onto the run, not leave the columns at 0 (TFAC-473).
	if _, err := conn.Exec(`
		INSERT INTO messages (conversation_id, role, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens)
		VALUES ('ra-child', 'assistant', 100, 20, 1000, 7),
		       ('ra-child', 'assistant',  50,  5,  500, 3)
	`); err != nil {
		t.Fatalf("seed ra-child messages: %v", err)
	}

	// A second orphan that never started: queued child under the same terminal
	// parent (the parent went terminal before the step was claimed). Must also be
	// cancelled — a queued step under a non-running parent is never claimable.
	insertConversationForTest(t, conn, domain.AgentRun{
		ID: "ra-child-queued", TaskID: taskA.ID, PromptID: "ra-p0", Status: "queued",
		Model: "claude-sonnet-4-6", BlueprintRunID: brA, BlueprintStepIndex: &step0,
	})

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
	insertConversationForTest(t, conn, domain.AgentRun{
		ID: "rb-child", TaskID: taskB.ID, PromptID: "rb-p0", Status: "running",
		Model: "claude-sonnet-4-6", BlueprintRunID: brB, BlueprintStepIndex: &step0,
	})
	if _, err := conn.Exec(`UPDATE conversations SET status = 'running' WHERE id = 'rb-child'`); err != nil {
		t.Fatalf("set child B running: %v", err)
	}
	// A genuinely running child holds an active claim (ClaimNextRun mints
	// it); without one, the claim-desync requeue arm would rightly treat the
	// row as stranded.
	if _, err := conn.Exec(`
		INSERT INTO claims (id, conversation_id, executor_id) VALUES ('rb-claim', 'rb-child', 'exec-b')
	`); err != nil {
		t.Fatalf("seed rb-child claim: %v", err)
	}

	n, err := stores.RunQueue.ReconcileOrphanedRuns(ctx)
	if err != nil {
		t.Fatalf("ReconcileOrphanedRuns: %v", err)
	}
	if n != 2 {
		t.Errorf("reconciled count = %d, want 2 (running + queued orphans under the terminal parent)", n)
	}
	if got := childRunStatusDB(t, conn, "ra-child"); got != "cancelled" {
		t.Errorf("orphan child status = %q, want cancelled", got)
	}
	if got := childRunStatusDB(t, conn, "ra-child-queued"); got != "cancelled" {
		t.Errorf("queued orphan child status = %q, want cancelled", got)
	}
	// The running orphan's streamed tokens still read through the ledger —
	// the sweep is a status flip only, nothing to roll up or lose.
	{
		var in, out, cr, cc int
		if err := conn.QueryRow(`
			SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
			       COALESCE(SUM(cache_read_tokens), 0), COALESCE(SUM(cache_creation_tokens), 0)
			FROM messages WHERE conversation_id = 'ra-child'
		`).Scan(&in, &out, &cr, &cc); err != nil {
			t.Fatalf("read ra-child tokens: %v", err)
		}
		if in != 150 || out != 25 || cr != 1500 || cc != 10 {
			t.Errorf("orphan child ledger tokens = (%d,%d,%d,%d), want (150,25,1500,10)", in, out, cr, cc)
		}
	}
	if got := childRunStatusDB(t, conn, "rb-child"); got != "running" {
		t.Errorf("healthy child status = %q, want running (must not touch children under a running parent)", got)
	}

	// Idempotent: a second sweep finds nothing.
	if n2, err := stores.RunQueue.ReconcileOrphanedRuns(ctx); err != nil || n2 != 0 {
		t.Errorf("second ReconcileOrphanedRuns = (%d, %v), want (0, nil)", n2, err)
	}
}

// TestReconcileOrphanedRuns_HealsClaimDesyncs pins the janitor arms on the
// SQLite side: a terminal conversation with a dangling active claim gets the
// claim released with the status-mapped outcome, and an in-flight delegation
// conversation with no active claim under a running parent goes back to
// 'queued' — while the healthy shapes (running with an active claim, queued
// with none) are untouched.
func TestReconcileOrphanedRuns_HealsClaimDesyncs(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	task := seedEntityEventTask(t, conn, "desync")
	insertPromptForBlueprintTest(t, conn, domain.Prompt{ID: "ds-p0", Name: "p0", Body: "b", Source: "user"})
	insertBlueprintForTest(t, conn, "ds-bp", "Desync BP")
	if err := stores.Blueprints.ReplaceSteps(ctx, org, "ds-bp", []string{"ds-p0"}, nil); err != nil {
		t.Fatalf("ReplaceSteps: %v", err)
	}
	brID, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "ds-br", BlueprintID: "ds-bp", TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning, WorktreePath: "/tmp/wt-ds",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	step0 := 0
	seedChild := func(id, status string) {
		t.Helper()
		insertConversationForTest(t, conn, domain.AgentRun{
			ID: id, TaskID: task.ID, PromptID: "ds-p0", Status: "running",
			Model: "claude-sonnet-4-6", BlueprintRunID: brID, BlueprintStepIndex: &step0,
		})
		if _, err := conn.Exec(`UPDATE conversations SET status = ? WHERE id = ?`, status, id); err != nil {
			t.Fatalf("set %s status: %v", id, err)
		}
	}
	activeClaim := func(claimID, convID string) {
		t.Helper()
		if _, err := conn.Exec(`
			INSERT INTO claims (id, conversation_id, executor_id) VALUES (?, ?, 'exec-ds')
		`, claimID, convID); err != nil {
			t.Fatalf("seed claim %s: %v", claimID, err)
		}
	}

	// Dangling claims on terminal rows (the crash-after-flip shape).
	seedChild("ds-done", "completed")
	activeClaim("ds-done-cl", "ds-done")
	seedChild("ds-unsolvable", "task_unsolvable")
	activeClaim("ds-unsolvable-cl", "ds-unsolvable")
	// Claimless running row (the rolled-back-flip shape), with a stale
	// placement stamp the requeue must clear.
	seedChild("ds-stranded", "running")
	if _, err := conn.Exec(`UPDATE conversations SET preferred_executor_id = 'exec-dead' WHERE id = 'ds-stranded'`); err != nil {
		t.Fatalf("stamp placement: %v", err)
	}
	// Healthy shapes: running with a live claim; queued with none.
	seedChild("ds-healthy", "running")
	activeClaim("ds-healthy-cl", "ds-healthy")
	seedChild("ds-queued", "queued")

	n, err := stores.RunQueue.ReconcileOrphanedRuns(ctx)
	if err != nil {
		t.Fatalf("ReconcileOrphanedRuns: %v", err)
	}
	if n != 3 {
		t.Errorf("healed count = %d, want 3 (two released claims + one requeue)", n)
	}

	claimState := func(id string) (released bool, outcome string) {
		t.Helper()
		var rel any
		if err := conn.QueryRow(`SELECT released_at, COALESCE(outcome, '') FROM claims WHERE id = ?`, id).Scan(&rel, &outcome); err != nil {
			t.Fatalf("read claim %s: %v", id, err)
		}
		return rel != nil, outcome
	}
	if rel, out := claimState("ds-done-cl"); !rel || out != "completed" {
		t.Errorf("completed row's claim = (released=%v, outcome=%q), want (true, completed)", rel, out)
	}
	if rel, out := claimState("ds-unsolvable-cl"); !rel || out != "failed" {
		t.Errorf("task_unsolvable row's claim = (released=%v, outcome=%q), want (true, failed)", rel, out)
	}
	if got := childRunStatusDB(t, conn, "ds-stranded"); got != "queued" {
		t.Errorf("stranded row status = %q, want queued", got)
	}
	var pref any
	if err := conn.QueryRow(`SELECT preferred_executor_id FROM conversations WHERE id = 'ds-stranded'`).Scan(&pref); err != nil {
		t.Fatalf("read preferred_executor_id: %v", err)
	}
	if pref != nil {
		t.Errorf("stranded row preferred_executor_id = %v, want cleared", pref)
	}
	if rel, _ := claimState("ds-healthy-cl"); rel {
		t.Error("healthy running row's live claim was released")
	}
	if got := childRunStatusDB(t, conn, "ds-healthy"); got != "running" {
		t.Errorf("healthy running row status = %q, want running", got)
	}
	if got := childRunStatusDB(t, conn, "ds-queued"); got != "queued" {
		t.Errorf("claimless queued row status = %q, want queued (already claimable, nothing to heal)", got)
	}

	// Idempotent: a second sweep finds nothing.
	if n2, err := stores.RunQueue.ReconcileOrphanedRuns(ctx); err != nil || n2 != 0 {
		t.Errorf("second sweep = (%d, %v), want (0, nil)", n2, err)
	}
}
