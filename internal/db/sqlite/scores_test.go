package sqlite_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestScoreStore_SQLite runs the shared conformance suite against the
// SQLite ScoreStore impl. The factory opens a fresh in-memory DB per
// test, bootstraps the schema, and supplies a seeder that creates
// queued+pending task rows the harness asserts against. See
// internal/db/dbtest for the assertion bodies.
func TestScoreStore_SQLite(t *testing.T) {
	dbtest.RunScoreStoreConformance(t, func(t *testing.T) dbtest.ScoreFixture {
		t.Helper()
		conn, err := sql.Open("sqlite", db.TestDSNMemory)
		if err != nil {
			t.Fatalf("open in-memory db: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		conn.SetMaxOpenConns(1)
		conn.SetMaxIdleConns(1)

		if err := db.BootstrapSchemaForTest(conn); err != nil {
			t.Fatalf("bootstrap schema: %v", err)
		}

		stores := sqlitestore.New(conn)
		return dbtest.ScoreFixture{
			Store:    stores.Scores,
			OrgID:    runmode.LocalDefaultOrgID,
			Seed:     func(t *testing.T, n int) []string { t.Helper(); return seedSQLiteTasks(t, conn, n) },
			ReDerive: stores.TaskReDerive,
			ScoreRevision: func(t *testing.T, taskID string) int64 {
				t.Helper()
				return readSQLiteScoreRevision(t, conn, taskID)
			},
			QueueRows: func(t *testing.T, taskID string) []dbtest.ReDeriveQueueRow {
				t.Helper()
				return readSQLiteReDeriveRows(t, conn, taskID)
			},
		}
	})
}

// readSQLiteScoreRevision reads tasks.score_revision, the one column no
// domain read projects.
func readSQLiteScoreRevision(t *testing.T, conn *sql.DB, taskID string) int64 {
	t.Helper()
	var rev int64
	if err := conn.QueryRow(`SELECT score_revision FROM tasks WHERE id = ?`, taskID).Scan(&rev); err != nil {
		t.Fatalf("read score_revision of %s: %v", taskID, err)
	}
	return rev
}

// readSQLiteReDeriveRows reads a task's task_rederive_queue rows, oldest
// first, in the shape the conformance suites compare.
func readSQLiteReDeriveRows(t *testing.T, conn *sql.DB, taskID string) []dbtest.ReDeriveQueueRow {
	t.Helper()
	rows, err := conn.Query(`
		SELECT id, status, attempt, requested_revision, COALESCE(unique_key, '')
		FROM task_rederive_queue WHERE task_id = ? ORDER BY id
	`, taskID)
	if err != nil {
		t.Fatalf("read task_rederive_queue rows of %s: %v", taskID, err)
	}
	defer rows.Close()
	out := []dbtest.ReDeriveQueueRow{}
	for rows.Next() {
		var r dbtest.ReDeriveQueueRow
		if err := rows.Scan(&r.ID, &r.Status, &r.Attempt, &r.RequestedRevision, &r.UniqueKey); err != nil {
			t.Fatalf("scan task_rederive_queue row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read task_rederive_queue rows: %v", err)
	}
	return out
}

// seedSQLiteTasks inserts n rows of (entity + task) directly via raw
// SQL. TaskStore hasn't migrated yet (wave 3a), so the seeder owns
// schema knowledge — the conformance harness is intentionally
// schema-blind. When TaskStore lands this collapses into a
// stores.Tasks.FindOrCreate call.
func seedSQLiteTasks(t *testing.T, conn *sql.DB, n int) []string {
	t.Helper()
	now := time.Now().UTC()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		entityID := uuid.New().String()
		taskID := uuid.New().String()
		eventID := uuid.New().String()
		sourceID := fmt.Sprintf("conformance-pr-%d-%d", i, now.UnixNano())
		// events_catalog must include the event_type before the FK
		// fires. The bootstrap seed includes the standard catalog;
		// "github:pr:opened" is a stable entry that matches
		// domain.EventGitHubPROpened.
		eventType := "github:pr:opened"

		if _, err := conn.Exec(`
			INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, created_at)
			VALUES (?, 'github', ?, 'pr', ?, ?, '{}', ?)
		`, entityID, sourceID, fmt.Sprintf("Conformance PR %d", i), "https://example/pr/"+sourceID, now); err != nil {
			t.Fatalf("seed entity: %v", err)
		}
		if _, err := conn.Exec(`
			INSERT INTO events (id, entity_id, event_type, dedup_key, metadata_json, created_at)
			VALUES (?, ?, ?, '', '{}', ?)
		`, eventID, entityID, eventType, now); err != nil {
			t.Fatalf("seed event: %v", err)
		}
		if _, err := conn.Exec(`
			INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id,
			                   status, scoring_status, created_at)
			VALUES (?, ?, ?, '', ?, 'queued', 'pending', ?)
		`, taskID, entityID, eventType, eventID, now); err != nil {
			t.Fatalf("seed task: %v", err)
		}
		ids = append(ids, taskID)
	}
	return ids
}

// TestScoreStore_SQLite_UpdateTaskScores_ChunksLargeBatch exercises
// the chunking path in UpdateTaskScores. The chunk size is 150
// updates × 5 placeholders = 750 placeholders per statement; this
// test passes 175 updates so chunking has to happen (>150 forces
// at least two chunks) and the all-or-nothing tx around them must
// hold so every row ends up scored.
func TestScoreStore_SQLite_UpdateTaskScores_ChunksLargeBatch(t *testing.T) {
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}

	const n = 175
	ids := seedSQLiteTasks(t, conn, n)
	updates := make([]domain.TaskScoreUpdate, len(ids))
	for i, id := range ids {
		updates[i] = domain.TaskScoreUpdate{
			ID:                  id,
			PriorityScore:       float64(i%10) * 0.1,
			AutonomySuitability: float64(i%5) * 0.2,
			Summary:             fmt.Sprintf("summary-%d", i),
			PriorityReasoning:   fmt.Sprintf("reason-%d", i),
		}
	}

	stores := sqlitestore.New(conn)
	if err := stores.Scores.UpdateTaskScores(context.Background(), runmode.LocalDefaultOrgID, updates); err != nil {
		t.Fatalf("UpdateTaskScores: %v", err)
	}

	// Every row should be 'scored' now — if a chunk silently dropped,
	// some rows would still be 'pending' or 'in_progress'.
	var scored int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM tasks WHERE scoring_status = 'scored'`).Scan(&scored); err != nil {
		t.Fatalf("count scored: %v", err)
	}
	if scored != n {
		t.Fatalf("scored count: got %d, want %d", scored, n)
	}
}
