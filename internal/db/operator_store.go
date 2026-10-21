package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// OperatorStore owns the operators table — the deployment-operator grants
// managed by the `triagefactory operator` CLI. Operators are org-less (fleet
// administration is deployment-scoped), so the table is not org-scoped and
// every method is admin-pool-only in Postgres. SQLite is N=1 and the table is
// effectively unused there (the single local user is implicitly the operator).
//
// The CLI names a grant by email, lower-cased and trimmed at the store
// boundary so callers never normalize. The grant rests on the principal the
// address resolves to (Operator.UserID): Add binds it when a login identity
// already holds the address verified, and a verified sign-in binds a pending
// row inside the login transaction (resolveOrCreatePrincipal in
// internal/server). A bound grant admits its principal and nobody else,
// wherever the address goes afterwards; only a pending grant is matched by
// address.
type OperatorStore interface {
	// Add records email as an operator and returns the stored row. When a
	// login identity holds the address verified, the row is bound to that
	// identity's principal. Re-adding an existing operator keeps its added_at
	// and added_by, and binds the row if it is still pending and the address
	// now has a verified holder. A bound row is never re-bound, so moving a
	// grant to another principal is a remove and an add. addedBy is
	// best-effort provenance.
	Add(ctx context.Context, email, addedBy string) (domain.Operator, error)

	// Remove drops email from the operator set. removed is false when the
	// email was not an operator (so the CLI can report "not an operator").
	//
	// Exempt from the returned-row rule: it is a delete.
	Remove(ctx context.Context, email string) (removed bool, err error)

	// IsOperatorUserSystem reports whether the principal userID is a
	// deployment operator: whether a grant is bound to it, or whether one of
	// its login identities holds the address of a pending grant with
	// email_verified set. email_verified is TF's trust decision for the
	// address (a SAML identity's counts only on a domain its connection's org
	// has verified), so an identity provider that merely asserts an
	// operator's address does not make its user one. A grant bound to another
	// principal never matches by address, whoever holds the address now. The
	// token's email claim is never consulted.
	//
	// Admin pool / claims-free: operators has no policy and the caller is the
	// deployment-scoped fleet gate. SQLite matches bound grants only: local
	// mode has no login identities, and the gate admits its single user
	// before asking.
	IsOperatorUserSystem(ctx context.Context, userID string) (bool, error)

	// List returns every operator, email order — backs `operator list`.
	List(ctx context.Context) ([]domain.Operator, error)
}
