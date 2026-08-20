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

// TestStagedInjectionStore_SQLite runs the shared conformance suite against the
// SQLite StagedInjectionStore impl. Each subtest opens a fresh in-memory DB so queue
// state doesn't leak between assertions.
func TestStagedInjectionStore_SQLite(t *testing.T) {
	dbtest.RunStagedInjectionStoreConformance(t, func(t *testing.T) (db.StagedInjectionStore, string, dbtest.StagedInjectionSeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		seed := dbtest.StagedInjectionSeeder{
			Conversation: func(t *testing.T, suffix string) string {
				t.Helper()
				return seedSQLiteConversationForStagedInjection(t, conn, suffix)
			},
			DeleteConversation: func(t *testing.T, conversationID string) {
				t.Helper()
				if _, err := conn.Exec(`DELETE FROM conversations WHERE id = ?`, conversationID); err != nil {
					t.Fatalf("delete conversation: %v", err)
				}
			},
		}
		return stores.StagedInjections, runmode.LocalDefaultOrgID, seed
	})
}

// TestStagedInjectionStore_SQLite_RejectsNonLocalOrg pins assertLocalOrg.
func TestStagedInjectionStore_SQLite_RejectsNonLocalOrg(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	const badOrg = "11111111-1111-1111-1111-111111111111"

	if _, err := stores.StagedInjections.AppendSystem(ctx, badOrg, domain.StagedInjection{ConversationID: "r", Producer: "p", Body: "b"}); err == nil {
		t.Error("AppendSystem(non-local org) should error")
	}
	if _, err := stores.StagedInjections.FlushPendingSystem(ctx, badOrg, "r"); err == nil {
		t.Error("FlushPendingSystem(non-local org) should error")
	}
}

// TestStagedInjectionStore_SQLite_ReturnedRow runs the returned-row arm of
// the staged-injection conformance suite (TFAC-869) against the SQLite impl.
func TestStagedInjectionStore_SQLite_ReturnedRow(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	conversationID := seedSQLiteConversationForStagedInjection(t, conn, "ret")

	dbtest.RunStagedInjectionReturnedRowConformance(t, func(t *testing.T) (db.StagedInjectionStore, string, string) {
		t.Helper()
		return stores.StagedInjections, runmode.LocalDefaultOrgID, conversationID
	})
}

// seedSQLiteConversationForStagedInjection inserts a bare conversation row
// (origin='interactive', so no blueprint_run FK chain is required) for the
// staged injection's messages.conversation_id FK to land on.
func seedSQLiteConversationForStagedInjection(t *testing.T, conn *sql.DB, suffix string) string {
	t.Helper()
	id := uuid.New().String()
	if _, err := conn.Exec(
		`INSERT INTO conversations (id, origin, status) VALUES (?, 'interactive', 'running')`, id,
	); err != nil {
		t.Fatalf("seed conversation %s (%s): %v", id, fmt.Sprintf("staged-%s", suffix), err)
	}
	return id
}
