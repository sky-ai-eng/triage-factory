package db

import (
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// createRunForTest inserts a run row directly via raw SQL so
// package-db CRUD tests (pending_firings, run_worktrees, ...) have
// a run to FK-point to without reaching for the AgentRunStore impl
// (which lives in internal/db/sqlite and would form a circular
// import if pulled into package db).
//
// Mirrors the post-D2 raw-SQL test seeders (seedTaskForTest in
// task_seed_helper_test.go). Each consumer here is testing a
// different table — runs are just fixtures, not the system under
// test — so this minimal insert is enough.
//
// promptID may be empty; the raw SQL inserts NULL, which the
// pre-D2 CreateAgentRun also tolerated via nullIfEmpty. Tests
// that need a real prompt FK should seed one first and pass the
// id explicitly.
func createRunForTest(t *testing.T, database *sql.DB, run domain.AgentRun) error {
	t.Helper()
	triggerType := run.TriggerType
	if triggerType == "" {
		triggerType = "manual"
	}
	creator := run.CreatorUserID
	if triggerType == "manual" && creator == "" {
		creator = runmode.LocalDefaultUserID
	}
	var stepIdx any
	if run.BlueprintStepIndex != nil {
		stepIdx = *run.BlueprintStepIndex
	}
	_, err := database.Exec(`
		INSERT INTO runs (id, task_id, prompt_id, status, model, worktree_path,
		                  trigger_type, trigger_id, team_id, visibility,
		                  creator_user_id, actor_agent_id, blueprint_run_id, blueprint_step_index)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'team', ?, ?, ?, ?)
	`, run.ID, run.TaskID, nullIfEmpty(run.PromptID), run.Status, run.Model, run.WorktreePath,
		triggerType, nullIfEmpty(run.TriggerID), runmode.LocalDefaultTeamID,
		nullIfEmpty(creator), nullIfEmpty(run.ActorAgentID),
		nullIfEmpty(run.BlueprintRunID), stepIdx)
	return err
}
