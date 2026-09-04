package delegate

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
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
	database, err := sql.Open("sqlite", db.TestDSNMemory)
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

// seedConversationBlueprint mints a blueprint + blueprint_run for taskID and returns
// the blueprint_run id, so run fixtures can satisfy
// conversations.blueprint_run_id NOT NULL. The suffix keeps ids unique +
// deterministic per fixture; the ids are distinct from
// makeConversationBlueprintStep's ("bp-"/"bpr-") so a test can re-point a run onto a
// specific blueprint_run without colliding with the one seedConversation attached.
func seedConversationBlueprint(t *testing.T, database *sql.DB, suffix, taskID string) string {
	t.Helper()
	bpID := "seedbp-" + suffix
	if _, err := sqlitestore.New(database).Blueprints.Create(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Blueprint{
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

// seedConversation inserts a run (plus its entity/event/task/blueprint fixtures)
// with the requested id, session id, and worktree path.
// We bypass the spawner's Delegate flow because these tests don't need
// a real goroutine — only a row in the conversations table. Every run is a
// blueprint step now (conversations.blueprint_run_id NOT NULL), so it mints a 1-step
// blueprint_run and links the run to it.
func seedConversation(t *testing.T, database *sql.DB, conversationID, sessionID, worktreePath string) {
	t.Helper()
	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#"+conversationID, "pr", "T", "https://example.com/"+conversationID)
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	eventID, err := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entity.ID,
		MetadataJSON: `{"check_name":"build"}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := testTaskStore(database).FindOrCreate(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, entity.ID, domain.EventGitHubPRCICheckFailed, conversationID, eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	ensureTestPrompt(t, database, domain.Prompt{ID: "test-prompt", Name: "T", Body: "x", Source: "user"})
	brID := seedConversationBlueprint(t, database, conversationID, task.ID)
	stepIdx := 0
	dbtest.SeedConversation(t, database, domain.Conversation{
		ID:                 conversationID,
		TaskID:             task.ID,
		PromptID:           "test-prompt",
		Status:             "running",
		Model:              "claude-sonnet-4-6",
		SessionID:          sessionID,
		WorktreePath:       worktreePath,
		BlueprintRunID:     brID,
		BlueprintStepIndex: &stepIdx,
	})
}

// settleConversationBlueprint puts a seedConversation fixture's blueprint into a terminal state —
// what the reactor writes once it has read a step's terminal, and the missing
// half of any fixture that stages a `completed` conversation by hand. Without
// it the fixture has staged the hand-off window rather than concluded work, and
// reads as unwakeable for exactly the right reason.
func settleConversationBlueprint(t *testing.T, database *sql.DB, conversationID, status string) {
	t.Helper()
	if _, err := database.Exec(`UPDATE blueprint_runs SET status = ? WHERE id = ?`, status, "seedbpr-"+conversationID); err != nil {
		t.Fatalf("settle blueprint for %s: %v", conversationID, err)
	}
}

// seedJiraConversation is the Jira variant of seedConversation: the task's entity is
// jira-sourced so source-gated paths see a Jira run rather than a
// GitHub PR run.
func seedJiraConversation(t *testing.T, database *sql.DB, conversationID, sessionID, worktreePath string) {
	t.Helper()
	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "jira", "SKY-"+conversationID, "issue", "T-"+conversationID, "https://x/"+conversationID)
	if err != nil {
		t.Fatalf("create jira entity: %v", err)
	}
	eventID, err := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType:    domain.EventJiraIssueAssigned,
		EntityID:     &entity.ID,
		MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := testTaskStore(database).FindOrCreate(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, entity.ID, domain.EventJiraIssueAssigned, conversationID, eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	ensureTestPrompt(t, database, domain.Prompt{ID: "test-prompt", Name: "T", Body: "x", Source: "user"})
	brID := seedConversationBlueprint(t, database, conversationID, task.ID)
	stepIdx := 0
	dbtest.SeedConversation(t, database, domain.Conversation{
		ID: conversationID, TaskID: task.ID, PromptID: "test-prompt",
		Status: "running", Model: "claude-sonnet-4-6", SessionID: sessionID,
		WorktreePath:   worktreePath,
		BlueprintRunID: brID, BlueprintStepIndex: &stepIdx,
	})
}

// storedStatus reads a conversation's STORED status, with SQL NULL — the
// mid-flight state, which is what "queued" and "running" both are now — as
// the empty string.
func storedStatus(t *testing.T, database *sql.DB, convID string) string {
	t.Helper()
	var status sql.NullString
	if err := database.QueryRow(`SELECT status FROM conversations WHERE id = ?`, convID).Scan(&status); err != nil {
		t.Fatalf("read stored status for %s: %v", convID, err)
	}
	return status.String
}

// markEngaged puts a seeded conversation into the state a claimed one is
// really in: no stored outcome, one unreleased claim. "Running" is an
// engagement now, not a column value. Returns the claim id, which is what an
// engagement's fenced writes have to name — the store refuses a claim it
// cannot find, on this dialect as on Postgres.
func markEngaged(t *testing.T, database *sql.DB, convID string) string {
	t.Helper()
	if _, err := database.Exec(`UPDATE conversations SET status = NULL WHERE id = ?`, convID); err != nil {
		t.Fatalf("clear stored status for %s: %v", convID, err)
	}
	claimID := uuid.New().String()
	if _, err := database.Exec(`
		INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch, claimed_at)
		VALUES (?, ?, ?, 'test-engagement', 1, CURRENT_TIMESTAMP)
	`, claimID, runmode.LocalDefaultOrgID, convID); err != nil {
		t.Fatalf("mint claim for %s: %v", convID, err)
	}
	return claimID
}

// hasActiveClaim reports whether the conversation currently holds an
// unreleased claim — the derived "an engagement is driving this".
func hasActiveClaim(t *testing.T, database *sql.DB, convID string) bool {
	t.Helper()
	var live bool
	if err := database.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM claims WHERE conversation_id = ? AND released_at IS NULL)`, convID,
	).Scan(&live); err != nil {
		t.Fatalf("read active claim for %s: %v", convID, err)
	}
	return live
}
