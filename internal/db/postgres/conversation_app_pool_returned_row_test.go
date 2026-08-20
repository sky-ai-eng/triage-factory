package postgres_test

import (
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestConversationStore_Postgres_ReturnedRow_AppPool runs the returned-row
// suite's RLS-focused arm against the Postgres impl under the
// registrant's claims, on the app pool — the same wiring shape
// TestJiraAppsStore_Postgres_ReturnedRowConformance uses and for the same
// reason: wiring both pools to AdminDB (what the plain conformance run does)
// is BYPASSRLS, and a RETURNING clause on a BYPASSRLS connection returns its
// row unconditionally. Under RLS it does not — the write's RETURNING has to
// satisfy the SELECT policy for the row it hands back, so a policy that
// admits the write but not the read-back yields zero rows from a statement
// that updated one. See dbtest.ConversationAppPoolFactory for why this suite
// covers only SetSession/SetWorktreePath, not their sibling Complete (which
// has a plain door too, but also touches the admin-only claims table before
// its own flip — RunConversationReturnedRowConformance, on the admin pool,
// covers Complete/CompleteSystem/CompleteForClaimSystem instead). Every
// claims-row write always routes through the admin pool, so RLS has nothing
// to narrow there either.
func TestConversationStore_Postgres_ReturnedRow_AppPool(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, agentID := seedPgConversationOrg(t, h)
	promptID := seedPgConversationPrompt(t, h, orgID, userID)
	seeder := newPgConversationSeeder(h.AdminDB, orgID, userID, agentID, promptID)

	if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		store := pgstore.NewForTx(tx, pgtest.SecretKey).Conversations
		dbtest.RunConversationAppPoolReturnedRowConformance(t, func(t *testing.T) (db.ConversationStore, string, string, dbtest.ConversationSeeder) {
			t.Helper()
			return store, orgID, userID, seeder
		})
		return nil
	}); err != nil {
		t.Fatalf("WithUser: %v", err)
	}
}
