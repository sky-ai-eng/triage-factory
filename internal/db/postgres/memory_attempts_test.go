package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestMemoryAttemptStore_Postgres runs the shared conformance suite against the
// Postgres MemoryAttemptStore impl. Wired against AdminDB, which is the pool
// production wires it to: every method is a `...System` variant because the
// writer is the brain's memory provisioner and the table's only app-pool
// caller is a read.
func TestMemoryAttemptStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	dbtest.RunMemoryAttemptStoreConformance(t, func(t *testing.T) (db.MemoryAttemptStore, string, dbtest.MemoryAttemptSeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID := seedPgTaskMemoryOrg(t, h)
		promptID := seedPgTaskMemoryPrompt(t, h, orgID, userID)
		seed := dbtest.MemoryAttemptSeeder{
			Conversation: func(t *testing.T, suffix string) string {
				t.Helper()
				conversationID, _ := seedPgConversationForTaskMemory(t, h, orgID, userID, promptID, suffix)
				return conversationID
			},
			SystemLLMRun: func(t *testing.T, suffix string) string {
				t.Helper()
				return seedPgSystemLLMRun(t, h, orgID, suffix)
			},
			DanglingSystemLLMRunID: func(t *testing.T) string {
				t.Helper()
				// A uuid the column accepts and no row carries: the id column
				// is uuid-typed, so a non-uuid string would fail the cast
				// instead of missing the row.
				return uuid.New().String()
			},
		}
		return stores.MemoryAttempts, orgID, seed
	})
}

// seedPgSystemLLMRun stages one system_llm_runs row through the store that owns
// it and returns its id — a valid target for an attempt's system_llm_run_id.
func seedPgSystemLLMRun(t *testing.T, h *pgtest.Harness, orgID, suffix string) string {
	t.Helper()
	id := uuid.New().String()
	store := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey).SystemLLMRuns
	if err := store.Record(context.Background(), domain.SystemLLMRun{
		ID:        id,
		OrgID:     orgID,
		Job:       "memory-" + suffix,
		Model:     "claude-haiku-4-5-20251001",
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed system_llm_runs: %v", err)
	}
	return id
}
