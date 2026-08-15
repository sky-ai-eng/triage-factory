package sqlite_test

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// seedBlueprintRunForRun mints a blueprint + blueprint_run pointed at the
// given task so a `runs` row can reference blueprint_runs(id). The column is
// nullable, but the conversations_origin_requires_parents CHECK requires it (plus
// task_id/prompt_id) to be set when origin='blueprint' (the default). Returns
// the blueprint_run id to drop into the run insert's blueprint_run_id column.
//
// Shared across the package-sqlite CRUD test files (factory,
// prompts, conversation_worktrees, task_memory) whose `runs` fixtures are not the
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

// insertConversationForTest inserts a conversations row directly — the test
// fixture stand-in for the queue's EnqueueRun mint, staging rows in
// arbitrary status. The trigger_type↔creator CHECK is satisfied by pairing
// 'manual' with the sentinel user and 'event' with NULL. Fields honored:
// ID, TaskID, PromptID, Status, Model, TriggerType, TriggerID,
// BlueprintRunID, BlueprintStepIndex.
func insertConversationForTest(t *testing.T, conn *sql.DB, run domain.Conversation) {
	t.Helper()
	trigger := run.TriggerType
	if trigger == "" {
		trigger = "manual"
	}
	var creator any
	if trigger == "manual" {
		creator = runmode.LocalDefaultUserID
	}
	var triggerID any
	if run.TriggerID != "" {
		triggerID = run.TriggerID
	}
	var stepIdx any
	if run.BlueprintStepIndex != nil {
		stepIdx = *run.BlueprintStepIndex
	}
	if _, err := conn.Exec(`
		INSERT INTO conversations (id, task_id, prompt_id, status, model,
		                           trigger_type, trigger_id, team_id, visibility,
		                           creator_user_id, blueprint_run_id, blueprint_step_index)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'team', ?, ?, ?)
	`, run.ID, run.TaskID, run.PromptID, run.Status, run.Model,
		trigger, triggerID, runmode.LocalDefaultTeamID, creator, run.BlueprintRunID, stepIdx); err != nil {
		t.Fatalf("insert conversation %s: %v", run.ID, err)
	}
}

// insertActiveClaimForTest stamps prior-engagement ownership the way the
// dispatcher's claim would: one active claims row for the conversation.
func insertActiveClaimForTest(t *testing.T, conn *sql.DB, conversationID, executorID string, bootEpoch int64) {
	t.Helper()
	if _, err := conn.Exec(`
		INSERT INTO claims (id, conversation_id, executor_id, boot_epoch)
		VALUES (?, ?, ?, ?)
	`, uuid.New().String(), conversationID, executorID, bootEpoch); err != nil {
		t.Fatalf("insert claim for %s: %v", conversationID, err)
	}
}
