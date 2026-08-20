package sqlite_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestSwipeStore_SQLite runs the shared conformance suite against
// the SQLite SwipeStore impl. Each subtest opens a fresh in-memory
// DB so swipe_events state doesn't leak between assertions.
func TestSwipeStore_SQLite(t *testing.T) {
	dbtest.RunSwipeStoreConformance(t, func(t *testing.T) dbtest.SwipeStoreHarness {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)

		return dbtest.SwipeStoreHarness{
			Store: stores.Swipes,
			OrgID: runmode.LocalDefaultOrgID,
			// swipe_events.creator_user_id has no explicit write in local
			// mode — N=1, so the column default is the single local user,
			// which is exactly the subject a local request carries.
			UserID: runmode.LocalDefaultUserID,
			SeedTask: func(t *testing.T) string {
				t.Helper()
				return seedSQLiteTaskForSwipes(t, conn)
			},
			ReadTask: func(t *testing.T, taskID string) (string, time.Time) {
				t.Helper()
				return readSQLiteTask(t, conn, taskID)
			},
			ReadAudit: func(t *testing.T, taskID string) []string {
				t.Helper()
				return readSQLiteSwipeAudit(t, conn, taskID)
			},
			SeedForeignGesture: func(t *testing.T, taskID, action string) {
				t.Helper()
				// creator_user_id is written explicitly here — the point of
				// the hook is a row the local default did NOT author.
				if _, err := conn.Exec(
					`INSERT INTO swipe_events (task_id, action, hesitation_ms, creator_user_id)
					 VALUES (?, ?, 0, ?)`,
					taskID, action, foreignSwipeUserID,
				); err != nil {
					t.Fatalf("seed foreign %s gesture on %s: %v", action, taskID, err)
				}
			},
		}
	})
}

// foreignSwipeUserID is a user other than the local default, for staging
// "somebody else acted after you" in the conformance suite. Local mode is N=1
// so no such user exists in practice; swipe_events has no FK on the column in
// this dialect, and the row only ever has to be attributable to someone else.
const foreignSwipeUserID = "00000000-0000-0000-0000-0000000000fe"

// readSQLiteSwipeAudit returns swipe_events.action rows for a task,
// oldest first. Used by the harness to pin the audit-log invariants
// (RecordSwipe writes one, RequeueTask writes none, UndoLastSwipe
// appends 'undo'). Schema-coupled to swipe_events; the harness
// itself is schema-blind.
func readSQLiteSwipeAudit(t *testing.T, conn *sql.DB, taskID string) []string {
	t.Helper()
	rows, err := conn.Query(`SELECT action FROM swipe_events WHERE task_id = ? ORDER BY id`, taskID)
	if err != nil {
		t.Fatalf("readSQLiteSwipeAudit %s: %v", taskID, err)
	}
	defer rows.Close()
	var actions []string
	for rows.Next() {
		var action string
		if err := rows.Scan(&action); err != nil {
			t.Fatalf("scan swipe_events action: %v", err)
		}
		actions = append(actions, action)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("readSQLiteSwipeAudit iteration: %v", err)
	}
	return actions
}

// seedSQLiteTaskForSwipes creates an entity + event + task row for
// the swipe conformance suite to swipe against. Returns the task ID.
func seedSQLiteTaskForSwipes(t *testing.T, conn *sql.DB) string {
	t.Helper()
	now := time.Now().UTC()
	entityID := uuid.New().String()
	taskID := uuid.New().String()
	eventID := uuid.New().String()
	sourceID := fmt.Sprintf("swipe-conformance-%d", now.UnixNano())

	if _, err := conn.Exec(`
		INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES (?, 'github', ?, 'pr', 'Swipe Conformance', 'https://example/x', '{}', ?)
	`, entityID, sourceID, now); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO events (id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES (?, ?, 'github:pr:opened', '', '{}', ?)
	`, eventID, entityID, now); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id, status, scoring_status, created_at)
		VALUES (?, ?, 'github:pr:opened', '', ?, 'queued', 'pending', ?)
	`, taskID, entityID, eventID, now); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return taskID
}

// readSQLiteTask returns status + snooze_until for the harness's
// post-swipe assertions. snooze_until parses from SQLite's text
// timestamp; zero time means NULL.
func readSQLiteTask(t *testing.T, conn *sql.DB, taskID string) (string, time.Time) {
	t.Helper()
	var status string
	var snoozeUntil sql.NullTime
	err := conn.QueryRow(`SELECT status, snooze_until FROM tasks WHERE id = ?`, taskID).Scan(&status, &snoozeUntil)
	if err != nil {
		t.Fatalf("readSQLiteTask %s: %v", taskID, err)
	}
	if snoozeUntil.Valid {
		return status, snoozeUntil.Time
	}
	return status, time.Time{}
}

// TestSnoozeVisibility_SQLite runs the snooze-visibility conformance suite —
// the seam between "SwipeStore wrote a wake time" and "TaskStore.List hides
// the row until it arrives" — against the SQLite stores.
func TestSnoozeVisibility_SQLite(t *testing.T) {
	dbtest.RunSnoozeVisibilityConformance(t, func(t *testing.T) (db.SwipeStore, db.TaskStore, string, dbtest.TaskSeederForSwipes) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		seed := func(t *testing.T) string {
			t.Helper()
			return seedSQLiteTaskForSwipes(t, conn)
		}
		return stores.Swipes, stores.Tasks, runmode.LocalDefaultOrgID, seed
	})
}
