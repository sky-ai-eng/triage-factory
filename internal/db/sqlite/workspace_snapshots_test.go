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
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestWorkspaceSnapshotStore_SQLite runs the shared conformance suite against
// the SQLite WorkspaceSnapshotStore impl. Each subtest opens a fresh in-memory
// DB so one key's lifecycle doesn't leak into the next assertion.
func TestWorkspaceSnapshotStore_SQLite(t *testing.T) {
	dbtest.RunWorkspaceSnapshotStoreConformance(t, func(t *testing.T) (db.WorkspaceSnapshotStore, string, dbtest.WorkspaceSnapshotSeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		seed := dbtest.WorkspaceSnapshotSeeder{
			BlueprintRun: func(t *testing.T, suffix string) string {
				t.Helper()
				return seedBlueprintRunForConversation(t, conn, seedSQLiteTaskForSnapshot(t, conn, suffix))
			},
			DeleteBlueprintRun: func(t *testing.T, blueprintRunID string) {
				t.Helper()
				if _, err := conn.Exec(`DELETE FROM blueprint_runs WHERE id = ?`, blueprintRunID); err != nil {
					t.Fatalf("delete blueprint_run: %v", err)
				}
			},
		}
		return stores.WorkspaceSnapshots, runmode.LocalDefaultOrgID, seed
	})
}

// TestWorkspaceSnapshotStore_SQLite_RejectsNonLocalOrg pins assertLocalOrg.
func TestWorkspaceSnapshotStore_SQLite_RejectsNonLocalOrg(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	const badOrg = "11111111-1111-1111-1111-111111111111"

	if err := stores.WorkspaceSnapshots.BeginSnapshotSystem(ctx, badOrg, "br", "claim"); err == nil {
		t.Error("BeginSnapshotSystem(non-local org) should error")
	}
	if _, err := stores.WorkspaceSnapshots.FinishSnapshotSystem(ctx, badOrg, "br", "claim", true); err == nil {
		t.Error("FinishSnapshotSystem(non-local org) should error")
	}
	if _, err := stores.WorkspaceSnapshots.GetSnapshotStateSystem(ctx, badOrg, "br"); err == nil {
		t.Error("GetSnapshotStateSystem(non-local org) should error")
	}
	if err := stores.WorkspaceSnapshots.DeleteSnapshotStateSystem(ctx, badOrg, "br"); err == nil {
		t.Error("DeleteSnapshotStateSystem(non-local org) should error")
	}
}

// seedSQLiteTaskForSnapshot seeds the entity + event + task chain a
// blueprint_runs row needs (its task_id is NOT NULL and FK'd). Mirrors the
// worktree/task-memory fixtures so these store tests read as siblings.
func seedSQLiteTaskForSnapshot(t *testing.T, conn *sql.DB, suffix string) string {
	t.Helper()
	now := time.Now().UTC()
	entityID := uuid.New().String()
	sourceID := fmt.Sprintf("snapshot-%s-%d", suffix, now.UnixNano())
	if _, err := conn.Exec(`
		INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, created_at, state)
		VALUES (?, 'jira', ?, 'issue', 'WorkspaceSnapshot Conformance', 'https://example/x', '{}', ?, 'active')
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
	taskID := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status)
		VALUES (?, ?, ?, ?, 'queued')
	`, taskID, entityID, eventType, eventID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return taskID
}
