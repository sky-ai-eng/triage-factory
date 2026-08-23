package postgres_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestOrgsStore_Postgres_ReturnedRowConformance runs the shared returned-row
// suite against the Postgres impl under the registrant's claims, on the app
// pool.
//
// The claims are the point. Wiring both pools to AdminDB — what
// TestSettingsStores_Postgres does, so behavior stays independent of the auth
// path — makes the connection BYPASSRLS, and a RETURNING clause on a
// BYPASSRLS connection returns its row unconditionally. Under RLS it does
// not: the upsert's conflict arm has to satisfy the SELECT policy for the row
// it hands back, so a policy that admits the write but not the read-back
// yields zero rows from a statement that updated one. That is the failure
// this wiring exists to catch, and it is invisible on the admin pool.
func TestOrgsStore_Postgres_ReturnedRowConformance(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "orgs-returned-row")

	if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		store := pgstore.NewForTx(tx, pgtest.SecretKey).Orgs
		dbtest.RunOrgsReturnedRowConformance(t, func(t *testing.T) (db.OrgsStore, string) {
			t.Helper()
			return store, orgID
		})
		return nil
	}); err != nil {
		t.Fatalf("WithUser: %v", err)
	}
}

// TestOrgsStore_Postgres_ListActiveSystem_ExcludesSoftDeleted pins the
// admin-pool ListActiveSystem behavior:
//   - returns every orgs row with deleted_at IS NULL
//   - filters out soft-deleted rows (deleted_at IS NOT NULL)
//   - returns ids in ascending order so per-org iteration is stable
//     across poll cycles
//
// This is the contract the background-service callers (poller,
// tracker, repoprofile) iterate at the top of each
// cycle.
func TestOrgsStore_Postgres_ListActiveSystem_ExcludesSoftDeleted(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA := seedPgOrgForAgents(t, h)
	orgB := seedPgOrgForAgents(t, h)
	orgDeleted := seedPgOrgForAgents(t, h)
	if _, err := h.AdminDB.Exec(
		`UPDATE orgs SET deleted_at = now() WHERE id = $1`, orgDeleted,
	); err != nil {
		t.Fatalf("soft-delete org: %v", err)
	}

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	got, err := stores.Orgs.ListActiveSystem(context.Background())
	if err != nil {
		t.Fatalf("ListActiveSystem: %v", err)
	}
	gotSet := make(map[string]bool, len(got))
	for _, id := range got {
		gotSet[id] = true
	}
	if !gotSet[orgA] || !gotSet[orgB] {
		t.Errorf("ListActiveSystem missing active orgs (got=%v, want both %s and %s)", got, orgA, orgB)
	}
	if gotSet[orgDeleted] {
		t.Errorf("ListActiveSystem leaked soft-deleted org %s; got=%v", orgDeleted, got)
	}
	for i := 1; i < len(got); i++ {
		if got[i] < got[i-1] {
			t.Errorf("ListActiveSystem ids not ascending at index %d: %s < %s", i, got[i], got[i-1])
			break
		}
	}
}
