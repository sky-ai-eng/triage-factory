package postgres_test

import (
	"context"
	"sort"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestUsersStore_Postgres_UserIDsForVerifiedEmailSystem exercises the
// set-valued verified-email lookup backing TFAC-531's Slack identity
// resolver: a verified match is returned, an unverified one is excluded,
// the match is case-insensitive, and a verified email shared by two
// principals returns BOTH — the resolver's "never guess on ambiguity" case
// depends on seeing the full set, not just the first hit. Postgres-only:
// user_identities is a multi-mode-only auth table absent from SQLite.
func TestUsersStore_Postgres_UserIDsForVerifiedEmailSystem(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	verifiedUser := pgtest.SeedUser(t, h, "verified-owner")
	unverifiedUser := pgtest.SeedUser(t, h, "unverified-owner")
	dupUserA := pgtest.SeedUser(t, h, "dup-a")
	dupUserB := pgtest.SeedUser(t, h, "dup-b")

	pgtest.SeedIdentity(t, h, verifiedUser, "match@example.com", true)
	pgtest.SeedIdentity(t, h, unverifiedUser, "unverified@example.com", false)
	pgtest.SeedIdentity(t, h, dupUserA, "dup@example.com", true)
	pgtest.SeedIdentity(t, h, dupUserB, "dup@example.com", true)

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

	t.Run("two principals share one verified email: both returned", func(t *testing.T) {
		got, err := stores.Users.UserIDsForVerifiedEmailSystem(ctx, "dup@example.com")
		if err != nil {
			t.Fatalf("UserIDsForVerifiedEmailSystem: %v", err)
		}
		sort.Strings(got)
		want := []string{dupUserA, dupUserB}
		sort.Strings(want)
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("got = %v; want both %v (the ambiguous-match set)", got, want)
		}
	})
}
