package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// OperatorStore owns the operators table — the deployment-operator identity
// managed by the `triagefactory operator` CLI. Operators are org-less (fleet
// administration is deployment-scoped), so the table is not org-scoped and
// every method is admin-pool-only in Postgres. SQLite is N=1 and the table is
// effectively unused there (the single local user is implicitly the operator).
//
// All emails are lower-cased/trimmed at the store boundary so callers never
// have to normalize — Add, Remove, and IsOperator all fold case, matching how
// the request gate compares claims.Email.
type OperatorStore interface {
	// Add records email as an operator, idempotently (re-adding an existing
	// operator is a no-op that succeeds). addedBy is best-effort provenance.
	//
	// Exempt from the returned-row rule, by decision rather than by shape: the
	// insert is ON CONFLICT DO NOTHING so a re-add keeps the original
	// provenance, and that returns zero rows precisely on the re-add. The CLI
	// reports the email it was given; nothing renders the row.
	Add(ctx context.Context, email, addedBy string) error

	// Remove drops email from the operator set. removed is false when the
	// email was not an operator (so the CLI can report "not an operator").
	//
	// Exempt from the returned-row rule: it is a delete.
	Remove(ctx context.Context, email string) (removed bool, err error)

	// IsOperator reports whether email is a deployment operator.
	IsOperator(ctx context.Context, email string) (bool, error)

	// List returns every operator, email order — backs `operator list`.
	List(ctx context.Context) ([]domain.Operator, error)
}
