package dbtest

import "testing"

// LoginIdentitySeeder stages principals and their login identities
// (public.user_identities) for the suites whose reads resolve a person through
// a verified address. No store method writes an identity — the OAuth callback
// does, inline — so each backend seeds them against its own schema.
//
// A backend without login identities (SQLite: local mode has no sign-in)
// passes a nil seeder, and the suites then pin that every such read answers
// false rather than erroring.
type LoginIdentitySeeder struct {
	// Principal inserts a users row and returns its id.
	Principal func(t *testing.T) string

	// Identity attaches a fresh login identity to principalID carrying email
	// with the given email_verified flag.
	Identity func(t *testing.T, principalID, email string, verified bool)
}
