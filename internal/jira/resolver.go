package jira

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// Sentinel errors returned by the Resolver.
var (
	// ErrNoJiraSystemCredential is returned by ForSystem when the org has no
	// stored Jira service credential (URL + PAT). The poller skips it
	// silently; the exec CLI maps it to the "Jira not configured" message.
	ErrNoJiraSystemCredential = errors.New("jira: no system credential for org")

	// ErrNoJiraUserCredential is returned by ForUser when the acting user has
	// no stored Jira credential for the org's Jira host. It is the
	// defense-in-depth boundary for the per-user write path: the resolver
	// NEVER degrades to the org service credential, because acting as the bot
	// would mis-attribute a user-initiated ticket action — a board claim would
	// assign the ticket to the service account, not the claimer. Handlers
	// surface it as a "connect your Jira" 409/422 rather than acting as the bot.
	ErrNoJiraUserCredential = errors.New("jira: no user credential for org/user")
)

// The well-known org-level Jira secret keys. They mirror integrations.KeyJira*
// verbatim and are kept as local constants on purpose: internal/integrations
// transitively imports internal/jira (via internal/auth.ValidateJira), so
// importing it here would be a cycle. Keep them in sync with
// internal/integrations; keys_drift_test pins the agreement.
//
// keyJiraURL is shared by both backends. The remaining keys split by scheme:
// keyJiraPAT is the Data Center PAT (Bearer); keyJiraEmail + keyJiraAPIToken
// are the Cloud API-token pair (Basic); keyJiraAuthMethod is the AuthMethod
// marker recording which of the two the org uses.
const (
	keyJiraURL        = "jira_url"
	keyJiraPAT        = "jira_pat"
	keyJiraEmail      = "jira_email"
	keyJiraAPIToken   = "jira_api_token"
	keyJiraAuthMethod = "jira_auth_method"
)

// CanonicalHost canonicalizes an org's configured Jira base URL into the value
// the per-user credential is keyed under ("jira_token/<host>") AND the origin a
// per-user Client talks to. It trims a trailing slash + surrounding whitespace
// and requires a real http(s) origin; ok=false on an empty ("Jira not
// configured") or malformed base URL. This is the single source of truth the
// bind flow (server.resolveJiraHost) and this resolver both compose, so a
// stored credential always reads back under the key it was written with.
func CanonicalHost(orgBase string) (string, bool) {
	host := strings.TrimRight(strings.TrimSpace(orgBase), "/")
	if host == "" {
		return "", false
	}
	u, err := url.Parse(host)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	return host, true
}

// UserTokenKey is the per-user secret key a user's Jira access token is
// custodied under — host-scoped so a user can hold credentials on more than one
// Jira host (forward-compat; v1 is single-host). Composed here so the bind-flow
// writer (server.handleJiraIdentityPAT) and this resolver stay in lockstep.
func UserTokenKey(host string) string {
	return "jira_token/" + host
}

// UserCredential is the structured per-user Jira access secret stored under
// UserTokenKey(host). The method marker lets ForUser rebuild the right client
// without re-sniffing the host: a Cloud API token (Basic auth over email +
// token, REST v3) or a Data Center PAT (Bearer, REST v2, token only). It is the
// per-user mirror of the org-side jira_auth_method marker (integrations) — the
// resolver dispatches on Method exactly as ForSystem dispatches on the org
// marker.
//
// Cloud OAuth (the one-click Connect path) adds a third method to this envelope:
// AuthMethodCloudOAuth stores a rotating RefreshToken + the resolved CloudID and
// carries no directly-usable Token (the short-lived access token is minted on
// demand and never persisted).
type UserCredential struct {
	Method AuthMethod `json:"method"`
	// Email is the Atlassian account email; set only for AuthMethodCloudAPIToken
	// (the Basic-auth pair), empty for a DC PAT or a Cloud OAuth credential.
	Email string `json:"email,omitempty"`
	// Token is the static credential value: a DC PAT or a Cloud API token.
	// Empty for AuthMethodCloudOAuth (no static token — the access token is
	// minted from RefreshToken on demand).
	Token string `json:"token,omitempty"`
	// CloudID is the Atlassian site identifier (from accessible-resources),
	// set only for AuthMethodCloudOAuth — it selects the api.atlassian.com
	// gateway base the minted access token talks to.
	CloudID string `json:"cloud_id,omitempty"`
	// RefreshToken is the durable, ROTATING OAuth refresh token, set only for
	// AuthMethodCloudOAuth. Atlassian returns a fresh refresh token on every
	// refresh, so the minter writes a new envelope back after each mint — this
	// field is the one that changes under rotation.
	RefreshToken string `json:"refresh_token,omitempty"`
}

// MarshalUserCredential renders a UserCredential to the JSON envelope persisted
// under the per-user secret key.
func MarshalUserCredential(c UserCredential) (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("jira: marshal user credential: %w", err)
	}
	return string(b), nil
}

// ParseUserCredential decodes a stored per-user secret into a UserCredential.
// It accepts two shapes: the JSON envelope written by the current bind flow, and
// a bare token — the pre-envelope shape the original Data Center bind wrote
// directly under the key — which is read back as a dc_pat for back-compat. The
// envelope is recognized by a leading '{' (neither a DC PAT nor a Cloud API
// token is JSON), so the two never collide. A malformed envelope is an error
// (corruption, not absence) — the caller propagates it rather than degrading to
// the org service credential.
func ParseUserCredential(raw string) (UserCredential, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return UserCredential{}, errors.New("jira: empty user credential")
	}
	if strings.HasPrefix(raw, "{") {
		var c UserCredential
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			return UserCredential{}, fmt.Errorf("jira: parse user credential envelope: %w", err)
		}
		return c, nil
	}
	// Back-compat: a bare token from the original DC bind, before the envelope.
	return UserCredential{Method: AuthMethodDCPAT, Token: raw}, nil
}

// UsableFor reports whether the credential is well-formed for the given
// deployment — the network-free precondition ForUser enforces before building a
// client. A method that doesn't match the deployment, or a missing required
// field, is not usable. Shared by ForUser's dispatch and the status reader so
// "connected" means the same thing as "ForUser would succeed".
func (c UserCredential) UsableFor(d Deployment) bool {
	switch c.Method {
	case AuthMethodCloudAPIToken:
		return d == DeploymentCloud && c.Email != "" && c.Token != ""
	case AuthMethodCloudOAuth:
		return d == DeploymentCloud && c.CloudID != "" && c.RefreshToken != ""
	case AuthMethodDCPAT:
		return d == DeploymentDataCenter && c.Token != ""
	default:
		return false
	}
}

// Resolver produces an authenticated *Client routed by provenance:
//
//   - ForSystem: the org's Jira service credential — the poller's read path and
//     the cmd/exec/jira agent-triage write surface. Bot-attributed by design.
//   - ForUser: the acting user's own stored credential — board claim / undo /
//     requeue. User-attributed; required, never falls back to the org cred.
//
// All store reads use the ...System (claims-free) doors: credential resolution
// is a system operation and the (orgID, userID) are already authorized by
// upstream middleware (request path) or are the trusted local / poll-cycle
// identifiers. This lets one resolver serve both request handlers and
// background callers. Mirrors internal/github.Resolver.
type Resolver interface {
	ForSystem(ctx context.Context, orgID string) (*Client, error)
	ForUser(ctx context.Context, orgID, userID string) (*Client, error)
}

// UserAccessTokenSource mints a short-lived Cloud OAuth access token for a
// user, handling refresh-token rotation write-back internally. It is the seam
// the resolver's AuthMethodCloudOAuth branch delegates to — implemented by
// internal/jiraoauth, wired in at construction so internal/jira doesn't import
// the OAuth/HTTP machinery (and there's no import cycle: jiraoauth imports jira,
// not the reverse). A nil source means OAuth minting isn't wired (the system
// resolver used by background board-mirror callers, where ForUser-OAuth never
// runs); ForUser surfaces a clear error rather than panicking in that case.
//
// host is the canonical org Jira host the credential is keyed under (the same
// value ForUser already resolved); the implementation reads the stored
// envelope, mints/refreshes, persists the rotated refresh token, and returns
// the cloud_id + access token to build a CloudOAuth client.
type UserAccessTokenSource interface {
	AccessTokenForUser(ctx context.Context, orgID, userID, host string) (cloudID, accessToken string, err error)
}

type resolver struct {
	secrets db.SecretStore
	orgs    db.OrgsStore
	oauth   UserAccessTokenSource // nil when OAuth minting isn't wired
}

// NewResolver builds a Resolver from the secret + org-settings stores, without
// Cloud OAuth minting. Use NewResolverWithOAuth on the request-serving path
// where per-user Cloud OAuth Connect credentials must resolve; the OAuth-free
// form is for background callers that only ever take ForSystem (the board-mirror
// spawner) or that predate Cloud OAuth.
func NewResolver(secrets db.SecretStore, orgs db.OrgsStore) Resolver {
	return &resolver{secrets: secrets, orgs: orgs}
}

// NewResolverWithOAuth builds a Resolver that can mint per-user Cloud OAuth
// access tokens via the given source (internal/jiraoauth). Everything else is
// identical to NewResolver; only the AuthMethodCloudOAuth branch of ForUser
// depends on the source.
func NewResolverWithOAuth(secrets db.SecretStore, orgs db.OrgsStore, oauth UserAccessTokenSource) Resolver {
	return &resolver{secrets: secrets, orgs: orgs, oauth: oauth}
}

// ForSystem resolves the org's Jira service credential into an authenticated
// client, routed by the stored auth-method marker: a Cloud org gets a Basic /
// REST v3 client (CloudAPIToken, email + token), a Data Center org a Bearer /
// REST v2 one (DataCenterPAT). The base URL is the org's stored jira_url
// secret, canonicalized through CanonicalHost so the system client talks to
// the exact same trimmed/validated origin a per-user client does (ForUser) —
// no drift from a stray trailing slash or whitespace in the stored value.
//
// An absent marker is an org onboarded before Cloud support landed, so the
// deployment is inferred from the host shape (DeploymentForMarker →
// DeploymentForHost): a *.atlassian.net host resolves Cloud, any other origin
// Data Center. Every genuine pre-Cloud org is non-*.atlassian.net (Cloud
// onboarding always writes a marker), so this preserves the historical Data
// Center behavior for them while still classifying a Cloud host correctly.
// Returns ErrNoJiraSystemCredential when the URL is absent/unusable or the
// scheme-appropriate credential is absent; a backend read error propagates so
// a transient vault/keychain outage isn't misreported as "not configured".
func (r *resolver) ForSystem(ctx context.Context, orgID string) (*Client, error) {
	rawURL, err := r.secrets.GetSystem(ctx, orgID, keyJiraURL)
	if err != nil {
		return nil, fmt.Errorf("resolve jira url for org %s: %w", orgID, err)
	}
	host, ok := CanonicalHost(rawURL)
	if !ok {
		return nil, fmt.Errorf("%w: org=%s", ErrNoJiraSystemCredential, orgID)
	}

	method, err := r.secrets.GetSystem(ctx, orgID, keyJiraAuthMethod)
	if err != nil {
		return nil, fmt.Errorf("resolve jira auth method for org %s: %w", orgID, err)
	}

	if DeploymentForMarker(AuthMethod(method), host) == DeploymentCloud {
		email, err := r.secrets.GetSystem(ctx, orgID, keyJiraEmail)
		if err != nil {
			return nil, fmt.Errorf("resolve jira email for org %s: %w", orgID, err)
		}
		token, err := r.secrets.GetSystem(ctx, orgID, keyJiraAPIToken)
		if err != nil {
			return nil, fmt.Errorf("resolve jira api token for org %s: %w", orgID, err)
		}
		if email == "" || token == "" {
			return nil, fmt.Errorf("%w: org=%s", ErrNoJiraSystemCredential, orgID)
		}
		return NewClient(CloudAPIToken(host, email, token)), nil
	}

	pat, err := r.secrets.GetSystem(ctx, orgID, keyJiraPAT)
	if err != nil {
		return nil, fmt.Errorf("resolve jira pat for org %s: %w", orgID, err)
	}
	if pat == "" {
		return nil, fmt.Errorf("%w: org=%s", ErrNoJiraSystemCredential, orgID)
	}
	return NewClient(DataCenterPAT(host, pat)), nil
}

// SystemCredential is the org's Jira service credential in raw, serializable
// form — the fields ForSystem would otherwise build directly into a live
// *Client. It exists for TFAC-614's brain-side credential provisioner, which
// needs to seal the credential into a run's bundle rather than hold a live
// client, and for the executor-side bundle-backed resolver, which
// reconstructs a *Client from these fields with no further secret-store
// read (jira.NewClient(CloudAPIToken(...)) or jira.NewClient(DataCenterPAT(...))).
type SystemCredential struct {
	URL        string
	Deployment Deployment
	Email      string
	APIToken   string
	PAT        string
}

// systemCredentialResolver is the optional Resolver extension the
// production resolver implements — a type assertion, mirroring
// internal/github's ScopedResolver pattern, so this stays additive and
// doesn't touch the Resolver interface (and therefore no existing fake
// needs a new method to keep compiling).
type systemCredentialResolver interface {
	ResolveSystemCredential(ctx context.Context, orgID string) (SystemCredential, error)
}

// ResolveSystemCredential resolves the org's Jira service credential to its
// raw fields — the same resolution ForSystem uses (including the
// Cloud-vs-Data-Center dispatch), stopping short of building a live
// *Client. See SystemCredential's doc for why this exists alongside
// ForSystem rather than replacing it.
func (r *resolver) ResolveSystemCredential(ctx context.Context, orgID string) (SystemCredential, error) {
	rawURL, err := r.secrets.GetSystem(ctx, orgID, keyJiraURL)
	if err != nil {
		return SystemCredential{}, fmt.Errorf("resolve jira url for org %s: %w", orgID, err)
	}
	host, ok := CanonicalHost(rawURL)
	if !ok {
		return SystemCredential{}, fmt.Errorf("%w: org=%s", ErrNoJiraSystemCredential, orgID)
	}

	method, err := r.secrets.GetSystem(ctx, orgID, keyJiraAuthMethod)
	if err != nil {
		return SystemCredential{}, fmt.Errorf("resolve jira auth method for org %s: %w", orgID, err)
	}

	if DeploymentForMarker(AuthMethod(method), host) == DeploymentCloud {
		email, err := r.secrets.GetSystem(ctx, orgID, keyJiraEmail)
		if err != nil {
			return SystemCredential{}, fmt.Errorf("resolve jira email for org %s: %w", orgID, err)
		}
		token, err := r.secrets.GetSystem(ctx, orgID, keyJiraAPIToken)
		if err != nil {
			return SystemCredential{}, fmt.Errorf("resolve jira api token for org %s: %w", orgID, err)
		}
		if email == "" || token == "" {
			return SystemCredential{}, fmt.Errorf("%w: org=%s", ErrNoJiraSystemCredential, orgID)
		}
		return SystemCredential{URL: host, Deployment: DeploymentCloud, Email: email, APIToken: token}, nil
	}

	pat, err := r.secrets.GetSystem(ctx, orgID, keyJiraPAT)
	if err != nil {
		return SystemCredential{}, fmt.Errorf("resolve jira pat for org %s: %w", orgID, err)
	}
	if pat == "" {
		return SystemCredential{}, fmt.Errorf("%w: org=%s", ErrNoJiraSystemCredential, orgID)
	}
	return SystemCredential{URL: host, Deployment: DeploymentDataCenter, PAT: pat}, nil
}

var _ systemCredentialResolver = (*resolver)(nil)

// ForUser resolves the acting user's own Jira credential into an authenticated
// client, keyed under and talking to the org's Jira host. The stored secret is
// a UserCredential envelope whose method marker selects the scheme: a Cloud API
// token yields a Basic / REST v3 client, a Data Center PAT a Bearer / REST v2
// one — the per-user mirror of ForSystem's marker dispatch. A bare token from
// the pre-envelope DC bind is read back as a dc_pat (ParseUserCredential).
//
// The credential is gated on UsableFor(<org deployment>), the SAME predicate the
// status reader uses — so "ForUser succeeds" means exactly what the status
// endpoint reports as connected. A stale cross-scheme credential (e.g. a Cloud
// token left after a Cloud→DC cutover) is therefore ErrNoJiraUserCredential, not
// a wrong-scheme client built against the new host.
//
// REQUIRED — an absent (or unusable) credential is ErrNoJiraUserCredential,
// NEVER a fall-back to the org service cred (that would mis-attribute the user's
// write to the bot). A backend read error propagates rather than being
// misreported as "not connected"; a corrupt envelope or an unknown method
// likewise propagates rather than silently degrading to the bot.
func (r *resolver) ForUser(ctx context.Context, orgID, userID string) (*Client, error) {
	orgSet, err := r.orgs.GetSettingsSystem(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("resolve jira host for org %s: %w", orgID, err)
	}
	host, ok := CanonicalHost(orgSet.JiraBaseURL)
	if !ok {
		// No usable org Jira host → no host to key a user credential under, so
		// there cannot be one. This is the absent-credential boundary, not a
		// transient error.
		return nil, fmt.Errorf("%w: org=%s user=%s (org has no jira host)", ErrNoJiraUserCredential, orgID, userID)
	}
	raw, err := r.secrets.GetUserSystem(ctx, orgID, userID, UserTokenKey(host))
	if err != nil {
		return nil, fmt.Errorf("resolve jira user credential for org %s user %s: %w", orgID, userID, err)
	}
	if raw == "" {
		return nil, fmt.Errorf("%w: org=%s user=%s host=%s", ErrNoJiraUserCredential, orgID, userID, host)
	}
	cred, err := ParseUserCredential(raw)
	if err != nil {
		return nil, fmt.Errorf("resolve jira user credential for org %s user %s: %w", orgID, userID, err)
	}

	// Resolve the org's deployment from its stored auth-method marker (falling
	// back to host shape for a pre-Cloud org) — the same dispatch ForSystem and
	// the status reader use — so the credential's scheme is checked against the
	// org's CURRENT deployment, not just its own self-description.
	method, err := r.secrets.GetSystem(ctx, orgID, keyJiraAuthMethod)
	if err != nil {
		return nil, fmt.Errorf("resolve jira auth method for org %s: %w", orgID, err)
	}
	deployment := DeploymentForMarker(AuthMethod(method), host)

	switch cred.Method {
	case AuthMethodCloudAPIToken, AuthMethodDCPAT:
		// A known scheme that's incomplete OR doesn't match the org's current
		// deployment (a stale cross-scheme cred) is an absent-credential boundary
		// — re-binding is the fix, so surface ErrNoJiraUserCredential.
		if !cred.UsableFor(deployment) {
			return nil, fmt.Errorf("%w: org=%s user=%s host=%s (credential not usable for %s deployment)", ErrNoJiraUserCredential, orgID, userID, host, deployment)
		}
		if cred.Method == AuthMethodCloudAPIToken {
			return NewClient(CloudAPIToken(host, cred.Email, cred.Token)), nil
		}
		return NewClient(DataCenterPAT(host, cred.Token)), nil
	case AuthMethodCloudOAuth:
		// Same usability gate as the static schemes: a Cloud OAuth credential
		// left over after a Cloud→DC cutover (or one missing its cloud_id /
		// refresh token) is an absent-credential boundary, fixed by re-binding.
		if !cred.UsableFor(deployment) {
			return nil, fmt.Errorf("%w: org=%s user=%s host=%s (credential not usable for %s deployment)", ErrNoJiraUserCredential, orgID, userID, host, deployment)
		}
		// The refresh-token → access-token mint (with rotation write-back) lives
		// in the OAuth source. A nil source means this resolver wasn't built with
		// OAuth wiring (a background board-mirror resolver) — not an absent
		// credential, so surface it directly rather than as "connect your Jira".
		if r.oauth == nil {
			return nil, fmt.Errorf("resolve jira user credential for org %s user %s: cloud oauth minting not configured", orgID, userID)
		}
		cloudID, accessToken, err := r.oauth.AccessTokenForUser(ctx, orgID, userID, host)
		if err != nil {
			return nil, fmt.Errorf("mint jira oauth access token for org %s user %s: %w", orgID, userID, err)
		}
		return NewClient(CloudOAuth(cloudID, accessToken)), nil
	default:
		// An unknown/empty method is corruption or a forward-version credential
		// (e.g. a future OAuth method this build predates). Surface it rather than
		// silently misrouting to a DC client — it is NOT ErrNoJiraUserCredential
		// (the credential exists, it's just unreadable), so handlers don't tell
		// the user to "connect Jira" when re-binding wouldn't be the fix.
		return nil, fmt.Errorf("resolve jira user credential for org %s user %s: unknown credential method %q", orgID, userID, cred.Method)
	}
}
