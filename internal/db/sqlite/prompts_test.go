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

// TestPromptStore_SQLite runs the shared PromptStore conformance suite
// against the SQLite impl. Each subtest opens a fresh in-memory DB so
// state doesn't leak between assertions.
func TestPromptStore_SQLite(t *testing.T) {
	dbtest.RunPromptStoreConformance(t, func(t *testing.T) (db.PromptStore, string, string, dbtest.RunSeederForStats) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		seeder := func(t *testing.T, promptID string, statusByOffset []string) []string {
			t.Helper()
			return seedSQLiteRunsForStats(t, conn, promptID, statusByOffset)
		}
		return stores.Prompts, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, seeder
	})
}

// openSQLiteForTest returns a fresh in-memory SQLite handle with the
// bootstrap schema applied. Each test gets its own DB so subtests
// don't pollute each other.
func openSQLiteForTest(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return conn
}

// seedSQLiteRunsForStats inserts entity+task+run rows for each entry
// in statusByOffset so PromptStore.Stats has data to aggregate.
// started_at is staggered by `i` days back so the per-day grouping
// has variation.
//
// RunStore hasn't migrated yet (wave 3b), so the seeder owns raw SQL
// — the conformance harness is intentionally schema-blind.
func seedSQLiteRunsForStats(t *testing.T, conn *sql.DB, promptID string, statusByOffset []string) []string {
	t.Helper()
	now := time.Now().UTC()

	// The prompt is assumed to already exist (the harness seeds via
	// Create before calling). Runs needs a task + entity to
	// satisfy FKs.
	entityID := uuid.New().String()
	taskID := uuid.New().String()
	eventID := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES (?, 'github', ?, 'pr', 'Conformance Entity', 'https://example/x', '{}', ?)
	`, entityID, fmt.Sprintf("conformance-runs-%d", now.UnixNano()), now); err != nil {
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

	blueprintRunID := seedBlueprintRunForRun(t, conn, taskID)
	ids := make([]string, 0, len(statusByOffset))
	for i, status := range statusByOffset {
		runID := uuid.New().String()
		startedAt := now.AddDate(0, 0, -i)
		if _, err := conn.Exec(`
			INSERT INTO conversations (id, task_id, prompt_id, status, started_at, blueprint_run_id)
			VALUES (?, ?, ?, ?, ?, ?)
		`, runID, taskID, promptID, status, startedAt, blueprintRunID); err != nil {
			t.Fatalf("seed run %d: %v", i, err)
		}
		// The accounting the stats read derives from: one cost-stamped
		// ledger row + one released claim carrying the duration telemetry.
		if _, err := conn.Exec(`
			INSERT INTO messages (conversation_id, role, subtype, content, cost_usd, created_at)
			VALUES (?, 'assistant', '', 'work', 0.01, ?)
		`, runID, startedAt); err != nil {
			t.Fatalf("seed run message %d: %v", i, err)
		}
		if _, err := conn.Exec(`
			INSERT INTO claims (id, conversation_id, executor_id, boot_epoch, claimed_at, released_at, outcome, duration_ms)
			VALUES (?, ?, 'exec-p', 1, ?, ?, 'completed', 100)
		`, uuid.New().String(), runID, startedAt, startedAt); err != nil {
			t.Fatalf("seed run claim %d: %v", i, err)
		}
		ids = append(ids, runID)
	}
	return ids
}
