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

// The seeder stages the principals and login identities Add and
// IsOperatorUserSystem resolve an address through.
func TestOperatorStore_Postgres_Conformance(t *testing.T) {
	dbtest.RunOperatorStoreConformance(t, func(t *testing.T) (db.OperatorStore, *dbtest.LoginIdentitySeeder) {
		h := pgtest.Shared(t)
		h.Reset(t)
		return pgstore.NewOperatorStore(h.AdminDB), loginIdentitySeeder(h)
	})
}

// TestOperatorStore_Postgres_MixedCaseRowMatches: Add stores operators
// lower-cased, but a pending row written by hand in another case must still
// match the principal holding that address.
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
		Readdress: func(t *testing.T, principalID, from, to string) {
			t.Helper()
			pgtest.MustExec(t, h.AdminDB, `
				UPDATE user_identities SET email = $3
				 WHERE user_id = $1 AND lower(email) = lower($2)`, principalID, from, to)
		},
	}
}

// TestOperatorStore_Postgres_DeletingThePrincipalDeletesTheGrant pins the
// operators.user_id FK's ON DELETE CASCADE. Setting the column back to NULL
// instead would make the row pending again, and the next principal to verify
// the address would hold the grant.
func TestOperatorStore_Postgres_DeletingThePrincipalDeletesTheGrant(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()
	store := pgstore.NewOperatorStore(h.AdminDB)

	principal := pgtest.SeedUser(t, h, "deleted-operator")
	pgtest.SeedIdentity(t, h, principal, "ops@example.com", true)
	op, err := store.Add(ctx, "ops@example.com", "root")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if op.UserID != principal {
		t.Fatalf("Add bound %q, want %s", op.UserID, principal)
	}

	pgtest.MustExec(t, h.AdminDB, `DELETE FROM users WHERE id = $1`, principal)

	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("operators after deleting the bound principal = %+v, want none", list)
	}
}
