package postgres_test

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestUsersStore_Postgres_UserIDsForVerifiedEmailSystem exercises the
// verified-email lookup backing the Slack identity resolver: a verified match
// is returned, an unverified one is excluded, and the match is
// case-insensitive. A verified address held by two principals cannot be
// seeded — the baseline's exclusion constraint refuses it — so the result is
// at most one id. Postgres-only: user_identities is a multi-mode-only auth
// table absent from SQLite.
func TestUsersStore_Postgres_UserIDsForVerifiedEmailSystem(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	verifiedUser := pgtest.SeedUser(t, h, "verified-owner")
	unverifiedUser := pgtest.SeedUser(t, h, "unverified-owner")
	pgtest.SeedIdentity(t, h, verifiedUser, "match@example.com", true)
	pgtest.SeedIdentity(t, h, unverifiedUser, "unverified@example.com", false)

	t.Run("verified match, case-insensitive", func(t *testing.T) {
		got, err := stores.Users.UserIDsForVerifiedEmailSystem(ctx, "MATCH@Example.com")
		if err != nil {
			t.Fatalf("UserIDsForVerifiedEmailSystem: %v", err)
		}
		if len(got) != 1 || got[0] != verifiedUser {
			t.Errorf("got = %v; want [%s]", got, verifiedUser)
		}
	})

	t.Run("unverified email never matches", func(t *testing.T) {
		got, err := stores.Users.UserIDsForVerifiedEmailSystem(ctx, "unverified@example.com")
		if err != nil {
			t.Fatalf("UserIDsForVerifiedEmailSystem: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got = %v; want none (email is unverified)", got)
		}
	})

	t.Run("no matching row at all", func(t *testing.T) {
		got, err := stores.Users.UserIDsForVerifiedEmailSystem(ctx, "nobody@example.com")
		if err != nil {
			t.Fatalf("UserIDsForVerifiedEmailSystem: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got = %v; want none", got)
		}
	})

	t.Run("a second identity of the same principal is one result", func(t *testing.T) {
		pgtest.SeedIdentity(t, h, verifiedUser, "Match@example.com", true)
		got, err := stores.Users.UserIDsForVerifiedEmailSystem(ctx, "match@example.com")
		if err != nil {
			t.Fatalf("UserIDsForVerifiedEmailSystem: %v", err)
		}
		if len(got) != 1 || got[0] != verifiedUser {
			t.Errorf("got = %v; want [%s]", got, verifiedUser)
		}
	})
}

// TestUsersVerifiedEmail_Postgres_Conformance runs the shared
// HoldsVerifiedEmailSystem suite against the admin-pool read.
func TestUsersVerifiedEmail_Postgres_Conformance(t *testing.T) {
	dbtest.RunUsersVerifiedEmailConformance(t, func(t *testing.T) (db.UsersStore, *dbtest.LoginIdentitySeeder) {
		h := pgtest.Shared(t)
		h.Reset(t)
		return pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey).Users, loginIdentitySeeder(h)
	})
}
