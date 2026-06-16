// Package jiraoauth mints short-lived Atlassian Cloud OAuth 3LO access tokens
// for the per-user "Connect Jira" flow, mirroring internal/githubapp's
// minter+cache split.
//
// # The flow
//
// A user authorizes TF's Atlassian OAuth app (3LO consent) and TF receives an
// authorization code; ExchangeCode trades it for an {access_token,
// refresh_token, expires_in} set. The access token is short-lived (~1h) and
// talks to the api.atlassian.com/ex/jira/{cloud_id} gateway; the REFRESH token
// is the durable per-user credential TF stores.
//
// # The rotation wrinkle
//
// Atlassian rotates the refresh token on EVERY refresh: each call to the token
// endpoint with grant_type=refresh_token returns a brand-new refresh token and
// invalidates the one just used. So a caller that refreshes must persist the
// new refresh token back, or the next refresh fails with an invalid_grant. The
// stateless Minter here just performs the HTTP exchanges and returns the rotated
// token; the write-back (and access-token caching) lives in TokenCache, which
// is the layer the resolver consumes.
//
// # cloud_id
//
// An access token isn't scoped to a site by itself — AccessibleResources lists
// the Atlassian sites the token can reach, each with its opaque `id` (the
// cloud_id) and `url`. The Connect callback matches the org's configured site
// host against that list to pin the single cloud_id v1 supports.
package jiraoauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/jira"
)

// Atlassian OAuth 3LO endpoints (identity platform). These are global —
// unlike the per-site REST host, the authorize/token/accessible-resources
// endpoints are the same for every Cloud tenant.
const (
	authorizeEndpoint           = "https://auth.atlassian.com/authorize"
	tokenEndpoint               = "https://auth.atlassian.com/oauth/token"
	accessibleResourcesEndpoint = "https://api.atlassian.com/oauth/token/accessible-resources"
)

// ConnectScopes is the space-delimited 3LO scope set the Connect flow requests.
// read/write:jira-work cover the board-claim ticket actions the per-user client
// performs; read:me derives the bound account; offline_access is REQUIRED to be
// issued a refresh token at all (without it Atlassian returns only an access
// token, and there's nothing durable to store).
const ConnectScopes = "read:jira-work write:jira-work read:me offline_access"

// ErrTokenEndpoint wraps a logical failure reported by the Atlassian token
// endpoint (e.g. invalid_grant on an exhausted/rotated refresh token), as
// distinct from a transport error. Callers can errors.Is it to tell "the
// stored refresh token is dead, re-Connect" apart from "the network blipped".
var ErrTokenEndpoint = errors.New("jiraoauth: token endpoint error")

// Token is one minted access token, the rotated refresh token returned
// alongside it, and the access token's expiry (computed from expires_in).
type Token struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

// Resource is one Atlassian site an access token can reach, from the
// accessible-resources lookup. ID is the cloud_id; URL is the site origin
// (e.g. "https://acme.atlassian.net") the Connect callback matches the org's
// configured host against.
type Resource struct {
	ID   string `json:"id"`
	URL  string `json:"url"`
	Name string `json:"name"`
}

// Minter performs the stateless Atlassian OAuth HTTP exchanges. Safe for
// concurrent use; it holds no per-flow state (the rotation write-back +
// caching live in TokenCache).
type Minter struct {
	httpClient *http.Client
	// tokenURL / resourcesURL default to the Atlassian endpoints; tests
	// override them to point at an httptest server. Left at the constants in
	// production.
	tokenURL     string
	resourcesURL string
	// now is injectable for tests; production leaves it nil and uses time.Now.
	// The expiry on a minted Token is computed off it.
	now func() time.Time
}

// NewMinter returns a Minter with a 30s-timeout HTTP client.
func NewMinter() *Minter {
	return &Minter{
		httpClient:   &http.Client{Timeout: 30 * time.Second},
		tokenURL:     tokenEndpoint,
		resourcesURL: accessibleResourcesEndpoint,
	}
}

func (m *Minter) timeNow() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// AuthorizeURL builds the consent redirect the Connect start handler sends the
// browser to. audience pins the api.atlassian.com gateway, prompt=consent
// forces the refresh-token-issuing consent screen, and state carries the
// caller's CSRF nonce. The scope set is ConnectScopes (offline_access included
// so a refresh token is issued).
func AuthorizeURL(app jira.OAuthApp, redirectURI, state string) string {
	q := url.Values{}
	q.Set("audience", "api.atlassian.com")
	q.Set("client_id", app.ClientID)
	q.Set("scope", ConnectScopes)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	q.Set("response_type", "code")
	q.Set("prompt", "consent")
	return authorizeEndpoint + "?" + q.Encode()
}

// ExchangeCode trades an authorization code for the first token set
// (grant_type=authorization_code). redirectURI must match the one the authorize
// redirect used.
func (m *Minter) ExchangeCode(ctx context.Context, app jira.OAuthApp, code, redirectURI string) (Token, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", app.ClientID)
	form.Set("client_secret", app.ClientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	return m.requestToken(ctx, form)
}

// Refresh trades a refresh token for a new token set
// (grant_type=refresh_token). The returned Token.RefreshToken is the ROTATED
// token — Atlassian invalidates the one passed in, so the caller MUST persist
// the new one. An invalid_grant from a dead/already-rotated token surfaces
// wrapped in ErrTokenEndpoint.
func (m *Minter) Refresh(ctx context.Context, app jira.OAuthApp, refreshToken string) (Token, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", app.ClientID)
	form.Set("client_secret", app.ClientSecret)
	form.Set("refresh_token", refreshToken)
	return m.requestToken(ctx, form)
}

// tokenResponse is the subset of the Atlassian token endpoint response we use.
// On a logical failure the endpoint returns a non-2xx with an `error` /
// `error_description` body instead.
type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int    `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// requestToken POSTs the form to the token endpoint and parses the result.
// A transport failure returns a plain wrapped error; a non-2xx or an `error`
// field wraps ErrTokenEndpoint so the rotation-dead case is distinguishable.
func (m *Minter) requestToken(ctx context.Context, form url.Values) (Token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, fmt.Errorf("jiraoauth: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("jiraoauth: token request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var parsed tokenResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Token{}, fmt.Errorf("jiraoauth: parse token response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK || parsed.Error != "" {
		detail := parsed.Error
		if parsed.ErrorDescription != "" {
			detail = parsed.Error + ": " + parsed.ErrorDescription
		}
		if detail == "" {
			detail = fmt.Sprintf("status %d", resp.StatusCode)
		}
		return Token{}, fmt.Errorf("%w: %s", ErrTokenEndpoint, detail)
	}
	if parsed.AccessToken == "" {
		return Token{}, fmt.Errorf("%w: response missing access_token", ErrTokenEndpoint)
	}
	if parsed.RefreshToken == "" {
		// offline_access should always yield a refresh token; its absence means
		// the app wasn't granted the scope, and storing an envelope with no
		// refresh token would brick the next mint. Fail loudly.
		return Token{}, fmt.Errorf("%w: response missing refresh_token (offline_access scope required)", ErrTokenEndpoint)
	}
	return Token{
		AccessToken:  parsed.AccessToken,
		RefreshToken: parsed.RefreshToken,
		ExpiresAt:    m.timeNow().Add(time.Duration(parsed.ExpiresIn) * time.Second),
	}, nil
}

// AccessibleResources lists the Atlassian sites the access token can reach
// (GET .../accessible-resources, Bearer). The Connect callback matches the
// org's configured site host against these to resolve the cloud_id.
func (m *Minter) AccessibleResources(ctx context.Context, accessToken string) ([]Resource, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.resourcesURL, nil)
	if err != nil {
		return nil, fmt.Errorf("jiraoauth: build accessible-resources request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jiraoauth: accessible-resources request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jiraoauth: accessible-resources status %d: %s", resp.StatusCode, truncate(string(body), 256))
	}
	var resources []Resource
	if err := json.Unmarshal(body, &resources); err != nil {
		return nil, fmt.Errorf("jiraoauth: parse accessible-resources: %w", err)
	}
	return resources, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
