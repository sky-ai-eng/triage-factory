package postgres_test

import (
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestOrgEventSourceStore_Postgres_Conformance runs the shared contract against
// the Postgres impl. Both pools bind the admin (BYPASSRLS) connection, the
// convention for a store suite: the contract under test is the SQL's, and
// wiring it through the auth path would make every case depend on seeded
// claims. The RLS split is covered separately below.
func TestOrgEventSourceStore_Postgres_Conformance(t *testing.T) {
	dbtest.RunOrgEventSourceStoreConformance(t, func(t *testing.T) (db.OrgEventSourceStore, string) {
		h := pgtest.Shared(t)
		h.Reset(t)
		orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "sources")
		return pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey).OrgEventSources, orgID
	})
}

// TestOrgEventSourceStore_Postgres_OrgScoped pins the isolation the SQLite
// suite structurally cannot: one org's pause is invisible to another. The
// SQLite impl asserts a single sentinel org, so cross-tenant scoping is a
// Postgres-only question — and it is the one that matters, since the router
// reads this per org on every event.
func TestOrgEventSourceStore_Postgres_OrgScoped(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, _, _ := pgtest.SeedOrgWithUser(t, h, "alice")
	orgB, _, _ := pgtest.SeedOrgWithUser(t, h, "bob")
	store := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey).OrgEventSources

	if _, err := store.SetDisabled(t.Context(), orgA, "jira", true, ""); err != nil {
		t.Fatalf("SetDisabled orgA: %v", err)
	}
	disabled, err := store.ListDisabled(t.Context(), orgB)
	if err != nil {
		t.Fatalf("ListDisabled orgB: %v", err)
	}
	if len(disabled) != 0 {
		t.Errorf("orgB sees %v disabled, want none — orgA's pause is not orgB's", disabled)
	}
	row, err := store.Get(t.Context(), orgB, "jira")
	if err != nil {
		t.Fatalf("Get orgB: %v", err)
	}
	if row != nil {
		t.Errorf("Get orgB jira = %+v, want nil", row)
	}
}

// TestOrgEventSourceStore_Postgres_ReturnedRow_AppPool runs SetDisabled's
// returned-row contract through the APP pool under real claims, which the
// conformance suite above cannot: it wires both pools to the admin (BYPASSRLS)
// connection, and on a BYPASSRLS connection a RETURNING clause hands back its
// row unconditionally.
//
// Under RLS it does not. The write's RETURNING has to satisfy the SELECT policy
// for the row it returns, so a policy admitting the write but not the read-back
// yields zero rows from a statement that updated one — which this store would
// surface as ErrNoSuchOrgEventSource on a write that in fact succeeded, leaving
// the source turned off and the admin told it failed.
//
// Both arms are exercised because they are different statements: the INSERT and
// the ON CONFLICT ... DO UPDATE that a second flip takes.
func TestOrgEventSourceStore_Postgres_ReturnedRow_AppPool(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "sources-rls")

	if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		store := pgstore.NewForTx(tx, pgtest.SecretKey).OrgEventSources

		off, err := store.SetDisabled(t.Context(), orgID, "jira", true, userID)
		if err != nil {
			t.Fatalf("SetDisabled(true) on the app pool: %v", err)
		}
		dbtest.AssertWriteReturnedStoredRow(t, "SetDisabled insert arm", off, func() (*domain.OrgEventSource, error) {
			return store.Get(t.Context(), orgID, "jira")
		})

		on, err := store.SetDisabled(t.Context(), orgID, "jira", false, userID)
		if err != nil {
			t.Fatalf("SetDisabled(false) on the app pool: %v", err)
		}
		dbtest.AssertWriteReturnedStoredRow(t, "SetDisabled conflict arm", on, func() (*domain.OrgEventSource, error) {
			return store.Get(t.Context(), orgID, "jira")
		})
		return nil
	}); err != nil {
		t.Fatalf("WithUser: %v", err)
	}
}
