package postgres_test

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestRunPendingInputStore_Postgres runs the shared conformance suite
// against the Postgres ConversationPendingInputStore impl, wired against AdminDB
// (production wiring is admin-pool only — see the store's doc comment).
// Skips cleanly when Docker isn't available (pgtest.Shared).
func TestRunPendingInputStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunRunPendingInputStoreConformance(t, func(t *testing.T) (db.ConversationPendingInputStore, string, string, dbtest.RunPendingInputSeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
		seed := dbtest.RunPendingInputSeeder{
			Run: func(t *testing.T, suffix string) string {
				t.Helper()
				return seedPgArtifactRun(t, h, orgID, teamID, userID)
			},
			DeleteRun: func(t *testing.T, conversationID string) {
				t.Helper()
				if _, err := h.AdminDB.Exec(`DELETE FROM conversations WHERE id = $1`, conversationID); err != nil {
					t.Fatalf("delete run: %v", err)
				}
			},
			// Through the production writer, not a test-local INSERT: the
			// store under test reads rows it does not write, so staging them
			// any other way would let the two drift apart unnoticed.
			StagePending: func(t *testing.T, conversationID, userID, message string) {
				t.Helper()
				pending := false
				if _, err := stores.Conversations.InsertMessageSystem(context.Background(), orgID, &domain.Message{
					ConversationID: conversationID,
					UserID:         userID,
					Role:           "user",
					Content:        message,
					Delivered:      &pending,
				}); err != nil {
					t.Fatalf("stage pending input: %v", err)
				}
			},
			SecondUser: func(t *testing.T) string {
				t.Helper()
				return pgtest.SeedUser(t, h, "teammate")
			},
		}
		return stores.ConversationPendingInput, orgID, userID, seed
	})
}
