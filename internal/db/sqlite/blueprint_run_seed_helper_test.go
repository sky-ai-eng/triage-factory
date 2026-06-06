package sqlite_test

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// seedBlueprintRunForRun mints a blueprint + blueprint_run pointed at the
// given task so a `runs` row can satisfy the now NOT-NULL
// runs.blueprint_run_id FK (→ blueprint_runs(id)). Returns the
// blueprint_run id to drop into the run insert's blueprint_run_id column.
//
// Shared across the package-sqlite CRUD test files (factory, pending_prs,
// prompts, run_worktrees, task_memory) whose `runs` fixtures are not the
// system under test — they just need a valid FK target.
func seedBlueprintRunForRun(t *testing.T, conn *sql.DB, taskID string) string {
	t.Helper()
	blueprintID := "bp_" + uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO blueprints (id, name, source, creator_user_id, team_id)
		VALUES (?, 'bp', 'user', ?, ?)
	`, blueprintID, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID); err != nil {
		t.Fatalf("seed blueprint: %v", err)
	}
	blueprintRunID := uuid.New().String()
	// step_plan is NOT NULL — stamp a minimal one-step frozen plan so the FK
	// fixture satisfies the constraint. These fixtures never dispatch, so the
	// plan content is inert; it just has to be valid JSON.
	stepPlan, err := domain.MarshalStepPlan([]domain.BlueprintPlanStep{
		{StepIndex: 0, PromptID: "seed-step", PromptName: "seed", PromptBody: "x", Source: "user"},
	})
	if err != nil {
		t.Fatalf("marshal seed step plan: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, status, worktree_path, step_plan)
		VALUES (?, ?, ?, 'manual', 'running', ?, ?)
	`, blueprintRunID, blueprintID, taskID, "/tmp/wt-"+blueprintRunID, stepPlan); err != nil {
		t.Fatalf("seed blueprint_run: %v", err)
	}
	return blueprintRunID
}
