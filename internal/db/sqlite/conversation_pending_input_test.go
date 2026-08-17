package sqlite_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestRunPendingInputStore_SQLite runs the shared conformance suite against
// the SQLite ConversationPendingInputStore impl. Each subtest opens a fresh
// in-memory DB so state doesn't leak between assertions.
func TestRunPendingInputStore_SQLite(t *testing.T) {
	dbtest.RunConversationPendingInputStoreConformance(t, func(t *testing.T) (db.ConversationPendingInputStore, string, string, dbtest.ConversationPendingInputSeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		seed := dbtest.ConversationPendingInputSeeder{
			Run: func(t *testing.T, suffix string) string {
				t.Helper()
				return seedSQLiteConversationForPendingInput(t, conn, suffix)
			},
			DeleteConversation: func(t *testing.T, conversationID string) {
				t.Helper()
				if _, err := conn.Exec(`DELETE FROM conversations WHERE id = ?`, conversationID); err != nil {
					t.Fatalf("delete run: %v", err)
				}
			},
			StagePending: func(t *testing.T, conversationID, userID, message string) {
				t.Helper()
				stagePendingViaTranscript(t, stores, runmode.LocalDefaultOrgID, conversationID, userID, message)
			},
			SecondUser: func(t *testing.T) string {
				t.Helper()
				id := uuid.New().String()
				if _, err := conn.Exec(`INSERT INTO users (id, display_name) VALUES (?, 'teammate')`, id); err != nil {
					t.Fatalf("seed second user: %v", err)
				}
				return id
			},
		}
		return stores.ConversationPendingInput, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, seed
	})
}

// TestRunPendingInputStore_SQLite_RejectsNonLocalOrg pins assertLocalOrg.
func TestRunPendingInputStore_SQLite_RejectsNonLocalOrg(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	const badOrg = "11111111-1111-1111-1111-111111111111"

	if _, _, _, err := stores.ConversationPendingInput.Peek(ctx, badOrg, "r"); err == nil {
		t.Error("Peek(non-local org) should error")
	}
	if _, _, _, err := stores.ConversationPendingInput.Consume(ctx, badOrg, "r"); err == nil {
		t.Error("Consume(non-local org) should error")
	}
}

// seedSQLiteConversationForPendingInput inserts a bare run row (origin='interactive',
// so no blueprint_run FK chain is required) the run_pending_input FK needs.
func seedSQLiteConversationForPendingInput(t *testing.T, conn *sql.DB, suffix string) string {
	t.Helper()
	id := uuid.New().String()
	if _, err := conn.Exec(
		`INSERT INTO conversations (id, origin, status) VALUES (?, 'interactive', 'running')`, id,
	); err != nil {
		t.Fatalf("seed run %s (%s): %v", id, fmt.Sprintf("pending-input-%s", suffix), err)
	}
	return id
}

// stagePendingViaTranscript queues a follow-up the way Spawner.queueFollowUp
// does — an undelivered plain user row through the ordinary transcript insert.
// Going through the production writer is the point: the store under test reads
// rows it does not write, so a test-local INSERT would let the two drift.
func stagePendingViaTranscript(t *testing.T, stores db.Stores, orgID, conversationID, userID, message string) {
	t.Helper()
	pending := false
	if _, err := stores.Conversations.InsertMessage(context.Background(), orgID, &domain.Message{
		ConversationID: conversationID,
		UserID:         userID,
		Role:           "user",
		Content:        message,
		Delivered:      &pending,
	}); err != nil {
		t.Fatalf("stage pending input: %v", err)
	}
}
