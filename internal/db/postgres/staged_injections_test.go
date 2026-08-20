package postgres_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestStagedInjectionStore_Postgres runs the shared conformance suite against the
// Postgres StagedInjectionStore impl, wired against AdminDB (the production wiring is
// admin-pool only: both producer and consumer are claims-less). Skips cleanly
// when Docker isn't available (pgtest.Shared).
func TestStagedInjectionStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	dbtest.RunStagedInjectionStoreConformance(t, func(t *testing.T) (db.StagedInjectionStore, string, dbtest.StagedInjectionSeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
		seed := dbtest.StagedInjectionSeeder{
			Conversation: func(t *testing.T, suffix string) string {
				t.Helper()
				return seedPgArtifactConversation(t, h, orgID, teamID, userID)
			},
			DeleteConversation: func(t *testing.T, conversationID string) {
				t.Helper()
				if _, err := h.AdminDB.Exec(`DELETE FROM conversations WHERE id = $1`, conversationID); err != nil {
					t.Fatalf("delete run: %v", err)
				}
			},
		}
		return stores.StagedInjections, orgID, seed
	})
}

// TestStagedInjectionStore_Postgres_OrgScoped pins org_id defense-in-depth: a flush
// scoped to a different org never drains another org's staged injections, even on the
// BYPASSRLS admin pool.
func TestStagedInjectionStore_Postgres_OrgScoped(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	orgA, userA, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	otherOrg, _, _ := pgtest.SeedOrgWithUser(t, h, "bob")
	convA := seedPgArtifactConversation(t, h, orgA, teamA, userA)

	if _, err := stores.StagedInjections.AppendSystem(ctx, orgA, domain.StagedInjection{
		ConversationID: convA, Producer: domain.StagedInjectionProducerPRNewCommits, Body: "for org A",
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, err := stores.StagedInjections.FlushPendingSystem(ctx, otherOrg, convA)
	if err != nil {
		t.Fatalf("flush other org: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("flush under a different org drained %d injections, want 0", len(got))
	}
}

// TestStagedInjectionStore_Postgres_ReturnedRow_AppPool runs the
// returned-row arm of the staged-injection conformance suite against the
// Postgres impl under a registrant's claims, on the app pool — the same
// wiring shape TestJiraAppsStore_Postgres_ReturnedRowConformance uses and for
// the same reason: wiring both pools to AdminDB (what the plain conformance
// run above does) is BYPASSRLS, and a RETURNING clause on a BYPASSRLS
// connection returns its row unconditionally. Under RLS an INSERT's
// RETURNING instead has to satisfy the INSERT policy's WITH CHECK — this
// wiring is what actually exercises that for messages_all, even though
// StagedInjections is admin-pool only in production (see the interface doc):
// the standard's return-what-you-persisted contract holds independent of
// which pool ends up calling it.
func TestStagedInjectionStore_Postgres_ReturnedRow_AppPool(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	conversationID := seedPgArtifactConversation(t, h, orgID, teamID, userID)

	if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		store := pgstore.NewForTx(tx, pgtest.SecretKey).StagedInjections
		dbtest.RunStagedInjectionReturnedRowConformance(t, func(t *testing.T) (db.StagedInjectionStore, string, string) {
			t.Helper()
			return store, orgID, conversationID
		})
		return nil
	}); err != nil {
		t.Fatalf("WithUser: %v", err)
	}
}
