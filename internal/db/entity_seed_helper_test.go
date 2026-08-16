package db

import (
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// createEntityForTest inserts an entity row directly via raw SQL so
// package-db CRUD tests (pending_firings, conversation_worktrees, projects, ...)
// have an entity to FK-point to without reaching for the EntityStore
// impl (which lives in internal/db/sqlite and would form a circular
// import if pulled into package db). Mirrors createRunForTest in
// agentrun_seed_helper_test.go — see that helper's doc for the
// rationale.
//
// Returns a populated domain.Entity for caller convenience (most
// tests want the generated id). state defaults to "active"; tests
// that need a closed entity flip it afterwards with their own UPDATE.
func createEntityForTest(t *testing.T, database *sql.DB, source, sourceID, kind, title, url string) *domain.Entity {
	t.Helper()
	id := uuid.New().String()
	now := time.Now()
	_, err := database.Exec(`
		INSERT INTO entities (id, source, source_id, kind, title, url, state, created_at, last_polled_at)
		VALUES (?, ?, ?, ?, ?, ?, 'active', ?, ?)
	`, id, source, sourceID, kind, title, url, now, now)
	if err != nil {
		t.Fatalf("createEntityForTest: %v", err)
	}
	return &domain.Entity{
		ID:           id,
		Source:       source,
		SourceID:     sourceID,
		Kind:         kind,
		Title:        title,
		URL:          url,
		State:        "active",
		CreatedAt:    now,
		LastPolledAt: &now,
	}
}
