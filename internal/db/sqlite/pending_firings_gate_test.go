package sqlite_test

import (
	"testing"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestPendingFiringsGate_SQLite pins that the firing kind's claim filter and
// the conversation store's live-conversation read keep answering the same
// question on this dialect.
func TestPendingFiringsGate_SQLite(t *testing.T) {
	dbtest.RunPendingFiringsGateAgreement(t, func(t *testing.T) (db.PendingFiringsStore, db.ConversationStore, string, dbtest.PendingFiringsGateSeeder) {
		t.Helper()
		conn := newSQLiteForPendingFiringsTest(t)
		stores := sqlitestore.New(conn)
		seed := dbtest.PendingFiringsGateSeeder{
			PendingFiringsSeeder: newSQLitePendingFiringsSeeder(conn, stores),
			SubConversation: func(t *testing.T, taskID, promptID, parentID string) string {
				t.Helper()
				id := uuid.New().String()
				dbtest.SeedConversation(t, conn, domain.Conversation{ID: id, TaskID: taskID, PromptID: promptID, Model: "m", TriggerType: "event"})
				execOne(t, conn, "set parent", `UPDATE conversations SET parent_conversation_id = ? WHERE id = ?`, parentID, id)
				return id
			},
			SetStatus: func(t *testing.T, conversationID, status string) {
				t.Helper()
				var v any
				if status != "" {
					v = status
				}
				execOne(t, conn, "set status", `UPDATE conversations SET status = ? WHERE id = ?`, v, conversationID)
			},
		}
		return stores.PendingFirings, stores.Conversations, runmode.LocalDefaultOrgID, seed
	})
}
