package domain

import "time"

// Operator is one row in the operators table — a deployment-operator identity,
// keyed by lower-cased email. Operators are org-less (fleet administration is
// deployment-scoped, not a member role); the flag is managed only through the
// `triagefactory operator` CLI and gates the fleet console together with the
// FeatureFleet deployment entitlement.
type Operator struct {
	Email   string
	AddedAt time.Time
	// AddedBy is the OS user that ran the CLI, best-effort provenance for the
	// `operator list` read-out — never an authorization input.
	AddedBy string
}
