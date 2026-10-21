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

	// Readdress moves every login identity of principalID that carries the
	// address from to the address to, keeping each identity's email_verified,
	// as a sign-in does when the identity provider now asserts another
	// address. It frees from for another principal to verify.
	Readdress func(t *testing.T, principalID, from, to string)
}
