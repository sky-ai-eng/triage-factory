package postgres_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// operators is keyed on email (no FK); the seeder stages the principals and
// login identities IsOperatorUserSystem resolves through.
func TestOperatorStore_Postgres_Conformance(t *testing.T) {
	dbtest.RunOperatorStoreConformance(t, func(t *testing.T) (db.OperatorStore, *dbtest.LoginIdentitySeeder) {
		h := pgtest.Shared(t)
		h.Reset(t)
		return pgstore.NewOperatorStore(h.AdminDB), loginIdentitySeeder(h)
	})
}

// TestOperatorStore_Postgres_MixedCaseRowMatches: Add stores operators
// lower-cased, but a row written by hand in another case must still match the
// principal holding that address.
func TestOperatorStore_Postgres_MixedCaseRowMatches(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	pgtest.MustExec(t, h.AdminDB, `INSERT INTO operators (email) VALUES ('Ops@Example.COM')`)
	principal := pgtest.SeedUser(t, h, "mixed-case-operator")
	pgtest.SeedIdentity(t, h, principal, "ops@example.com", true)

	ok, err := pgstore.NewOperatorStore(h.AdminDB).IsOperatorUserSystem(context.Background(), principal)
	if err != nil {
		t.Fatalf("IsOperatorUserSystem: %v", err)
	}
	if !ok {
		t.Error("a hand-written mixed-case operator row did not match the principal holding that address")
	}
}

// loginIdentitySeeder is the Postgres seeder for the suites that resolve a
// principal through public.user_identities.
func loginIdentitySeeder(h *pgtest.Harness) *dbtest.LoginIdentitySeeder {
	return &dbtest.LoginIdentitySeeder{
		Principal: func(t *testing.T) string {
			t.Helper()
			return pgtest.SeedUser(t, h, "principal-"+uuid.NewString())
		},
		Identity: func(t *testing.T, principalID, email string, verified bool) {
			t.Helper()
			pgtest.SeedIdentity(t, h, principalID, email, verified)
		},
	}
}
