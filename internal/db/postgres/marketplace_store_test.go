package postgres_test

import (
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestMarketplaceStore_Postgres_ReturnedRowConformance runs the shared
// returned-row suite against the Postgres impl under the publisher's claims,
// on the app pool — the point being the same as
// TestJiraAppsStore_Postgres_ReturnedRowConformance's: RLS has to actually
// admit the RETURNING read-back, which an admin-pool (BYPASSRLS) run can't
// prove. The whole suite runs inside ONE claims-carrying transaction, which
// is also what the sequence needs — every subtest after the seed Publish
// shares the one listing row its predecessors left behind.
func TestMarketplaceStore_Postgres_ReturnedRowConformance(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "conform")

	if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		store := pgstore.NewForTx(tx, pgtest.SecretKey).Marketplace
		dbtest.RunMarketplaceReturnedRowConformance(t, func(t *testing.T) (db.MarketplaceStore, string, string, string) {
			t.Helper()
			return store, orgID, teamID, userID
		})
		return nil
	}); err != nil {
		t.Fatalf("WithUser: %v", err)
	}
}
