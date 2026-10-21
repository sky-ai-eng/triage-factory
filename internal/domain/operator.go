package domain

import "time"

// Operator is one row in the operators table — a deployment-operator grant.
// Operators are org-less (fleet administration is deployment-scoped, not a
// member role); the flag is managed only through the `triagefactory operator`
// CLI and gates the fleet console together with the FeatureFleet deployment
// entitlement.
type Operator struct {
	// Email is the lower-cased address the CLI named, the key `operator add`
	// and `operator remove` take.
	Email string
	// UserID is the principal the grant is bound to, and the only one it
	// admits once set. Empty while the grant is pending: no login identity
	// held Email verified when it was added, and nobody holding it verified
	// has signed in since.
	UserID  string
	AddedAt time.Time
	// AddedBy is the OS user that ran the CLI, best-effort provenance for the
	// `operator list` read-out — never an authorization input.
	AddedBy string
}

// Pending reports whether the grant is bound to no principal yet.
func (o Operator) Pending() bool { return o.UserID == "" }
