package postgres_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
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
