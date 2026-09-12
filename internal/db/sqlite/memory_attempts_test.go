package sqlite_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestMemoryAttemptStore_SQLite runs the shared conformance suite against the
// SQLite MemoryAttemptStore impl. Each subtest opens a fresh in-memory DB so
// attempts don't leak between assertions.
func TestMemoryAttemptStore_SQLite(t *testing.T) {
	dbtest.RunMemoryAttemptStoreConformance(t, func(t *testing.T) (db.MemoryAttemptStore, string, dbtest.MemoryAttemptSeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		seed := dbtest.MemoryAttemptSeeder{
			Conversation: func(t *testing.T, suffix string) string {
				t.Helper()
				conversationID, _ := seedSQLiteConversationForTaskMemory(t, conn, suffix)
				return conversationID
			},
			SystemLLMRun: func(t *testing.T, suffix string) string {
				t.Helper()
				return seedSQLiteSystemLLMRun(t, conn, suffix)
			},
			DanglingSystemLLMRunID: func(t *testing.T) string {
				t.Helper()
				// SQLite keys these tables on TEXT, so any string would miss
				// the row; a uuid keeps the fixture the same shape as the ids
				// the app actually mints.
				return uuid.New().String()
			},
		}
		return stores.MemoryAttempts, runmode.LocalDefaultOrgID, seed
	})
}

// seedSQLiteSystemLLMRun stages one system_llm_runs row through the store that
// owns it and returns its id — a valid target for an attempt's
// system_llm_run_id.
func seedSQLiteSystemLLMRun(t *testing.T, conn *sql.DB, suffix string) string {
	t.Helper()
	id := uuid.New().String()
	if err := sqlitestore.New(conn).SystemLLMRuns.Record(context.Background(), domain.SystemLLMRun{
		ID:        id,
		OrgID:     runmode.LocalDefaultOrgID,
		Job:       "memory-" + suffix,
		Model:     "claude-haiku-4-5-20251001",
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed system_llm_runs: %v", err)
	}
	return id
}
