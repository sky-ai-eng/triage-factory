package dbtest

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// UsersVerifiedEmailFactory hands the suite a wired UsersStore plus the
// login-identity seeder its reads resolve through (nil on a backend without
// login identities).
type UsersVerifiedEmailFactory func(t *testing.T) (db.UsersStore, *LoginIdentitySeeder)

// RunUsersVerifiedEmailConformance pins HoldsVerifiedEmailSystem: true only for
// an identity of that principal holding the address verified, matched
// case-insensitively and trimmed; false for an unverified holder, for another
// principal's address, for an empty address and on a backend without login
// identities.
func RunUsersVerifiedEmailConformance(t *testing.T, mk UsersVerifiedEmailFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("holds_verified_email_system", func(t *testing.T) {
		users, seed := mk(t)
		holds := func(t *testing.T, userID, email string) bool {
			t.Helper()
			ok, err := users.HoldsVerifiedEmailSystem(ctx, userID, email)
			if err != nil {
				t.Fatalf("HoldsVerifiedEmailSystem(%s, %q): %v", userID, email, err)
			}
			return ok
		}

		if seed == nil {
			if holds(t, uuid.NewString(), "a@example.com") {
				t.Fatal("a backend without login identities must answer false")
			}
			return
		}

		alice := seed.Principal(t)
		seed.Identity(t, alice, "alice@example.com", true)
		seed.Identity(t, alice, "alice-alt@example.com", false)
		bob := seed.Principal(t)
		seed.Identity(t, bob, "bob@example.com", true)

		if !holds(t, alice, "  ALICE@Example.com ") {
			t.Error("alice's verified address (other case, padded) must match")
		}
		if holds(t, alice, "alice-alt@example.com") {
			t.Error("an address alice holds only unverified must not match")
		}
		if holds(t, alice, "bob@example.com") {
			t.Error("another principal's verified address must not match")
		}
		if holds(t, bob, "alice@example.com") {
			t.Error("bob must not hold alice's address")
		}
		if holds(t, alice, "") {
			t.Error("an empty address must not match")
		}
		if holds(t, uuid.NewString(), "alice@example.com") {
			t.Error("an unknown principal must not match")
		}
	})
}
