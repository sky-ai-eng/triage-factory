package sqlite_test

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// seedBlueprintRunForConversation mints a blueprint + blueprint_run pointed at the
// given task so a `conversations` row can reference blueprint_runs(id). The
// column is nullable, but the conversations_origin_requires_parents CHECK
// requires it (plus task_id/prompt_id) to be set when origin='blueprint'
// (the default). Returns the blueprint_run id to drop into the run insert's
// blueprint_run_id column.
//
// Shared across the package-sqlite CRUD test files (factory, prompts,
// conversation_worktrees, task_memory) whose `conversations` fixtures are
// not the system under test — they just need a valid FK target.
func seedBlueprintRunForConversation(t *testing.T, conn *sql.DB, taskID string) string {
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
	// One running blueprint_run per task is a schema invariant
	// (blueprint_runs_one_active_run_per_task), so staging a task's next
	// engagement settles the one before it — which is what the task moving on
	// means.
	if _, err := conn.Exec(`UPDATE blueprint_runs SET status = 'completed' WHERE task_id = ? AND status = 'running'`, taskID); err != nil {
		t.Fatalf("settle the task's prior blueprint_run: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, status, worktree_path, step_plan)
		VALUES (?, ?, ?, 'manual', 'running', ?, ?)
	`, blueprintRunID, blueprintID, taskID, "/tmp/wt-"+blueprintRunID, stepPlan); err != nil {
		t.Fatalf("seed blueprint_run: %v", err)
	}
	return blueprintRunID
}

// insertBlueprintRunForTest inserts a blueprint_runs row directly — the test
// fixture stand-in for the mint inside a BlueprintStore door, staging a run in
// arbitrary shape without staging a whole firing for each. Production writes
// this table through CreateRunWithFirstStepSystem alone, which commits a first
// step with it; a fixture whose subject is the run row itself, or one that
// stages its step conversations by hand beside it, wants neither that step nor
// the doorbell the door rings for it.
//
// The trigger_type↔creator CHECK is satisfied by pairing 'manual' with the
// sentinel user and 'event' with NULL. Fields honored: ID (minted when empty),
// BlueprintID, TaskID, TriggerType (default manual), TriggerID, ActorAgentID,
// Status (default running), StepPlan, WorktreePath. Returns the run id.
//
// One running blueprint_run per task is a schema invariant, so the task's
// prior run is settled first — which is what a task moving on to its next
// engagement means.
func insertBlueprintRunForTest(t *testing.T, conn *sql.DB, br domain.BlueprintRun) string {
	t.Helper()
	if br.ID == "" {
		br.ID = uuid.New().String()
	}
	if br.TriggerType == "" {
		br.TriggerType = domain.BlueprintTriggerManual
	}
	if br.Status == "" {
		br.Status = domain.BlueprintRunStatusRunning
	}
	var creator any
	if br.TriggerType != domain.BlueprintTriggerEvent {
		creator = runmode.LocalDefaultUserID
	}
	stepPlan, err := domain.MarshalStepPlan(br.StepPlan)
	if err != nil {
		t.Fatalf("marshal step plan: %v", err)
	}
	if _, err := conn.Exec(`UPDATE blueprint_runs SET status = 'completed' WHERE task_id = ? AND status = 'running'`, br.TaskID); err != nil {
		t.Fatalf("settle the task's prior blueprint_run: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, trigger_id,
		                            actor_agent_id, status, step_plan, worktree_path,
		                            creator_user_id, started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
	`, br.ID, br.BlueprintID, br.TaskID, br.TriggerType, nullIfEmptyForTest(br.TriggerID),
		nullIfEmptyForTest(br.ActorAgentID), br.Status, stepPlan, br.WorktreePath, creator); err != nil {
		t.Fatalf("insert blueprint_run %s: %v", br.ID, err)
	}
	return br.ID
}

// nullIfEmptyForTest binds "" as SQL NULL, which is what a nullable column
// holding "no value" carries.
func nullIfEmptyForTest(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// insertConversationForTest inserts a conversations row directly — the test
// fixture stand-in for the mint inside a BlueprintStore door, staging rows in
// arbitrary status without staging a whole firing for each. The trigger_type↔creator CHECK is satisfied by pairing
// 'manual' with the sentinel user and 'event' with NULL. Fields honored:
// ID, TaskID, PromptID, Status, Model, TriggerType, TriggerID,
// BlueprintRunID, BlueprintStepIndex.
func insertConversationForTest(t *testing.T, conn *sql.DB, conv domain.Conversation) {
	t.Helper()
	trigger := conv.TriggerType
	if trigger == "" {
		trigger = "manual"
	}
	var creator any
	if trigger == "manual" {
		creator = runmode.LocalDefaultUserID
	}
	var triggerID any
	if conv.TriggerID != "" {
		triggerID = conv.TriggerID
	}
	var stepIdx any
	if conv.BlueprintStepIndex != nil {
		stepIdx = *conv.BlueprintStepIndex
	}
	// An empty Status writes SQL NULL — the mid-flight state, which is what
	// an unconcluded conversation carries — not an empty string, which is not
	// a status at all.
	var status any
	if conv.Status != "" {
		status = conv.Status
	}
	if _, err := conn.Exec(`
		INSERT INTO conversations (id, task_id, prompt_id, status, model,
		                           trigger_type, trigger_id, team_id, visibility,
		                           creator_user_id, blueprint_run_id, blueprint_step_index, queued_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'team', ?, ?, ?, CURRENT_TIMESTAMP)
	`, conv.ID, conv.TaskID, conv.PromptID, status, conv.Model,
		trigger, triggerID, runmode.LocalDefaultTeamID, creator, conv.BlueprintRunID, stepIdx); err != nil {
		t.Fatalf("insert conversation %s: %v", conv.ID, err)
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
