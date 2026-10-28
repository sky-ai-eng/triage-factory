package linear

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

var (
	// ErrNoLinearSystemCredential is ForSystem finding no usable org service
	// credential. The poller skips the org; an exec verb reports Linear as not
	// configured.
	ErrNoLinearSystemCredential = errors.New("linear: no system credential for org")

	// ErrNoLinearUserCredential is ForUser finding no usable credential of the
	// acting user's own. ForUser never falls back to the org's service
	// credential in its place: a user-initiated write made as the org's
	// identity would be attributed to the wrong actor.
	ErrNoLinearUserCredential = errors.New("linear: no user credential for org/user")

	// ErrLinearUserCredentialRefused is ErrNoLinearUserCredential for a
	// Connect credential Linear refused outright: it answered a refresh with
	// invalid_grant, so the user revoked Triage Factory's access in Linear or
	// the grant expired, and the stored credential was replaced with a refused
	// marker. A caller that asks only whether a usable credential exists gets
	// the answer it already handles; one that can say why the user has to
	// connect again tells them so.
	ErrLinearUserCredentialRefused = fmt.Errorf("%w: linear refused the stored credential", ErrNoLinearUserCredential)

	// ErrLinearUserCredentialAppRemoved is ErrNoLinearUserCredential for a
	// Connect credential whose issuing app the org can no longer authenticate
	// as: an admin removed or replaced the org's Linear OAuth app, or the
	// operator replaced the deployment app. Linear accepts a refresh token only
	// with the secret of the app that minted it, so the credential cannot be
	// refreshed until that app is configured again. It is kept rather than
	// removed, so restoring the same app (the same client ID) brings it back.
	ErrLinearUserCredentialAppRemoved = fmt.Errorf("%w: the oauth app that issued the credential is no longer configured", ErrNoLinearUserCredential)
)

// The org-level Linear secret keys this package reads. They are copies of
// integrations.KeyLinear*, kept here because internal/integrations imports
// this package (through internal/auth's ValidateLinear), so importing it back
// would be a cycle. keys_drift_test pins the agreement.
const (
	keyLinearAPIKey     = "linear_api_key"
	keyLinearAuthMethod = "linear_auth_method"
)

// UserTokenKey is the per-user secret key a user's Linear credential is
// stored under, scoped to the Linear workspace it belongs to.
func UserTokenKey(workspaceID string) string {
	return "linear_token/" + workspaceID
}

// UserCredential is the envelope stored under UserTokenKey. Method is
// AuthMethodAPIKey, where Token is the user's personal key, or
// AuthMethodConnectOAuth, where RefreshToken is the durable credential and
// access tokens are minted from it on demand and never stored. ClientID is
// the OAuth app that minted the refresh token, whose secret every refresh
// needs. GrantID names the Connect that created the credential: every
// rotation keeps it and connecting again replaces it, so a cached access token
// is served only while the stored credential is still the grant it was minted
// for.
//
// Refused marks the record left in place of a Connect credential Linear
// refused (RefusedUserCredential). It carries no token, so it is never usable;
// it exists so the user can be told why they have to connect again.
type UserCredential struct {
	Method       AuthMethod `json:"method"`
	Token        string     `json:"token,omitempty"`
	RefreshToken string     `json:"refresh_token,omitempty"`
	ClientID     string     `json:"client_id,omitempty"`
	GrantID      string     `json:"grant_id,omitempty"`
	Refused      bool       `json:"refused,omitempty"`
}

// RefusedUserCredential is the marker stored in place of a Connect credential
// Linear refused.
func RefusedUserCredential() UserCredential {
	return UserCredential{Method: AuthMethodConnectOAuth, Refused: true}
}

// Usable reports whether c carries what its method needs: a key for api_key;
// a refresh token, the app that minted it and its grant for connect_oauth. A
// method this build does not know is not usable.
func (c UserCredential) Usable() bool {
	switch c.Method {
	case AuthMethodAPIKey:
		return c.Token != ""
	case AuthMethodConnectOAuth:
		return c.RefreshToken != "" && c.ClientID != "" && c.GrantID != ""
	default:
		return false
	}
}

// MarshalUserCredential renders a UserCredential for storage.
func MarshalUserCredential(c UserCredential) (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("linear: marshal user credential: %w", err)
	}
	return string(b), nil
}

// ParseUserCredential reads a stored UserCredential. An empty or malformed
// value is an error: the key exists, so this is corruption, not absence.
func ParseUserCredential(raw string) (UserCredential, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return UserCredential{}, errors.New("linear: empty user credential")
	}
	var c UserCredential
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return UserCredential{}, fmt.Errorf("linear: parse user credential: %w", err)
	}
	return c, nil
}

// InstallCredential is the envelope an app install stores under the org secret
// linear_app_install: the refresh token and what identifies the install it
// belongs to. ClientID is the app that minted it, whose secret every refresh
// needs. InstallID is the install's org_linear_installs.install_id: a rotation
// keeps it and a new install changes it, so a cached access token learns the
// install it was minted for has been replaced, and a removal names the one
// install it read.
//
// AccessToken and AccessTokenExpiresUnix are the access token the last refresh
// (or the install's code exchange) minted and its Unix-second expiry. They are
// stored so that every process reading the envelope can use the token until it
// expires instead of refreshing for itself: each refresh rotates the refresh
// token, and a process that starts cold, such as an exec verb run from the
// host CLI, would otherwise refresh on every call. Empty in an envelope
// written before they were stored, which the next refresh fills in.
type InstallCredential struct {
	InstallID              string `json:"install_id"`
	WorkspaceID            string `json:"workspace_id"`
	AppUserID              string `json:"app_user_id"`
	RefreshToken           string `json:"refresh_token"`
	ClientID               string `json:"client_id"`
	AccessToken            string `json:"access_token,omitempty"`
	AccessTokenExpiresUnix int64  `json:"access_token_expires_unix,omitempty"`
}

// MarshalInstallCredential renders an InstallCredential for storage.
func MarshalInstallCredential(c InstallCredential) (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("linear: marshal install credential: %w", err)
	}
	return string(b), nil
}

// ParseInstallCredential reads a stored InstallCredential. An empty or
// malformed value, or one missing its install id, refresh token or client id,
// is an error: the key exists, so this is corruption, not absence.
func ParseInstallCredential(raw string) (InstallCredential, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return InstallCredential{}, errors.New("linear: empty install credential")
	}
	var c InstallCredential
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return InstallCredential{}, fmt.Errorf("linear: parse install credential: %w", err)
	}
	if c.InstallID == "" || c.RefreshToken == "" || c.ClientID == "" {
		return InstallCredential{}, errors.New("linear: install credential is missing its install id, refresh token or client id")
	}
	return c, nil
}

// InstallTokenSource mints the app user's access token from an org's install
// envelope. internal/linearoauth's TokenCache implements it. An org with no
// usable install is ErrNoLinearSystemCredential.
type InstallTokenSource interface {
	AccessTokenForOrg(ctx context.Context, orgID string) (accessToken string, expiresAt time.Time, err error)
}

// UserTokenSource mints the access token of a user's Connect credential for a
// workspace. internal/linearoauth's UserTokenCache implements it. A user with
// no usable Connect credential is ErrNoLinearUserCredential, and so is one the
// source cannot mint a token for because it kept changing during the refresh:
// connecting again fixes both. Two causes are named by their own sentinels,
// which wrap it: ErrLinearUserCredentialRefused for a credential Linear
// refused, and ErrLinearUserCredentialAppRemoved for one minted by an app the
// org can no longer authenticate as.
type UserTokenSource interface {
	AccessTokenForUser(ctx context.Context, orgID, userID, workspaceID string) (string, error)
}

// SystemCredential is the org's service credential as raw fields rather than
// a live client, for sealing into a run's credential bundle. Method decides
// which field is set: APIKey for AuthMethodAPIKey, AccessToken and ExpiresAt
// for AuthMethodAppInstall.
type SystemCredential struct {
	Method      AuthMethod
	APIKey      string
	AccessToken string
	ExpiresAt   time.Time
}

// Config is the client configuration the credential stands for.
func (s SystemCredential) Config() Config {
	if s.Method == AuthMethodAppInstall {
		return Bearer(s.AccessToken)
	}
	return APIKey(s.APIKey)
}

// Resolver produces an authenticated *Client by provenance:
//
//   - ForSystem is the org's service credential: polling and agent verbs,
//     attributed to the org's Linear identity by design.
//   - ForUser is the acting user's own credential, for writes a person makes.
//     It is required and never falls back to the org's.
//
// Every store read goes through the ...System doors: resolving a credential is
// a system operation, and the ids it is handed are already authorized by the
// request middleware or are a background job's own. Every resolution first
// passes the resolver's SourceGate, so no client is made for an org whose
// admin turned Linear off.
type Resolver interface {
	ForSystem(ctx context.Context, orgID string) (*Client, error)
	ForUser(ctx context.Context, orgID, userID string) (*Client, error)
	ResolveSystemCredential(ctx context.Context, orgID string) (SystemCredential, error)
}

// SourceGate is the check a Resolver runs before every resolution — ForSystem,
// ForUser and ResolveSystemCredential alike — and before it reads any secret:
// whether orgID may use Linear at all. nil lets the resolution proceed; any
// error refuses it and is returned unchanged. The gate the product wires is
// eventsource.ResolverGate, which refuses a source an org admin turned off
// with an error wrapping eventsource.ErrDisabled (a sentinel this package
// cannot name, since eventsource imports it), and refuses a policy it cannot
// read with the read's own error.
//
// It is required, and it lives here rather than at the callers, because every
// Linear client TF makes comes out of a Resolver: a caller that forgot the
// check would be a hole in the off switch that nothing else closes.
type SourceGate func(ctx context.Context, orgID string) error

type resolver struct {
	secrets db.SecretStore
	orgs    db.OrgsStore
	gate    SourceGate
	install InstallTokenSource // nil when the app-install shape isn't wired
	user    UserTokenSource    // nil when the connect_oauth shape isn't wired
}

// NewResolver builds a Resolver over the secret and org-settings stores. It
// resolves the api_key shape only; an installed org is an error. Callers that
// serve installed orgs use NewResolverWithInstall. gate is required
// (SourceGate).
func NewResolver(secrets db.SecretStore, orgs db.OrgsStore, gate SourceGate) Resolver {
	return newResolver(secrets, orgs, gate, nil, nil)
}

// NewResolverWithInstall builds a Resolver that also resolves the app_install
// shape, minting the app user's access token through install. ForUser answers
// a user's Connect credential with an error; callers that serve per-user
// writes use NewResolverWithOAuth.
func NewResolverWithInstall(secrets db.SecretStore, orgs db.OrgsStore, gate SourceGate, install InstallTokenSource) Resolver {
	return newResolver(secrets, orgs, gate, install, nil)
}

// NewResolverWithOAuth builds a Resolver that resolves every shape: the
// app_install org credential through install, and a user's Connect
// credential through user.
func NewResolverWithOAuth(secrets db.SecretStore, orgs db.OrgsStore, gate SourceGate, install InstallTokenSource, user UserTokenSource) Resolver {
	return newResolver(secrets, orgs, gate, install, user)
}

func newResolver(secrets db.SecretStore, orgs db.OrgsStore, gate SourceGate, install InstallTokenSource, user UserTokenSource) *resolver {
	if gate == nil {
		panic("linear: a Resolver requires a SourceGate")
	}
	return &resolver{secrets: secrets, orgs: orgs, gate: gate, install: install, user: user}
}

// ForSystem builds a client from the org's service credential, counted under
// orgID, whose rate-limit readings are recorded under the credential's auth
// method (Client.WithCredential). It is ErrNoLinearSystemCredential when the
// org has none; a failed secret read is returned as itself, so a store outage
// is never reported as Linear being unconfigured.
func (r *resolver) ForSystem(ctx context.Context, orgID string) (*Client, error) {
	cred, err := r.ResolveSystemCredential(ctx, orgID)
	if err != nil {
		return nil, err
	}
	return NewClient(cred.Config()).WithOrg(orgID).WithCredential(string(cred.Method)), nil
}

// ResolveSystemCredential reads the org's service credential, dispatched on
// the stored auth-method marker. An absent marker reads as api_key: the marker
// only tells the two shapes apart, and a key under linear_api_key can only be
// the api_key shape, since an app install stores an envelope under its own key.
// No key under api_key is ErrNoLinearSystemCredential; a
// marker this build does not know is an error of its own, because rebinding is
// not what fixes it.
func (r *resolver) ResolveSystemCredential(ctx context.Context, orgID string) (SystemCredential, error) {
	if err := r.gate(ctx, orgID); err != nil {
		return SystemCredential{}, err
	}
	method, err := r.secrets.GetSystem(ctx, orgID, keyLinearAuthMethod)
	if err != nil {
		return SystemCredential{}, fmt.Errorf("resolve linear auth method for org %s: %w", orgID, err)
	}
	switch AuthMethod(method) {
	case AuthMethodAPIKey, "":
		key, err := r.secrets.GetSystem(ctx, orgID, keyLinearAPIKey)
		if err != nil {
			return SystemCredential{}, fmt.Errorf("resolve linear api key for org %s: %w", orgID, err)
		}
		if key == "" {
			return SystemCredential{}, fmt.Errorf("%w: org=%s", ErrNoLinearSystemCredential, orgID)
		}
		return SystemCredential{Method: AuthMethodAPIKey, APIKey: key}, nil
	case AuthMethodAppInstall:
		if r.install == nil {
			return SystemCredential{}, fmt.Errorf("resolve linear credential for org %s: app_install with no install token source wired", orgID)
		}
		token, expiresAt, err := r.install.AccessTokenForOrg(ctx, orgID)
		if err != nil {
			return SystemCredential{}, err
		}
		return SystemCredential{Method: AuthMethodAppInstall, AccessToken: token, ExpiresAt: expiresAt}, nil
	default:
		return SystemCredential{}, fmt.Errorf("resolve linear credential for org %s: unknown auth method %q", orgID, method)
	}
}

// ForUser builds a client from the acting user's own credential for the org's
// Linear workspace, counted under orgID. The client names no credential, so
// its rate-limit readings are not recorded: Linear meters each user's
// credential separately, and the only id that tells them apart is the user.
// With no workspace bound, no stored credential, or a Connect credential its
// UserTokenSource reports as unusable (Linear refused it, say) it is
// ErrNoLinearUserCredential, never a client built from the org's credential
// instead. A failed read, a corrupt envelope or a method this build does not
// know is returned as an error of its own.
func (r *resolver) ForUser(ctx context.Context, orgID, userID string) (*Client, error) {
	if err := r.gate(ctx, orgID); err != nil {
		return nil, err
	}
	orgSet, err := r.orgs.GetSettingsSystem(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("resolve linear workspace for org %s: %w", orgID, err)
	}
	workspaceID := strings.TrimSpace(orgSet.LinearWorkspaceID)
	if workspaceID == "" {
		return nil, fmt.Errorf("%w: org=%s user=%s (org has no linear workspace)", ErrNoLinearUserCredential, orgID, userID)
	}
	raw, err := r.secrets.GetUserSystem(ctx, orgID, userID, UserTokenKey(workspaceID))
	if err != nil {
		return nil, fmt.Errorf("resolve linear user credential for org %s user %s: %w", orgID, userID, err)
	}
	if raw == "" {
		return nil, fmt.Errorf("%w: org=%s user=%s workspace=%s", ErrNoLinearUserCredential, orgID, userID, workspaceID)
	}
	cred, err := ParseUserCredential(raw)
	if err != nil {
		return nil, fmt.Errorf("resolve linear user credential for org %s user %s: %w", orgID, userID, err)
	}
	switch cred.Method {
	case AuthMethodAPIKey:
		if cred.Token == "" {
			return nil, fmt.Errorf("%w: org=%s user=%s workspace=%s (empty api key)", ErrNoLinearUserCredential, orgID, userID, workspaceID)
		}
		return NewClient(APIKey(cred.Token)).WithOrg(orgID), nil
	case AuthMethodConnectOAuth:
		if r.user == nil {
			return nil, fmt.Errorf("resolve linear user credential for org %s user %s: connect_oauth with no user token source wired", orgID, userID)
		}
		token, err := r.user.AccessTokenForUser(ctx, orgID, userID, workspaceID)
		if err != nil {
			return nil, err
		}
		return NewClient(Bearer(token)).WithOrg(orgID), nil
	default:
		return nil, fmt.Errorf("resolve linear user credential for org %s user %s: unknown credential method %q", orgID, userID, cred.Method)
	}
}
