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
// Cloud OAuth (the one-click Connect path) is a later ticket; it extends this
// envelope with a third method without touching the two cases here.
type UserCredential struct {
	Method AuthMethod `json:"method"`
	// Email is the Atlassian account email; set only for AuthMethodCloudAPIToken
	// (the Basic-auth pair), empty for a DC PAT.
	Email string `json:"email,omitempty"`
	Token string `json:"token"`
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

type resolver struct {
	secrets db.SecretStore
	orgs    db.OrgsStore
}

// NewResolver builds a Resolver from the secret + org-settings stores.
func NewResolver(secrets db.SecretStore, orgs db.OrgsStore) Resolver {
	return &resolver{secrets: secrets, orgs: orgs}
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

// ForUser resolves the acting user's own Jira credential into an authenticated
// client, keyed under and talking to the org's Jira host. The stored secret is
// a UserCredential envelope whose method marker selects the scheme: a Cloud API
// token yields a Basic / REST v3 client, a Data Center PAT a Bearer / REST v2
// one — the per-user mirror of ForSystem's marker dispatch. A bare token from
// the pre-envelope DC bind is read back as a dc_pat (ParseUserCredential).
//
// REQUIRED — an absent credential is ErrNoJiraUserCredential, NEVER a fall-back
// to the org service cred (that would mis-attribute the user's write to the
// bot). A backend read error propagates rather than being misreported as "not
// connected"; a corrupt envelope likewise propagates rather than silently
// degrading to the bot.
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
	switch cred.Method {
	case AuthMethodCloudAPIToken:
		if cred.Email == "" || cred.Token == "" {
			return nil, fmt.Errorf("%w: org=%s user=%s host=%s (incomplete cloud credential)", ErrNoJiraUserCredential, orgID, userID, host)
		}
		return NewClient(CloudAPIToken(host, cred.Email, cred.Token)), nil
	default:
		// dc_pat — and the back-compat bare token, which ParseUserCredential
		// already normalized to AuthMethodDCPAT.
		if cred.Token == "" {
			return nil, fmt.Errorf("%w: org=%s user=%s host=%s", ErrNoJiraUserCredential, orgID, userID, host)
		}
		return NewClient(DataCenterPAT(host, cred.Token)), nil
	}
}
