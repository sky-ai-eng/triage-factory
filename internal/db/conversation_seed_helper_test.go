package db

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// mintBlueprintRunForTest inserts a blueprint + blueprint_run pair via raw SQL
// so a conversations row can satisfy the origin CHECK's blueprint_run_id FK without
// reaching for the BlueprintStore impl (which lives in internal/db/sqlite and
// would form a circular import in package db). Returns the blueprint_run id.
//
// taskID may be empty for tests that don't have a real task FK; blueprint_runs
// has no FK requiring task_id to resolve in the SQLite baseline.

// nullIfEmpty maps an empty string to a SQL NULL bind — test-local now that
// the last production raw-SQL file retired its shared copy.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func mintBlueprintRunForTest(t *testing.T, database *sql.DB, taskID string) string {
	t.Helper()
	bpID := uuid.New().String()
	bpRunID := uuid.New().String()
	if _, err := database.Exec(`
		INSERT INTO blueprints (id, team_id, name, source, creator_user_id)
		VALUES (?, ?, 'bp', 'user', ?)
	`, bpID, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("mintBlueprintRunForTest: insert blueprint: %v", err)
	}
	if _, err := database.Exec(`
		INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, status, worktree_path, step_plan)
		VALUES (?, ?, ?, 'manual', 'running', '/tmp/wt', '[]')
	`, bpRunID, bpID, nullIfEmpty(taskID)); err != nil {
		t.Fatalf("mintBlueprintRunForTest: insert blueprint_run: %v", err)
	}
	return bpRunID
}

// createConversationForTest inserts a conversations row directly via raw SQL so
// package-db CRUD tests (pending_firings, conversation_worktrees, ...) have
// a conversation to FK-point to without reaching for the ConversationStore
// impl
// (which lives in internal/db/sqlite and would form a circular
// import if pulled into package db).
//
// Mirrors the post-D2 raw-SQL test seeders (seedTaskForTest in
// task_seed_helper_test.go). Each consumer here is testing a
// different table — the conversations are just fixtures, not the system under
// test — so this minimal insert is enough.
//
// promptID may be empty; the raw SQL inserts NULL, which the
// pre-D2 CreateConversation also tolerated via nullIfEmpty. Tests
// that need a real prompt FK should seed one first and pass the
// id explicitly.
func createConversationForTest(t *testing.T, database *sql.DB, conv domain.Conversation) error {
	t.Helper()
	triggerType := conv.TriggerType
	if triggerType == "" {
		triggerType = "manual"
	}
	creator := conv.CreatorUserID
	if triggerType == "manual" && creator == "" {
		creator = runmode.LocalDefaultUserID
	}
	var stepIdx any
	if conv.BlueprintStepIndex != nil {
		stepIdx = *conv.BlueprintStepIndex
	}
	// The origin CHECK requires blueprint_run_id — mint a blueprint_run for
	// this conversation's
	// task when the caller didn't supply one. This single fallback fixes the
	// many package-db consumers that build a conversation fixture without
	// caring about
	// the blueprint layer.
	blueprintRunID := conv.BlueprintRunID
	if blueprintRunID == "" {
		blueprintRunID = mintBlueprintRunForTest(t, database, conv.TaskID)
	}
	_, err := database.Exec(`
		INSERT INTO conversations (id, task_id, prompt_id, status, model, worktree_path,
		                  trigger_type, trigger_id, team_id, visibility,
		                  creator_user_id, actor_agent_id, blueprint_run_id, blueprint_step_index)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'team', ?, ?, ?, ?)
	`, conv.ID, conv.TaskID, nullIfEmpty(conv.PromptID), conv.Status, conv.Model, conv.WorktreePath,
		triggerType, nullIfEmpty(conv.TriggerID), runmode.LocalDefaultTeamID,
		nullIfEmpty(creator), nullIfEmpty(conv.ActorAgentID),
		blueprintRunID, stepIdx)
	return err
}
