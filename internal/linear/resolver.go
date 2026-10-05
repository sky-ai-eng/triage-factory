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
// AuthMethodAPIKey (Token is the user's personal key) or
// AuthMethodConnectOAuth (Token is an access token, RefreshToken renews it).
type UserCredential struct {
	Method       AuthMethod `json:"method"`
	Token        string     `json:"token,omitempty"`
	RefreshToken string     `json:"refresh_token,omitempty"`
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
// request middleware or are a background job's own.
type Resolver interface {
	ForSystem(ctx context.Context, orgID string) (*Client, error)
	ForUser(ctx context.Context, orgID, userID string) (*Client, error)
	ResolveSystemCredential(ctx context.Context, orgID string) (SystemCredential, error)
}

type resolver struct {
	secrets db.SecretStore
	orgs    db.OrgsStore
}

// NewResolver builds a Resolver over the secret and org-settings stores.
func NewResolver(secrets db.SecretStore, orgs db.OrgsStore) Resolver {
	return &resolver{secrets: secrets, orgs: orgs}
}

// ForSystem builds a client from the org's service credential, counted under
// orgID. It is ErrNoLinearSystemCredential when the org has none; a failed
// secret read is returned as itself, so a store outage is never reported as
// Linear being unconfigured.
func (r *resolver) ForSystem(ctx context.Context, orgID string) (*Client, error) {
	cred, err := r.ResolveSystemCredential(ctx, orgID)
	if err != nil {
		return nil, err
	}
	return NewClient(cred.Config()).WithOrg(orgID), nil
}

// ResolveSystemCredential reads the org's service credential, dispatched on
// the stored auth-method marker. An absent marker, or an api_key marker with
// no key, is ErrNoLinearSystemCredential; a marker this build does not know is
// an error of its own, because rebinding is not what fixes it.
func (r *resolver) ResolveSystemCredential(ctx context.Context, orgID string) (SystemCredential, error) {
	method, err := r.secrets.GetSystem(ctx, orgID, keyLinearAuthMethod)
	if err != nil {
		return SystemCredential{}, fmt.Errorf("resolve linear auth method for org %s: %w", orgID, err)
	}
	switch AuthMethod(method) {
	case AuthMethodAPIKey:
		key, err := r.secrets.GetSystem(ctx, orgID, keyLinearAPIKey)
		if err != nil {
			return SystemCredential{}, fmt.Errorf("resolve linear api key for org %s: %w", orgID, err)
		}
		if key == "" {
			return SystemCredential{}, fmt.Errorf("%w: org=%s", ErrNoLinearSystemCredential, orgID)
		}
		return SystemCredential{Method: AuthMethodAPIKey, APIKey: key}, nil
	case AuthMethodAppInstall:
		// TODO(TFAC-1022): mint the app user's access token from the install
		// envelope stored under linear_app_install. Until then an installed org
		// resolves as unconfigured.
		return SystemCredential{}, fmt.Errorf("%w: org=%s (app_install not supported yet)", ErrNoLinearSystemCredential, orgID)
	case "":
		return SystemCredential{}, fmt.Errorf("%w: org=%s", ErrNoLinearSystemCredential, orgID)
	default:
		return SystemCredential{}, fmt.Errorf("resolve linear credential for org %s: unknown auth method %q", orgID, method)
	}
}

// ForUser builds a client from the acting user's own credential for the org's
// Linear workspace, counted under orgID. With no workspace bound or no stored
// credential it is ErrNoLinearUserCredential, never a client built from the
// org's credential instead. A failed read, a corrupt envelope or a method this
// build does not know is returned as an error of its own.
func (r *resolver) ForUser(ctx context.Context, orgID, userID string) (*Client, error) {
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
		// TODO(TFAC-1023): build a Bearer client from the Connect access token,
		// refreshing it when it has expired. Until then a connected user
		// resolves as having no credential.
		return nil, fmt.Errorf("%w: org=%s user=%s (connect_oauth not supported yet)", ErrNoLinearUserCredential, orgID, userID)
	default:
		return nil, fmt.Errorf("resolve linear user credential for org %s user %s: unknown credential method %q", orgID, userID, cred.Method)
	}
}
