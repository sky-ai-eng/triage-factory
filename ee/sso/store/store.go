// Package ssostore is the Enterprise Edition SSO data layer: the domain
// types, the store interfaces, and the typed accessors that retrieve the
// SSO store bundle from core's opaque per-transaction extension slot.
//
// Keeping the SSO types and interfaces here is what lets core hold zero SSO
// symbols: core's db.TxStores / db.Stores carry only an opaque Ext map; this
// package supplies the typed view back. The backend implementations live in
// the pg/ and lite/ subpackages, which register their factories with core's
// store-extension registry (see store/pg, store/lite).
//
// Enterprise Edition — governed by the repository-root LICENSE (Triage Factory
// License 1.0); enabling its features requires a valid license key.
package ssostore

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// ExtKey is the registry key the SSO store bundle is registered + read
// under in core's db.TxStores / db.Stores extension slot.
const ExtKey = "sso"

// Bundle is the SSO store set for one construction (one tx, or the non-tx
// aggregate). It is the value the registered factory returns as `any` and
// that FromTx / FromStores assert back to. Pool split lives inside each
// store impl exactly as it did when these stores were core fields.
type Bundle struct {
	Connections SSOConnectionStore
	Domains     SSODomainStore
	BreakGlass  SSOBreakGlassStore
}

// FromTx returns the tx-bound SSO bundle, or nil if no SSO extension is
// registered (a community build with the Enterprise Edition not linked
// in — in which case no SSO handler is mounted to call this).
func FromTx(tx db.TxStores) *Bundle {
	b, _ := tx.Extension(ExtKey).(*Bundle)
	return b
}

// FromStores returns the non-tx SSO bundle (admin-pool reads for the
// login path), or nil if no SSO extension is registered.
func FromStores(s db.Stores) *Bundle {
	b, _ := s.Extension(ExtKey).(*Bundle)
	return b
}

// --- Domain types (moved verbatim from internal/domain/sso.go) ---

// SSOConnection is one row of the sso_connections table — a TF-owned
// binding between an org and a GoTrue IdP provider. TF owns the
// authorization half (the org + the role a JIT-provisioned user gets);
// GoTrue owns authN + the provider registry. ProviderID (GoTrue's
// sso_providers.id, a UUID handled as an opaque string) is the single
// bridge between the two systems, and is globally unique so it maps to
// exactly one org.
type SSOConnection struct {
	ID          string
	OrgID       string
	Kind        string // 'saml' | 'oidc'
	ProviderID  string // GoTrue sso_providers.id (UUID), opaque to TF
	DefaultRole string // org_role JIT grants: 'admin' | 'member' (never 'owner')
	Enabled     bool
	// Enforced is the "Require SSO" switch: when true, a non-SSO (GitHub)
	// login whose verified email is on one of this connection's verified
	// domains is rejected unless the principal is break-glass. Separate
	// axis from Enabled ("allow SSO" vs "require SSO").
	Enforced bool
	// LastTestedAt is stamped when a verify-before-enforce Test passes
	// end-to-end. nil = never passed a Test. Enforcement gates on non-nil.
	LastTestedAt *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// CreateSSOConnectionParams is the input to SSOConnectionStore.Create.
// Kind and DefaultRole fall back to the schema defaults ('saml', 'member')
// when empty; Enabled is not a param — a freshly registered connection is
// always created enabled, and disabling is an Update.
type CreateSSOConnectionParams struct {
	OrgID       string
	Kind        string // "" → 'saml'
	ProviderID  string
	DefaultRole string // "" → 'member'
}

// UpdateSSOConnectionParams is the input to SSOConnectionStore.Update —
// the mutable management fields. An empty DefaultRole leaves the stored
// role unchanged (a disable-only update passes just Enabled). ProviderID
// and Kind are not updatable here.
type UpdateSSOConnectionParams struct {
	DefaultRole string
	Enabled     bool
}

// SSOProviderBinding is the projection SSOConnectionStore.GetByProviderID
// returns — exactly what the login-time JIT path needs to grant org
// membership, keyed on the provider_id the GoTrue assertion carries.
type SSOProviderBinding struct {
	OrgID       string
	DefaultRole string
	Enabled     bool
}

// SSODomain is one row of the sso_domains table — an email domain an org
// has claimed (and, once VerifiedAt is set, proven control of via
// DNS-TXT) to route identifier-first SSO login. The token is the public
// DNS-TXT verification value, not a secret. VerifiedAt is nil while
// pending; a pending row is inert.
type SSODomain struct {
	ID           string
	ConnectionID string
	OrgID        string
	Domain       string
	Token        string
	VerifiedAt   *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// CreateSSODomainParams is the input to SSODomainStore.Create. The row is
// created pending (verified_at NULL); SetVerified stamps it later.
type CreateSSODomainParams struct {
	ConnectionID string
	OrgID        string
	Domain       string
	Token        string
}

// SSODomainRoute is the projection SSODomainStore.GetVerifiedByDomain
// returns — the connection + provider a verified email domain routes to.
// Enabled + Enforced ride along so the login-path enforcement check can
// resolve, in one read, whether a non-SSO login on this domain must be
// rejected. The store does not filter on either.
type SSODomainRoute struct {
	ConnectionID string
	OrgID        string
	ProviderID   string
	Enabled      bool
	Enforced     bool
}

// SSOBreakGlassPrincipal is one row of the sso_break_glass table — a
// principal designated to retain non-SSO (GitHub) login under enforcement,
// the recovery path if the IdP breaks. Email/DisplayName are joined from
// the principal's identity for the admin view; the login-time check needs
// only (OrgID, UserID) and goes through IsBreakGlass.
type SSOBreakGlassPrincipal struct {
	UserID      string
	DisplayName string
	Email       string
	CreatedAt   time.Time
}

// --- Store interfaces ---

// SSOConnectionStore owns the sso_connections table — the TF-owned org↔IdP
// binding. Postgres-only; the SQLite impl is a stub returning
// db.ErrNotApplicableInLocal. App pool for the admin-facing CRUD (RLS
// gates on org-admin); admin pool for the login-time GetByProviderID /
// MarkTestedByProviderID reads, whose actor has no membership yet (the
// provider_id is the authorization).
type SSOConnectionStore interface {
	Create(ctx context.Context, p CreateSSOConnectionParams) (string, error)
	GetByID(ctx context.Context, orgID, id string) (*SSOConnection, error)
	ListByOrg(ctx context.Context, orgID string) ([]SSOConnection, error)
	Update(ctx context.Context, orgID, id string, p UpdateSSOConnectionParams) error
	Delete(ctx context.Context, orgID, id string) error
	GetByProviderID(ctx context.Context, providerID string) (*SSOProviderBinding, error)
	SetEnforced(ctx context.Context, orgID, id string, enforced bool) error
	MarkTestedByProviderID(ctx context.Context, providerID string) error
}

// SSOBreakGlassStore owns the sso_break_glass table — the principals that
// may still authenticate via their non-SSO identity under enforcement.
// Postgres-only. App pool for the org-admin mutations; admin pool for the
// reads that join cross-principal identity data (List, login-time
// IsBreakGlass, email resolution).
type SSOBreakGlassStore interface {
	List(ctx context.Context, orgID string) ([]SSOBreakGlassPrincipal, error)
	Add(ctx context.Context, orgID, userID string) error
	RemoveGuarded(ctx context.Context, orgID, userID string) (blockedLast bool, err error)
	SeedOwnerIfEmpty(ctx context.Context, orgID string) error
	IsBreakGlass(ctx context.Context, orgID, userID string) (bool, error)
	ResolveVerifiedEmailMember(ctx context.Context, orgID, email string) (string, error)
}

// SSODomainStore owns the sso_domains table — the verified email domains
// that route identifier-first SSO login. Postgres-only. App pool for the
// admin-facing claim/verify/CRUD; admin pool for GetVerifiedByDomain (the
// routing actor is pre-login; the verified domain is the authorization).
type SSODomainStore interface {
	Create(ctx context.Context, p CreateSSODomainParams) (string, error)
	ListByOrg(ctx context.Context, orgID string) ([]SSODomain, error)
	GetByID(ctx context.Context, orgID, id string) (*SSODomain, error)
	SetVerified(ctx context.Context, orgID, id string) error
	Delete(ctx context.Context, orgID, id string) error
	GetVerifiedByDomain(ctx context.Context, domainName string) (*SSODomainRoute, error)
}
