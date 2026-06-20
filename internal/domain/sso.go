package domain

import "time"

// SSOConnection is one row of the sso_connections table — a TF-owned
// binding between an org and a GoTrue IdP provider. TF owns the
// authorization half (the org + the role a JIT-provisioned user gets);
// GoTrue owns authN + the provider registry. ProviderID (GoTrue's
// sso_providers.id, a UUID handled as an opaque string) is the single
// bridge between the two systems, and is globally unique so it maps to
// exactly one org.
//
// Multi-mode only: local mode (N=1) never registers a connection.
type SSOConnection struct {
	ID          string
	OrgID       string
	Kind        string // 'saml' | 'oidc'
	ProviderID  string // GoTrue sso_providers.id (UUID), opaque to TF
	DefaultRole string // org_role JIT grants: 'admin' | 'member' (never 'owner')
	Enabled     bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
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

// UpdateSSOConnectionParams is the input to SSOConnectionStore.Update — the
// mutable management fields. ProviderID and Kind are not updatable here
// (rotating a provider is a deferred concern, TFAC-422 out-of-scope).
type UpdateSSOConnectionParams struct {
	DefaultRole string
	Enabled     bool
}

// SSOProviderBinding is the projection SSOConnectionStore.GetByProviderID
// returns — exactly what the login-time JIT path (TFAC-426) needs to grant
// org membership, keyed on the provider_id the GoTrue assertion carries.
// No connection identity beyond the binding itself: the provider_id is the
// caller's lookup key.
type SSOProviderBinding struct {
	OrgID       string
	DefaultRole string
	Enabled     bool
}

// SSODomain is one row of the sso_domains table — an email domain an org
// has claimed (and, once VerifiedAt is set, proven control of via DNS-TXT)
// to route identifier-first SSO login. The token is the public DNS-TXT
// verification value, not a secret. VerifiedAt is nil while the claim is
// pending; a pending row is inert (doesn't route, doesn't reserve the
// domain against other orgs).
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
// returns — the connection + provider a verified email domain routes to,
// for the identifier-first discovery endpoint (TFAC-427). Enabled rides
// along so the caller can refuse to route through a disabled connection;
// the store does not filter on it (the lookup key is the verified domain).
type SSODomainRoute struct {
	ConnectionID string
	OrgID        string
	ProviderID   string
	Enabled      bool
}
