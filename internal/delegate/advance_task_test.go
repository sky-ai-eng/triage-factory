package delegate

import (
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// recomputeTaskBoardColumn is the blueprint-era board placement rule: a
// bot-claimed task's live column is a recomputed aggregate over its active
// blueprint_run's step runs — in_review if the blueprint has an unresolved
// artifact (a draft PR / ready review, the derived approval signal that replaced
// pending_approval) or any run is parked open, else in_progress.
// Terminal columns (done / leave-open) are owned by terminateBlueprint, not this
// helper. These tests pin the aggregate directly by mutating run state and
// invoking the recompute, without spawning a real agent.
//
// setupAdvanceFixture seeds an entity + event + task + a 1-step blueprint_run
// whose single run starts 'running' (see seedRun → seedRunBlueprint).

func TestRecomputeBoard_RunningSetsInProgress(t *testing.T) {
	s, database, _, taskID := setupAdvanceFixture(t, "ip")
	stampBotClaim(t, database, taskID)

	s.recomputeTaskBoardColumn(runmode.LocalDefaultOrgID, taskID)

	if got := readTaskStatus(t, database, taskID); got != "in_progress" {
		t.Errorf("task.status = %q, want in_progress (an unparked active run)", got)
	}
}

// A parked run — `open` — moves the aggregate to in_review.
func TestRecomputeBoard_OpenSetsInReview(t *testing.T) {
	s, database, runID, taskID := setupAdvanceFixture(t, "ai")
	stampBotClaim(t, database, taskID)
	setRunStatus(t, database, runID, "open")

	s.recomputeTaskBoardColumn(runmode.LocalDefaultOrgID, taskID)

	if got := readTaskStatus(t, database, taskID); got != "in_review" {
		t.Errorf("task.status = %q, want in_review (open parks → needs 👀)", got)
	}
}

// An unresolved artifact (a draft PR) is the derived approval signal and lands
// the task in the same "needs 👀" column even though no run is parked open — the
// successor to the pending_approval run status.
func TestRecomputeBoard_UnresolvedArtifactSetsInReview(t *testing.T) {
	s, database, runID, taskID := setupAdvanceFixture(t, "unresolved")
	stampBotClaim(t, database, taskID)
	// Run stays running; the draft PR it queued is what surfaces the approval column.
	seedDraftPRArtifact(t, s, runID)

	s.recomputeTaskBoardColumn(runmode.LocalDefaultOrgID, taskID)

	if got := readTaskStatus(t, database, taskID); got != "in_review" {
		t.Errorf("task.status = %q, want in_review (unresolved draft PR → needs 👀)", got)
	}
}

// Bouncing across multiple human-interaction points: in_progress → in_review
// (park) → in_progress (resume) → in_review (park again). The aggregate follows
// the run state each time, no step-count gate.
func TestRecomputeBoard_BouncesAcrossInteractionPoints(t *testing.T) {
	s, database, runID, taskID := setupAdvanceFixture(t, "bounce")
	stampBotClaim(t, database, taskID)

	s.recomputeTaskBoardColumn(runmode.LocalDefaultOrgID, taskID)
	if got := readTaskStatus(t, database, taskID); got != "in_progress" {
		t.Fatalf("initial: status = %q, want in_progress", got)
	}

	setRunStatus(t, database, runID, "open")
	s.recomputeTaskBoardColumn(runmode.LocalDefaultOrgID, taskID)
	if got := readTaskStatus(t, database, taskID); got != "in_review" {
		t.Fatalf("after open: status = %q, want in_review", got)
	}

	setRunStatus(t, database, runID, "running")
	s.recomputeTaskBoardColumn(runmode.LocalDefaultOrgID, taskID)
	if got := readTaskStatus(t, database, taskID); got != "in_progress" {
		t.Fatalf("after resume: status = %q, want in_progress", got)
	}

	setRunStatus(t, database, runID, "open")
	s.recomputeTaskBoardColumn(runmode.LocalDefaultOrgID, taskID)
	if got := readTaskStatus(t, database, taskID); got != "in_review" {
		t.Fatalf("after second park: status = %q, want in_review", got)
	}
}

// Multi-step parity: a blueprint_run with several step runs shows ONE card.
// An earlier step completed + the current step parked → in_review (any parked).
// All-unparked → in_progress. Same rule as 1-step; more runs just feed it.
func TestRecomputeBoard_MultiStepAggregate(t *testing.T) {
	s, database, runID, taskID := setupAdvanceFixture(t, "multi")
	stampBotClaim(t, database, taskID)
	brID := blueprintRunIDForRun(t, database, runID)
	// Step 0 (the seeded run) completed; add step 1, currently running.
	setRunStatus(t, database, runID, "completed")
	addStepRun(t, database, brID, taskID, "step1-multi", 1, "running")

	s.recomputeTaskBoardColumn(runmode.LocalDefaultOrgID, taskID)
	if got := readTaskStatus(t, database, taskID); got != "in_progress" {
		t.Errorf("all-unparked: status = %q, want in_progress", got)
	}

	// Now step 1 parks open → in_review (any parked run flips the aggregate).
	setRunStatus(t, database, "step1-multi", "open")
	s.recomputeTaskBoardColumn(runmode.LocalDefaultOrgID, taskID)
	if got := readTaskStatus(t, database, taskID); got != "in_review" {
		t.Errorf("step parked: status = %q, want in_review", got)
	}
}

// A user claim is column-neutral: a user-claimed task is owned by the user, so
// the recompute leaves their card alone even with a parked run.
func TestRecomputeBoard_UserClaimedTaskNeutral(t *testing.T) {
	s, database, runID, taskID := setupAdvanceFixture(t, "user-claim")
	stampUserClaim(t, database, taskID)
	setRunStatus(t, database, runID, "open")

	s.recomputeTaskBoardColumn(runmode.LocalDefaultOrgID, taskID)

	if got := readTaskStatus(t, database, taskID); got != "queued" {
		t.Errorf("status = %q, want queued (user-claimed task must not auto-move)", got)
	}
}

// Unclaimed task — the spawner mustn't place rows it doesn't own.
func TestRecomputeBoard_UnclaimedTaskNeutral(t *testing.T) {
	s, database, _, taskID := setupAdvanceFixture(t, "unclaimed")

	s.recomputeTaskBoardColumn(runmode.LocalDefaultOrgID, taskID)

	if got := readTaskStatus(t, database, taskID); got != "queued" {
		t.Errorf("status = %q, want queued (unclaimed task must not move)", got)
	}
}

// Already-terminal task: a late transition must not reopen a done/dismissed row.
func TestRecomputeBoard_AlreadyTerminalNeutral(t *testing.T) {
	s, database, runID, taskID := setupAdvanceFixture(t, "already-done")
	stampBotClaim(t, database, taskID)
	if _, err := database.Exec(`UPDATE tasks SET status = 'dismissed' WHERE id = ?`, taskID); err != nil {
		t.Fatalf("park: %v", err)
	}
	setRunStatus(t, database, runID, "open")

	s.recomputeTaskBoardColumn(runmode.LocalDefaultOrgID, taskID)

	if got := readTaskStatus(t, database, taskID); got != "dismissed" {
		t.Errorf("status = %q, want dismissed (terminal task must not flip)", got)
	}
}

// No active blueprint_run: the live column is owned by no one here —
// terminateBlueprint owns the terminal column, so recompute is a no-op and
// leaves the task where it is.
func TestRecomputeBoard_NoActiveBlueprintRunNeutral(t *testing.T) {
	s, database, runID, taskID := setupAdvanceFixture(t, "noactive")
	stampBotClaim(t, database, taskID)
	if _, err := database.Exec(`UPDATE tasks SET status = 'in_progress' WHERE id = ?`, taskID); err != nil {
		t.Fatalf("park: %v", err)
	}
	// Terminate the blueprint_run so there is no active run for the task.
	brID := blueprintRunIDForRun(t, database, runID)
	if _, err := database.Exec(`UPDATE blueprint_runs SET status = 'completed' WHERE id = ?`, brID); err != nil {
		t.Fatalf("complete blueprint_run: %v", err)
	}
	setRunStatus(t, database, runID, "completed")

	s.recomputeTaskBoardColumn(runmode.LocalDefaultOrgID, taskID)

	if got := readTaskStatus(t, database, taskID); got != "in_progress" {
		t.Errorf("status = %q, want in_progress (no active blueprint_run → recompute is a no-op)", got)
	}
}

// --- helpers ---

// setupAdvanceFixture creates a fresh spawner + seeded run+task pair (with its
// 1-step blueprint_run) and returns the run/task ids so each test can mutate run
// state or the claim independently.
func setupAdvanceFixture(t *testing.T, suffix string) (*Spawner, *sql.DB, string, string) {
	t.Helper()
	database := newDelegateTestDB(t)
	// Bot claim writes require an agents row to satisfy the FK on
	// claimed_by_agent_id. newDelegateTestDB seeds the tenant (orgs/teams)
	// via BootstrapSchemaForTest but not the agent — production seeds that
	// through the explicit provision action (BootstrapLocalOrg). Insert
	// the sentinel agent + team_agents rows directly so the test reaches
	// the claim logic under test.
	if _, err := database.Exec(
		`INSERT OR IGNORE INTO agents (id, org_id, display_name) VALUES (?, ?, 'Triage Factory Bot')`,
		runmode.LocalDefaultAgentID, runmode.LocalDefaultOrgID,
	); err != nil {
		t.Fatalf("seed local agent: %v", err)
	}
	if _, err := database.Exec(
		`INSERT OR IGNORE INTO team_agents (team_id, agent_id, enabled) VALUES (?, ?, 1)`,
		runmode.LocalDefaultTeamID, runmode.LocalDefaultAgentID,
	); err != nil {
		t.Fatalf("seed local team_agents: %v", err)
	}
	runID := "r-adv-" + suffix
	seedRun(t, database, runID, "sess-"+suffix, "/tmp/wt-adv-"+suffix)
	var taskID string
	if err := database.QueryRow(`SELECT task_id FROM runs WHERE id = ?`, runID).Scan(&taskID); err != nil {
		t.Fatalf("lookup task_id: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
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

func setRunStatus(t *testing.T, database *sql.DB, runID, status string) {
	t.Helper()
	if _, err := database.Exec(`UPDATE runs SET status = ? WHERE id = ?`, status, runID); err != nil {
		t.Fatalf("set run status: %v", err)
	}
}

func blueprintRunIDForRun(t *testing.T, database *sql.DB, runID string) string {
	t.Helper()
	var brID string
	if err := database.QueryRow(`SELECT blueprint_run_id FROM runs WHERE id = ?`, runID).Scan(&brID); err != nil {
		t.Fatalf("read blueprint_run_id: %v", err)
	}
	return brID
}

// addStepRun appends another step run to an existing blueprint_run so the
// multi-step aggregate can be exercised.
func addStepRun(t *testing.T, database *sql.DB, blueprintRunID, taskID, runID string, stepIndex int, status string) {
	t.Helper()
	if _, err := database.Exec(`
		INSERT INTO runs (id, task_id, prompt_id, status, trigger_type, team_id, visibility,
		                  creator_user_id, worktree_path, blueprint_run_id, blueprint_step_index)
		VALUES (?, ?, 'test-prompt', ?, 'manual', ?, 'team', ?, '/tmp/wt-step', ?, ?)
	`, runID, taskID, status, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID, blueprintRunID, stepIndex); err != nil {
		t.Fatalf("add step run: %v", err)
	}
}
