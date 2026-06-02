package delegate

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// SKY-330: advanceTaskFromRunStatus mirrors run-state lifecycle changes
// onto the bot-claimed task's status field so the board's columns
// reflect where the work is. These tests pin the state machine
// directly — the matrix below covers every branch the helper exposes,
// without needing a real Claude process running.
//
// Test seeds reuse the takeover-test fixture: seedRun creates the
// entity + event + task + run chain with status='running'. We then
// stamp the bot claim (or skip / replace it) per case and invoke
// advanceTaskFromRunStatus with the run-status we want to test.

func TestAdvanceTaskFromRunStatus_RunningSetsInProgress(t *testing.T) {
	s, database, runID, taskID := setupAdvanceFixture(t, "ip")
	stampBotClaim(t, database, taskID)

	s.advanceTaskFromRunStatus(runmode.LocalDefaultOrg, runID, "running")

	if got := readTaskStatus(t, database, taskID); got != "in_progress" {
		t.Errorf("task.status = %q, want in_progress", got)
	}
}

// initializing also maps to in_progress (active-stage bucket). The
// spawner's Delegate path broadcasts initializing directly without
// going through updateStatus, so this helper is rarely the path
// initializing arrives via — but if it does, the mapping must still
// land. The Claimed column projection handles the more common
// "claimed-queued + initializing" window via the store-level
// claimed-derivation update sibling to this test.
func TestAdvanceTaskFromRunStatus_InitializingSetsInProgress(t *testing.T) {
	s, database, runID, taskID := setupAdvanceFixture(t, "init")
	stampBotClaim(t, database, taskID)

	s.advanceTaskFromRunStatus(runmode.LocalDefaultOrg, runID, "initializing")

	if got := readTaskStatus(t, database, taskID); got != "in_progress" {
		t.Errorf("task.status = %q, want in_progress", got)
	}
}

func TestAdvanceTaskFromRunStatus_PendingApprovalSetsInReview(t *testing.T) {
	s, database, runID, taskID := setupAdvanceFixture(t, "ir")
	stampBotClaim(t, database, taskID)

	s.advanceTaskFromRunStatus(runmode.LocalDefaultOrg, runID, "pending_approval")

	if got := readTaskStatus(t, database, taskID); got != "in_review" {
		t.Errorf("task.status = %q, want in_review", got)
	}
}

// completed must land via CloseSystem (not raw SetStatus) so
// close_reason='run_completed' and closed_at are populated — the Done
// column's 7-day cap reads closed_at and excludes NULLs after the
// SKY-330 tighten.
func TestAdvanceTaskFromRunStatus_CompletedClosesWithReason(t *testing.T) {
	s, database, runID, taskID := setupAdvanceFixture(t, "done")
	stampBotClaim(t, database, taskID)

	s.advanceTaskFromRunStatus(runmode.LocalDefaultOrg, runID, "completed")

	var status, closeReason string
	var closedAt sql.NullTime
	if err := database.QueryRow(
		`SELECT status, COALESCE(close_reason, ''), closed_at FROM tasks WHERE id = ?`,
		taskID,
	).Scan(&status, &closeReason, &closedAt); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if status != "done" {
		t.Errorf("status = %q, want done", status)
	}
	if closeReason != "run_completed" {
		t.Errorf("close_reason = %q, want run_completed", closeReason)
	}
	if !closedAt.Valid {
		t.Error("closed_at not set; Done column's 7-day cap will exclude this row")
	}
}

// Terminal run states the helper deliberately ignores. Failure /
// cancellation leave the task in its current status so the user
// decides next steps (re-prompt, take over, dismiss) via the
// assignee picker.
func TestAdvanceTaskFromRunStatus_FailedNoOp(t *testing.T) {
	s, database, runID, taskID := setupAdvanceFixture(t, "failed")
	stampBotClaim(t, database, taskID)
	// Park the task at in_progress so we can prove "failed" doesn't
	// move it to done or anywhere else.
	if _, err := database.Exec(`UPDATE tasks SET status = 'in_progress' WHERE id = ?`, taskID); err != nil {
		t.Fatalf("park: %v", err)
	}

	s.advanceTaskFromRunStatus(runmode.LocalDefaultOrg, runID, "failed")

	if got := readTaskStatus(t, database, taskID); got != "in_progress" {
		t.Errorf("status = %q, want in_progress (failed must not transition)", got)
	}
}

func TestAdvanceTaskFromRunStatus_CancelledNoOp(t *testing.T) {
	s, database, runID, taskID := setupAdvanceFixture(t, "cancelled")
	stampBotClaim(t, database, taskID)
	if _, err := database.Exec(`UPDATE tasks SET status = 'in_review' WHERE id = ?`, taskID); err != nil {
		t.Fatalf("park: %v", err)
	}

	s.advanceTaskFromRunStatus(runmode.LocalDefaultOrg, runID, "cancelled")

	if got := readTaskStatus(t, database, taskID); got != "in_review" {
		t.Errorf("status = %q, want in_review (cancelled must not transition)", got)
	}
}

func TestAdvanceTaskFromRunStatus_TaskUnsolvableNoOp(t *testing.T) {
	s, database, runID, taskID := setupAdvanceFixture(t, "unsolvable")
	stampBotClaim(t, database, taskID)
	if _, err := database.Exec(`UPDATE tasks SET status = 'in_progress' WHERE id = ?`, taskID); err != nil {
		t.Fatalf("park: %v", err)
	}

	s.advanceTaskFromRunStatus(runmode.LocalDefaultOrg, runID, "task_unsolvable")

	if got := readTaskStatus(t, database, taskID); got != "in_progress" {
		t.Errorf("status = %q, want in_progress (task_unsolvable must not transition)", got)
	}
}

// User takeover replaced the bot claim while the run was in flight —
// the user owns the lifecycle now, and the spawner's mirror must
// leave their card alone even if a stale run event arrives.
func TestAdvanceTaskFromRunStatus_UserClaimedTaskIgnored(t *testing.T) {
	s, database, runID, taskID := setupAdvanceFixture(t, "user-claim")
	stampUserClaim(t, database, taskID)

	s.advanceTaskFromRunStatus(runmode.LocalDefaultOrg, runID, "pending_approval")

	if got := readTaskStatus(t, database, taskID); got != "queued" {
		t.Errorf("status = %q, want queued (user-claimed task must not auto-advance)", got)
	}
}

// Unclaimed task — same idea, the spawner mustn't mirror state onto
// rows it doesn't own. This shouldn't happen in production (a run
// without a claim means the claim was cleared mid-flight, e.g. by
// requeue) but the helper guards anyway.
func TestAdvanceTaskFromRunStatus_UnclaimedTaskIgnored(t *testing.T) {
	s, database, runID, taskID := setupAdvanceFixture(t, "unclaimed")

	s.advanceTaskFromRunStatus(runmode.LocalDefaultOrg, runID, "completed")

	if got := readTaskStatus(t, database, taskID); got != "queued" {
		t.Errorf("status = %q, want queued (unclaimed task must not be closed)", got)
	}
}

// Already-terminal task: a late run event arrives after the task
// reached done/dismissed via a user swipe or requeue cleanup. The
// helper short-circuits so a late completion doesn't re-open or
// re-close the row.
func TestAdvanceTaskFromRunStatus_AlreadyTerminalIgnored(t *testing.T) {
	s, database, runID, taskID := setupAdvanceFixture(t, "already-done")
	stampBotClaim(t, database, taskID)
	if _, err := database.Exec(`UPDATE tasks SET status = 'dismissed' WHERE id = ?`, taskID); err != nil {
		t.Fatalf("park: %v", err)
	}

	s.advanceTaskFromRunStatus(runmode.LocalDefaultOrg, runID, "completed")

	if got := readTaskStatus(t, database, taskID); got != "dismissed" {
		t.Errorf("status = %q, want dismissed (terminal task must not flip)", got)
	}
}

// Re-delegation race: an older run reaches pending_approval or
// completed AFTER a newer run is already active on the same task.
// processCompletion's inline 'completed' path has a
// HasOtherActiveRunForTaskSystem guard so the older run doesn't
// clobber the newer one's lifecycle; this mirror has to share that
// guard or the same stale-run problem walks the task to in_review/
// done while the newer run is still working.
func TestAdvanceTaskFromRunStatus_StaleRunNoOpWhenNewerActive(t *testing.T) {
	s, database, runID, taskID := setupAdvanceFixture(t, "stale-completed")
	stampBotClaim(t, database, taskID)
	// Park the task in in_progress (the newer run's state).
	if _, err := database.Exec(`UPDATE tasks SET status = 'in_progress' WHERE id = ?`, taskID); err != nil {
		t.Fatalf("park: %v", err)
	}
	// Seed a second active run on the same task. Use a unique run id
	// + session id to avoid colliding with the original fixture.
	if _, err := database.Exec(`
		INSERT INTO runs (id, task_id, prompt_id, status, trigger_type, session_id, worktree_path)
		VALUES (?, ?, 'test-prompt', 'running', 'manual', 'sess-newer', '/tmp/wt-newer')
	`, "r-newer", taskID); err != nil {
		t.Fatalf("seed newer run: %v", err)
	}

	// The OLDER run (runID from the fixture) reaches completed.
	s.advanceTaskFromRunStatus(runmode.LocalDefaultOrg, runID, "completed")

	if got := readTaskStatus(t, database, taskID); got != "in_progress" {
		t.Errorf("status = %q, want in_progress (stale completed run must not close a task with a newer active run)", got)
	}

	// Same scenario for pending_approval: older run produces a
	// review while the newer run keeps working. The task must not
	// jump to in_review.
	s.advanceTaskFromRunStatus(runmode.LocalDefaultOrg, runID, "pending_approval")
	if got := readTaskStatus(t, database, taskID); got != "in_progress" {
		t.Errorf("status = %q, want in_progress (stale pending_approval must not move task to in_review)", got)
	}
}

// Blueprint steps: the blueprint orchestrator owns task lifecycle for the
// whole blueprint. Mid-blueprint step terminals must not flip the task —
// terminateBlueprint handles closure when the blueprint itself terminates.
func TestAdvanceTaskFromRunStatus_ChainStepIgnored(t *testing.T) {
	s, database, runID, taskID := setupAdvanceFixture(t, "chain-step")
	stampBotClaim(t, database, taskID)
	// Seed a blueprint + blueprint_runs row first (FK requirements), then
	// point the existing run at it so advanceTaskFromRunStatus's
	// blueprint-step guard trips.
	if err := sqlitestore.New(database).Blueprints.Create(context.Background(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Blueprint{
		ID: "bp-abc", Name: "bp-abc", Source: "user", TeamID: runmode.LocalDefaultTeamID,
	}); err != nil {
		t.Fatalf("seed blueprint: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, worktree_path)
		 VALUES (?, ?, ?, 'manual', ?)`,
		"chain-abc", "bp-abc", taskID, "/tmp/wt-chain",
	); err != nil {
		t.Fatalf("seed blueprint_runs: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE runs SET blueprint_run_id = ? WHERE id = ?`,
		"chain-abc", runID,
	); err != nil {
		t.Fatalf("set blueprint_run_id: %v", err)
	}

	s.advanceTaskFromRunStatus(runmode.LocalDefaultOrg, runID, "pending_approval")

	if got := readTaskStatus(t, database, taskID); got != "queued" {
		t.Errorf("status = %q, want queued (chain step terminal must not flip the task; chain orchestrator owns it)", got)
	}
}

// --- helpers ---

// setupAdvanceFixture creates a fresh spawner + seeded run+task pair
// and returns the run/task ids so each test can mutate the claim or
// pre-state independently. Wraps seedRun (already covers the
// entity/event/task/run chain) with a unique suffix per test.
func setupAdvanceFixture(t *testing.T, suffix string) (*Spawner, *sql.DB, string, string) {
	t.Helper()
	database := newTakeoverTestDB(t)
	// Bot claim writes require an agents row to satisfy the FK on
	// claimed_by_agent_id. newTakeoverTestDB doesn't seed one; the
	// production codepath (BootstrapLocalAgent in main) does. Same
	// helper, same idempotent insert.
	if err := db.BootstrapLocalAgent(context.Background(), sqlitestore.New(database)); err != nil {
		t.Fatalf("bootstrap local agent: %v", err)
	}
	runID := "r-adv-" + suffix
	seedRun(t, database, runID, "sess-"+suffix, "/tmp/wt-adv-"+suffix)
	var taskID string
	if err := database.QueryRow(`SELECT task_id FROM runs WHERE id = ?`, runID).Scan(&taskID); err != nil {
		t.Fatalf("lookup task_id: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6", "")
	return s, database, runID, taskID
}

func stampBotClaim(t *testing.T, database *sql.DB, taskID string) {
	t.Helper()
	if _, err := database.Exec(
		`UPDATE tasks SET claimed_by_agent_id = ?, claimed_by_user_id = NULL WHERE id = ?`,
		runmode.LocalDefaultAgentID, taskID,
	); err != nil {
		t.Fatalf("stamp bot claim: %v", err)
	}
}

func stampUserClaim(t *testing.T, database *sql.DB, taskID string) {
	t.Helper()
	if _, err := database.Exec(
		`UPDATE tasks SET claimed_by_user_id = ?, claimed_by_agent_id = NULL WHERE id = ?`,
		runmode.LocalDefaultUserID, taskID,
	); err != nil {
		t.Fatalf("stamp user claim: %v", err)
	}
}

func readTaskStatus(t *testing.T, database *sql.DB, taskID string) string {
	t.Helper()
	var status string
	if err := database.QueryRow(`SELECT status FROM tasks WHERE id = ?`, taskID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	return status
}

// Ensure the helper signature still matches what the targetTaskStatusForRunStatus
// pure function expects — if someone changes the helper's signature, the
// build will catch it here rather than at the call site.
var _ = func() {
	_, _ = targetTaskStatusForRunStatus("running")
	_ = context.Background
}
