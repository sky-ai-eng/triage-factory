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

// TestModelAvailabilityStore_Postgres_Conformance runs the shared lifecycle
// contract against the Postgres impl. Both pools bind the admin (BYPASSRLS)
// connection, the convention for a store suite: the contract under test is the
// SQL's, and wiring it through the auth path would make every case depend on
// seeded claims. The RLS split is covered separately below.
func TestModelAvailabilityStore_Postgres_Conformance(t *testing.T) {
	dbtest.RunModelAvailabilityStoreConformance(t, func(t *testing.T) (db.ModelAvailabilityStore, string) {
		h := pgtest.Shared(t)
		h.Reset(t)
		orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "availability")
		return pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey).ModelAvailability, orgID
	})
}

// TestModelAvailabilityStore_Postgres_OrgScoped pins the isolation the SQLite
// suite structurally cannot: one org's probe results are invisible to another.
// The SQLite impl asserts a single sentinel org, so cross-tenant scoping is a
// Postgres-only question — and it is the one that matters here, because what
// leaks would be another tenant's inference entitlements.
func TestModelAvailabilityStore_Postgres_OrgScoped(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, _, _ := pgtest.SeedOrgWithUser(t, h, "availability-alice")
	orgB, _, _ := pgtest.SeedOrgWithUser(t, h, "availability-bob")
	store := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey).ModelAvailability

	if _, err := store.Record(t.Context(), orgA, "anthropic", "claude-sonnet-5", domain.ModelAvailabilityGreen, ""); err != nil {
		t.Fatalf("Record orgA: %v", err)
	}
	rows, err := store.List(t.Context(), orgB)
	if err != nil {
		t.Fatalf("List orgB: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("orgB sees %v, want none — orgA's entitlements are not orgB's", rows)
	}
	row, err := store.Get(t.Context(), orgB, "anthropic", "claude-sonnet-5")
	if err != nil {
		t.Fatalf("Get orgB: %v", err)
	}
	if row != nil {
		t.Errorf("Get orgB = %+v, want nil", row)
	}
}

// TestModelAvailabilityStore_Postgres_ReturnedRow_AppPool runs Record's
// returned-row contract through the APP pool under real claims, which the
// conformance suite above cannot: it wires both pools to the admin (BYPASSRLS)
// connection, and on a BYPASSRLS connection a RETURNING clause hands back its
// row unconditionally.
//
// Under RLS it does not. The write's RETURNING has to satisfy the SELECT policy
// for the row it returns, so a policy admitting the write but not the read-back
// would yield zero rows from a statement that inserted one — surfacing as an
// error on a probe that in fact succeeded and was billed for.
//
// Both arms are exercised because they are different statements: the INSERT and
// the ON CONFLICT ... DO UPDATE a re-test takes.
func TestModelAvailabilityStore_Postgres_ReturnedRow_AppPool(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "availability-rls")

	if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		store := pgstore.NewForTx(tx, pgtest.SecretKey).ModelAvailability
		read := func() (*domain.ModelAvailability, error) {
			return store.Get(t.Context(), orgID, "anthropic", "claude-sonnet-5")
		}

		green, err := store.Record(t.Context(), orgID, "anthropic", "claude-sonnet-5", domain.ModelAvailabilityGreen, "")
		if err != nil {
			t.Fatalf("Record(green) on the app pool: %v", err)
		}
		dbtest.AssertWriteReturnedStoredRow(t, "Record insert arm", green, read)

		red, err := store.Record(t.Context(), orgID, "anthropic", "claude-sonnet-5", domain.ModelAvailabilityRed, "AccessDeniedException")
		if err != nil {
			t.Fatalf("Record(red) on the app pool: %v", err)
		}
		dbtest.AssertWriteReturnedStoredRow(t, "Record conflict arm", red, read)
		return nil
	}); err != nil {
		t.Fatalf("WithUser: %v", err)
	}
}

// TestModelAvailabilityStore_Postgres_NonAdminCannotWrite pins the half of the
// gate that lives in the database. The handler's RequireOrgAdmin is what a
// caller sees, but this row is the receipt for a real billed request, so a
// plain member reaching the store must not be able to mint one — and must
// still be able to READ them, because that is what renders the badge on their
// own picker.
func TestModelAvailabilityStore_Postgres_NonAdminCannotWrite(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, adminID, teamID := pgtest.SeedOrgWithUser(t, h, "availability-gate")
	memberID := pgtest.SeedUser(t, h, "availability-member")
	pgtest.AddOrgMember(t, h, memberID, orgID, teamID, "member", "member")

	if err := h.WithUser(t, adminID, orgID, func(tx *sql.Tx) error {
		_, err := pgstore.NewForTx(tx, pgtest.SecretKey).ModelAvailability.
			Record(t.Context(), orgID, "anthropic", "claude-sonnet-5", domain.ModelAvailabilityGreen, "")
		return err
	}); err != nil {
		t.Fatalf("org admin Record: %v", err)
	}

	if err := h.WithUser(t, memberID, orgID, func(tx *sql.Tx) error {
		rows, err := pgstore.NewForTx(tx, pgtest.SecretKey).ModelAvailability.List(t.Context(), orgID)
		if err != nil {
			return err
		}
		if len(rows) != 1 {
			t.Errorf("member List = %v, want the org's one row", rows)
		}
		return nil
	}); err != nil {
		t.Fatalf("member List: %v — a member must be able to read the badge on their own picker", err)
	}

	// The refused write gets its own transaction: an RLS denial aborts the one
	// it happens in, so sharing with the read above would fail at commit and
	// say nothing about which statement the policy stopped.
	if err := h.WithUser(t, memberID, orgID, func(tx *sql.Tx) error {
		_, err := pgstore.NewForTx(tx, pgtest.SecretKey).ModelAvailability.
			Record(t.Context(), orgID, "anthropic", "claude-opus-5", domain.ModelAvailabilityGreen, "")
		return err
	}); err == nil {
		t.Error("a plain member minted an availability row; the write policy is org-admin only")
	}
}
