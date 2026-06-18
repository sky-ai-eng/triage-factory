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
// against the SQLite RunWorktreeStore impl. Each subtest opens a
// fresh in-memory DB so run_worktrees state doesn't leak between
// assertions.
func TestRunWorktreeStore_SQLite(t *testing.T) {
	dbtest.RunRunWorktreeStoreConformance(t, func(t *testing.T) (db.RunWorktreeStore, string, dbtest.RunWorktreeSeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		seed := dbtest.RunWorktreeSeeder{
			Run: func(t *testing.T, suffix string) string {
				t.Helper()
				return seedSQLiteRunForWorktree(t, conn, suffix)
			},
			DeleteRun: func(t *testing.T, runID string) {
				t.Helper()
				if _, err := conn.Exec(`DELETE FROM runs WHERE id = ?`, runID); err != nil {
					t.Fatalf("delete run: %v", err)
				}
			},
		}
		return stores.RunWorktrees, runmode.LocalDefaultOrgID, seed
	})
}

// TestRunWorktreeStore_SQLite_RejectsNonLocalOrg pins assertLocalOrg.
func TestRunWorktreeStore_SQLite_RejectsNonLocalOrg(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	const badOrg = "11111111-1111-1111-1111-111111111111"

	if _, _, err := stores.RunWorktrees.Insert(ctx, badOrg, domain.RunWorktree{RunID: "r", RepoID: "owner/repo", Path: "/p"}); err == nil {
		t.Error("Insert(non-local org) should error")
	}
	if _, err := stores.RunWorktrees.GetByRepo(ctx, badOrg, "r", "owner/repo"); err == nil {
		t.Error("GetByRepo(non-local org) should error")
	}
	if _, err := stores.RunWorktrees.List(ctx, badOrg, "r"); err == nil {
		t.Error("List(non-local org) should error")
	}
	if _, err := stores.RunWorktrees.ListSystem(ctx, badOrg, "r"); err == nil {
		t.Error("ListSystem(non-local org) should error")
	}
	if err := stores.RunWorktrees.DeleteByRepo(ctx, badOrg, "r", "owner/repo"); err == nil {
		t.Error("DeleteByRepo(non-local org) should error")
	}
	if err := stores.RunWorktrees.DeleteByPathSystem(ctx, badOrg, "r", "/p"); err == nil {
		t.Error("DeleteByPathSystem(non-local org) should error")
	}
}

// seedSQLiteRunForWorktree seeds the entity + event + prompt + task
// + run FK chain run_worktrees needs. Mirrors the seedSQLiteRunFor
// TaskMemory shape so both stores' tests stay reading like siblings.
func seedSQLiteRunForWorktree(t *testing.T, conn *sql.DB, suffix string) string {
	t.Helper()
	now := time.Now().UTC()
	entityID := uuid.New().String()
	sourceID := fmt.Sprintf("run-worktree-%s-%d", suffix, now.UnixNano())
	if _, err := conn.Exec(`
		INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, created_at, state)
		VALUES (?, 'jira', ?, 'issue', 'RunWorktree Conformance', 'https://example/x', '{}', ?, 'active')
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
		VALUES ('p_run_worktree', 'RunWorktree', 'body', ?, ?)
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
	runID := uuid.New().String()
	blueprintRunID := seedBlueprintRunForRun(t, conn, taskID)
	if _, err := conn.Exec(`
		INSERT INTO runs (id, task_id, prompt_id, status, model, blueprint_run_id) VALUES (?, ?, 'p_run_worktree', 'running', 'm', ?)
	`, runID, taskID, blueprintRunID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return runID
}
