package postgres_test

import (
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
