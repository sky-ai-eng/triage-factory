package postgres_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestPendingFiringsGate_Postgres pins that the firing kind's claim filter
// and the conversation store's live-conversation read keep answering the
// same question on this dialect. Skips without Docker like every pgtest
// file.
func TestPendingFiringsGate_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	dbtest.RunPendingFiringsGateAgreement(t, func(t *testing.T) (db.PendingFiringsStore, db.ConversationStore, string, dbtest.PendingFiringsGateSeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID, agentID := seedPgPendingFiringsOrg(t, h)
		seed := dbtest.PendingFiringsGateSeeder{
			PendingFiringsSeeder: newPgPendingFiringsSeeder(h, stores, orgID, userID, agentID),
			SubConversation: func(t *testing.T, taskID, promptID, parentID string) string {
				t.Helper()
				id := seedPgLiveConversation(t, h, orgID, userID, taskID, promptID)
				pgExecOne(t, h, "set parent", `UPDATE conversations SET parent_conversation_id = $1 WHERE id = $2`, parentID, id)
				return id
			},
			SetStatus: func(t *testing.T, conversationID, status string) {
				t.Helper()
				var v any
				if status != "" {
					v = status
				}
				pgExecOne(t, h, "set status", `UPDATE conversations SET status = $1 WHERE id = $2`, v, conversationID)
			},
		}
		return stores.PendingFirings, stores.Conversations, orgID, seed
	})
}
