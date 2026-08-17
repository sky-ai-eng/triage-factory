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
	dbtest.RunScoreStoreConformance(t, func(t *testing.T) (db.ScoreStore, string, dbtest.ScoreSeeder) {
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
		seeder := func(t *testing.T, n int) []string {
			t.Helper()
			return seedSQLiteTasks(t, conn, n)
		}
		return stores.Scores, runmode.LocalDefaultOrgID, seeder
	})
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
