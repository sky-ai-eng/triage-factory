package delegate

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	_ "modernc.org/sqlite"
)

// newDelegateTestDB spins up an in-memory SQLite with the full schema
// so tests can seed runs in any state they care about. Forcing
// single-conn because :memory: is per-conn — a pooled second
// connection would see an empty schema.
func newDelegateTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { database.Close() })
	if err := db.BootstrapSchemaForTest(database); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return database
}

// seedRunBlueprint mints a blueprint + blueprint_run for taskID and returns the
// blueprint_run id, so run fixtures can satisfy runs.blueprint_run_id NOT NULL.
// The suffix keeps ids unique + deterministic per fixture; the ids are distinct
// from makeRunBlueprintStep's ("bp-"/"bpr-") so a test can re-point a run onto a
// specific blueprint_run without colliding with the one seedRun attached.
func seedRunBlueprint(t *testing.T, database *sql.DB, suffix, taskID string) string {
	t.Helper()
	bpID := "seedbp-" + suffix
	if err := sqlitestore.New(database).Blueprints.Create(context.Background(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Blueprint{
		ID: bpID, Name: bpID, Source: "user", TeamID: runmode.LocalDefaultTeamID,
	}); err != nil {
		t.Fatalf("seed blueprint: %v", err)
	}
	brID := "seedbpr-" + suffix
	if _, err := database.Exec(
		`INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, worktree_path, step_plan) VALUES (?, ?, ?, 'manual', ?, '[]')`,
		brID, bpID, taskID, "/tmp/wt-"+brID); err != nil {
		t.Fatalf("seed blueprint_run: %v", err)
	}
	return brID
}

// seedRun inserts a run (plus its entity/event/task/blueprint fixtures)
// with the requested id, session id, and worktree path.
// We bypass the spawner's Delegate flow because these tests don't need
// a real goroutine — only a row in the runs table. Every run is a
// blueprint step now (runs.blueprint_run_id NOT NULL), so it mints a 1-step
// blueprint_run and links the run to it.
func seedRun(t *testing.T, database *sql.DB, runID, sessionID, worktreePath string) {
	t.Helper()
	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#"+runID, "pr", "T", "https://example.com/"+runID)
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	eventID, err := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrg, domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entity.ID,
		MetadataJSON: `{"check_name":"build"}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := testTaskStore(database).FindOrCreate(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, entity.ID, domain.EventGitHubPRCICheckFailed, runID, eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	ensureTestPrompt(t, database, domain.Prompt{ID: "test-prompt", Name: "T", Body: "x", Source: "user"})
	brID := seedRunBlueprint(t, database, runID, task.ID)
	stepIdx := 0
	if err := sqlitestore.New(database).AgentRuns.Create(t.Context(), runmode.LocalDefaultOrg, domain.AgentRun{
		ID:                 runID,
		TaskID:             task.ID,
		PromptID:           "test-prompt",
		Status:             "running",
		Model:              "claude-sonnet-4-6",
		WorktreePath:       worktreePath,
		BlueprintRunID:     brID,
		BlueprintStepIndex: &stepIdx,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := database.Exec(`UPDATE runs SET status = 'running', session_id = ?, worktree_path = ? WHERE id = ?`, sessionID, worktreePath, runID); err != nil {
		t.Fatalf("update run: %v", err)
	}
}

// seedJiraRun is the Jira variant of seedRun: the task's entity is
// jira-sourced so source-gated paths see a Jira run rather than a
// GitHub PR run.
func seedJiraRun(t *testing.T, database *sql.DB, runID, sessionID, worktreePath string) {
	t.Helper()
	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "jira", "SKY-"+runID, "issue", "T-"+runID, "https://x/"+runID)
	if err != nil {
		t.Fatalf("create jira entity: %v", err)
	}
	eventID, err := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrg, domain.Event{
		EventType:    domain.EventJiraIssueAssigned,
		EntityID:     &entity.ID,
		MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := testTaskStore(database).FindOrCreate(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, entity.ID, domain.EventJiraIssueAssigned, runID, eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	ensureTestPrompt(t, database, domain.Prompt{ID: "test-prompt", Name: "T", Body: "x", Source: "user"})
	brID := seedRunBlueprint(t, database, runID, task.ID)
	stepIdx := 0
	if err := sqlitestore.New(database).AgentRuns.Create(t.Context(), runmode.LocalDefaultOrg, domain.AgentRun{
		ID: runID, TaskID: task.ID, PromptID: "test-prompt",
		Status: "running", Model: "claude-sonnet-4-6", WorktreePath: worktreePath,
		BlueprintRunID: brID, BlueprintStepIndex: &stepIdx,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := database.Exec(`UPDATE runs SET status = 'running', session_id = ?, worktree_path = ? WHERE id = ?`, sessionID, worktreePath, runID); err != nil {
		t.Fatalf("update run: %v", err)
	}
}
