package sqlite_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestRunWorktreeStore_SQLite runs the shared conformance suite
// against the SQLite ConversationWorktreeStore impl. Each subtest opens a
// fresh in-memory DB so conversation_worktrees state doesn't leak between
// assertions.
func TestRunWorktreeStore_SQLite(t *testing.T) {
	dbtest.RunConversationWorktreeStoreConformance(t, func(t *testing.T) (db.ConversationWorktreeStore, string, dbtest.ConversationWorktreeSeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		seed := dbtest.ConversationWorktreeSeeder{
			Run: func(t *testing.T, suffix string) string {
				t.Helper()
				return seedSQLiteConversationForWorktree(t, conn, suffix)
			},
			DeleteConversation: func(t *testing.T, conversationID string) {
				t.Helper()
				if _, err := conn.Exec(`DELETE FROM conversations WHERE id = ?`, conversationID); err != nil {
					t.Fatalf("delete run: %v", err)
				}
			},
			Repo: func(t *testing.T, slug string) {
				t.Helper()
				if _, err := stores.Repos.GetOrCreateSystem(context.Background(),
					runmode.LocalDefaultOrgID, domain.RepoRefFromSlug(slug)); err != nil {
					t.Fatalf("seed repository %s: %v", slug, err)
				}
			},
		}
		return stores.ConversationWorktrees, runmode.LocalDefaultOrgID, seed
	})
}

// TestRunWorktreeStore_SQLite_RejectsNonLocalOrg pins assertLocalOrg.
func TestRunWorktreeStore_SQLite_RejectsNonLocalOrg(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	const badOrg = "11111111-1111-1111-1111-111111111111"

	if _, _, err := stores.ConversationWorktrees.Insert(ctx, badOrg, domain.ConversationWorktree{ConversationID: "r", RepoID: "owner/repo", Path: "/p", Ref: "@default"}); err == nil {
		t.Error("Insert(non-local org) should error")
	}
	if _, err := stores.ConversationWorktrees.GetByRepoRef(ctx, badOrg, "r", "owner/repo", "@default"); err == nil {
		t.Error("GetByRepoRef(non-local org) should error")
	}
	if _, err := stores.ConversationWorktrees.List(ctx, badOrg, "r"); err == nil {
		t.Error("List(non-local org) should error")
	}
	if _, err := stores.ConversationWorktrees.ListSystem(ctx, badOrg, "r"); err == nil {
		t.Error("ListSystem(non-local org) should error")
	}
	if err := stores.ConversationWorktrees.DeleteByRepoRef(ctx, badOrg, "r", "owner/repo", "@default"); err == nil {
		t.Error("DeleteByRepoRef(non-local org) should error")
	}
	if err := stores.ConversationWorktrees.DeleteByPathSystem(ctx, badOrg, "r", "/p"); err == nil {
		t.Error("DeleteByPathSystem(non-local org) should error")
	}
}

// seedSQLiteConversationForWorktree seeds the entity + event + prompt + task
// + run FK chain conversation_worktrees needs. Mirrors the seedSQLiteRunFor
// TaskMemory shape so both stores' tests stay reading like siblings.
func seedSQLiteConversationForWorktree(t *testing.T, conn *sql.DB, suffix string) string {
	t.Helper()
	now := time.Now().UTC()
	entityID := uuid.New().String()
	sourceID := fmt.Sprintf("run-worktree-%s-%d", suffix, now.UnixNano())
	if _, err := conn.Exec(`
		INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, created_at, state)
		VALUES (?, 'jira', ?, 'issue', 'ConversationWorktree Conformance', 'https://example/x', '{}', ?, 'active')
	`, entityID, sourceID, now); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	eventID := uuid.New().String()
	const eventType = "jira:issue:assigned"
	if _, err := conn.Exec(`
		INSERT INTO events (id, entity_id, event_type, dedup_key)
		VALUES (?, ?, ?, '')
	`, eventID, entityID, eventType); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT OR IGNORE INTO prompts (id, name, body, creator_user_id, team_id)
		VALUES ('p_run_worktree', 'ConversationWorktree', 'body', ?, ?)
	`, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	taskID := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status)
		VALUES (?, ?, ?, ?, 'queued')
	`, taskID, entityID, eventType, eventID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	conversationID := uuid.New().String()
	blueprintRunID := seedBlueprintRunForConversation(t, conn, taskID)
	if _, err := conn.Exec(`
		INSERT INTO conversations (id, task_id, prompt_id, status, model, blueprint_run_id) VALUES (?, ?, 'p_run_worktree', 'running', 'm', ?)
	`, conversationID, taskID, blueprintRunID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return conversationID
}
