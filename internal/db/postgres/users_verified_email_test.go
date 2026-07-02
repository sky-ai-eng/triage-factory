package postgres_test

import (
	"context"
	"sort"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// seedIdentity inserts a public.user_identities row linking a fresh
// auth.users identity to principalID, mirroring the seeding pattern used by
// internal/server's auth_handlers_test.go (there is no pgtest helper for
// this multi-mode-only auth table, so tests write it directly via the
// admin pool like the login path itself would).
func seedIdentity(t *testing.T, h *pgtest.Harness, principalID, email string, verified bool) {
	t.Helper()
	var authUserID string
	if err := h.AdminDB.QueryRow(`SELECT gen_random_uuid()`).Scan(&authUserID); err != nil {
		t.Fatalf("gen auth_user_id: %v", err)
	}
	h.SeedAuthUser(t, authUserID, authUserID+"@auth.test")
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO user_identities (auth_user_id, user_id, provider, email, email_verified)
		VALUES ($1, $2, 'github', $3, $4)
	`, authUserID, principalID, email, verified)
}

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

	seedIdentity(t, h, verifiedUser, "match@example.com", true)
	seedIdentity(t, h, unverifiedUser, "unverified@example.com", false)
	seedIdentity(t, h, dupUserA, "dup@example.com", true)
	seedIdentity(t, h, dupUserB, "dup@example.com", true)

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
