package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// SSOConnectionStore owns the sso_connections table — the TF-owned org↔IdP
// binding. Postgres-only: local mode (N=1) never registers a connection, so
// the SQLite impl is a stub returning ErrNotApplicableInLocal.
//
// # Pool split (Postgres)
//
//   - Create, GetByID, ListByOrg, Update, Delete run on the app pool. The
//     sso_connections_{select,insert,update,delete} RLS policies gate every
//     operation on org-admin in the current org; the request-handler caller
//     has set the JWT claims via the TxRunner.
//   - GetByProviderID runs on the admin pool (BYPASSRLS). The login-time
//     JIT actor (TFAC-426) has no membership in the target org yet, so RLS
//     can't express the lookup — the provider_id the GoTrue assertion
//     carries is the authorization. Mirrors the invites redeem-read split.
type SSOConnectionStore interface {
	// Create inserts a connection and returns its id. Kind / DefaultRole
	// fall back to the schema defaults ('saml' / 'member') when empty; the
	// row is created enabled. A second connection reusing a provider_id
	// already bound anywhere in the deployment trips the global-unique index
	// (sso_connections_provider_uniq) — the handler maps that to 409. App
	// pool.
	Create(ctx context.Context, p domain.CreateSSOConnectionParams) (string, error)

	// GetByID returns the org's connection by id, or (nil, nil) when no row
	// matches in the org. App pool.
	GetByID(ctx context.Context, orgID, id string) (*domain.SSOConnection, error)

	// ListByOrg returns the org's connections, newest first. App pool.
	ListByOrg(ctx context.Context, orgID string) ([]domain.SSOConnection, error)

	// Update sets the mutable management fields on the org's connection.
	// Enabled is always applied; an empty DefaultRole leaves the stored role
	// unchanged (so a disable-only call can't silently downgrade it). A no-op
	// (no error) when no row matches. App pool.
	Update(ctx context.Context, orgID, id string, p domain.UpdateSSOConnectionParams) error

	// Delete removes the org's connection (cascading its sso_domains rows).
	// A no-op (no error) when no row matches. App pool.
	Delete(ctx context.Context, orgID, id string) error

	// GetByProviderID resolves a GoTrue provider_id to its org binding —
	// the JIT isolation boundary. Returns (nil, nil) when the provider is
	// bound to no org. Admin pool (the login actor has no membership yet).
	GetByProviderID(ctx context.Context, providerID string) (*domain.SSOProviderBinding, error)
}

// SSODomainStore owns the sso_domains table — the verified email domains
// that route identifier-first SSO login. Postgres-only; the SQLite impl is
// a stub returning ErrNotApplicableInLocal.
//
// # Pool split (Postgres)
//
//   - Create, ListByOrg, GetByID, SetVerified, Delete run on the app pool,
//     gated by the sso_domains_* RLS policies (org-admin in the current
//     org).
//   - GetVerifiedByDomain runs on the admin pool (BYPASSRLS). The routing
//     actor (TFAC-427 discovery endpoint) is pre-login with no membership;
//     the verified domain is the lookup authorization, not an RLS predicate.
type SSODomainStore interface {
	// Create inserts a pending domain claim (verified_at NULL) and returns
	// its id. A re-claim of the same (org, domain) trips the per-org unique
	// index (sso_domains_org_domain_uniq); a pending claim of a domain
	// another org has only *pending* is allowed (first-to-verify wins). App
	// pool.
	Create(ctx context.Context, p domain.CreateSSODomainParams) (string, error)

	// ListByOrg returns the org's domain claims (pending and verified),
	// newest first. App pool.
	ListByOrg(ctx context.Context, orgID string) ([]domain.SSODomain, error)

	// GetByID returns the org's domain claim by id, or (nil, nil) when no
	// row matches in the org. App pool.
	GetByID(ctx context.Context, orgID, id string) (*domain.SSODomain, error)

	// SetVerified stamps verified_at = now() on the org's pending claim.
	// This is the moment the global-uniqueness guarantee is enforced: if
	// another org has already verified the same domain, the
	// sso_domains_verified_global_uniq index trips and SetVerified returns
	// that (opaque) unique-violation error. A no-op (no error) when no row
	// matches. App pool.
	SetVerified(ctx context.Context, orgID, id string) error

	// Delete removes the org's domain claim. A no-op (no error) when no row
	// matches. App pool.
	Delete(ctx context.Context, orgID, id string) error

	// GetVerifiedByDomain resolves an email domain to the connection it
	// routes to — exact lower(domain) match, VERIFIED rows only (pending
	// claims are ignored). No suffix / longest-match: corp.com never matches
	// eng.corp.com. Returns (nil, nil) when no verified row matches. Admin
	// pool (the routing actor is pre-login).
	GetVerifiedByDomain(ctx context.Context, domainName string) (*domain.SSODomainRoute, error)
}
