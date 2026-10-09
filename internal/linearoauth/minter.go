// Package linearoauth performs Linear's OAuth exchanges and keeps the custody
// of the tokens they mint, the Linear sibling of internal/jiraoauth.
//
// # The install
//
// A workspace admin installs the org's resolved Linear OAuth app with
// actor=app: TF then acts in the workspace as the app user the install
// creates, not as any human. The authorization code exchanges for a 24-hour
// access token and a refresh token; the refresh token is the durable
// credential, stored in the org secret linear_app_install.
//
// # Rotation
//
// Linear rotates the refresh token on every refresh, so the caller persists
// the new one. Replaying the previous refresh token within Linear's 30-minute
// grace returns the identical new pair, so two processes refreshing the same
// install at once converge on one pair and either one's write-back is
// correct. The Minter here only performs the HTTP exchanges; the write-back
// and the access-token cache live in TokenCache.
package linearoauth

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

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// Linear's OAuth endpoints.
const (
	authorizeEndpoint = "https://linear.app/oauth/authorize"
	tokenEndpoint     = "https://api.linear.app/oauth/token"
	revokeEndpoint    = "https://api.linear.app/oauth/revoke"
)

// InstallScopes is the comma-separated scope set the install requests. admin
// is refused alongside actor=app, and the app:assignable / app:mentionable
// scopes belong to Linear's agent sessions, which TF does not use.
const InstallScopes = "read,write,issues:create,comments:create"

// ErrTokenEndpoint wraps a refusal from Linear's token endpoint, such as
// invalid_grant on a refresh token the workspace has revoked, as distinct from
// an endpoint that was unavailable or could not be reached.
var ErrTokenEndpoint = errors.New("linearoauth: token endpoint error")

// The operations a StatusError names.
const (
	opToken  = "token request"
	opRevoke = "revoke request"
)

// StatusError is a failing answer from one of Linear's OAuth endpoints: a
// non-200 status, or an error member on a 200.
type StatusError struct {
	Op         string
	StatusCode int
	// Code is the endpoint's OAuth error code (invalid_grant), empty when the
	// answer carried none.
	Code        string
	BodyExcerpt string
	Class       upstream.Class
}

func (e *StatusError) Error() string {
	detail := e.BodyExcerpt
	if e.Code != "" && !strings.Contains(detail, e.Code) {
		detail = e.Code + ": " + detail
	}
	return fmt.Sprintf("linearoauth: %s: status %d: %s", e.Op, e.StatusCode, detail)
}

// UpstreamClass implements upstream.Classified.
func (e *StatusError) UpstreamClass() upstream.Class { return e.Class }

// Unwrap makes a refusal from the token endpoint an ErrTokenEndpoint. An
// unavailable or rate-limiting endpoint said nothing about the token.
func (e *StatusError) Unwrap() error {
	if e.Op == opToken && e.Class != upstream.Transient && e.Class != upstream.RateLimited {
		return ErrTokenEndpoint
	}
	return nil
}

// RefusedGrant reports whether err is the token endpoint refusing the grant
// itself (invalid_grant): for a refresh, the install was revoked in Linear.
func RefusedGrant(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Op == opToken && se.Code == "invalid_grant"
}

func newStatusError(op string, resp *http.Response, body []byte) *StatusError {
	var oauth struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &oauth)
	return &StatusError{
		Op:          op,
		StatusCode:  resp.StatusCode,
		Code:        oauthErrorCode(oauth.Error),
		BodyExcerpt: upstream.Excerpt(body),
		Class:       upstream.ClassifyResponse(resp.StatusCode, resp.Header, body),
	}
}

// oauthErrorCode is s when it reads as an OAuth error code (RFC 6749: a short
// run of printable ASCII), and "" otherwise.
func oauthErrorCode(s string) string {
	if len(s) > 64 {
		return ""
	}
	for _, r := range s {
		if r < 0x21 || r > 0x7e || r == '"' || r == '\\' {
			return ""
		}
	}
	return s
}

// transportError marks err, a request that failed in transit, as an upstream
// outcome unless the caller abandoned it.
func transportError(ctx context.Context, err error) error {
	if _, counted := upstream.ClassifyTransport(ctx, err); counted {
		return &upstream.TransportError{Err: err}
	}
	return err
}

// Token is one minted access token, the refresh token issued with it, and the
// access token's expiry.
type Token struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

// TokenTypeHint names which kind of token a Revoke is handed.
type TokenTypeHint string

const (
	HintAccessToken  TokenTypeHint = "access_token"
	HintRefreshToken TokenTypeHint = "refresh_token"
)

// Minter performs Linear's stateless OAuth exchanges. Safe for concurrent use.
type Minter struct {
	httpClient *http.Client
	// tokenURL and revokeURL default to Linear's endpoints; tests point them
	// at an httptest server.
	tokenURL  string
	revokeURL string
	// now is injectable for tests; nil is time.Now.
	now func() time.Time
}

// NewMinter returns a Minter with a 30s-timeout HTTP client.
func NewMinter() *Minter {
	return &Minter{
		httpClient: telemetry.TracedHTTPClient(30*time.Second, "linear"),
		tokenURL:   tokenEndpoint,
		revokeURL:  revokeEndpoint,
	}
}

// NewMinterWithEndpoints is NewMinter against other token and revoke URLs,
// for tests outside this package that stand up a fake Linear.
func NewMinterWithEndpoints(tokenURL, revokeURL string) *Minter {
	m := NewMinter()
	m.tokenURL = tokenURL
	m.revokeURL = revokeURL
	return m
}

func (m *Minter) timeNow() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// InstallAuthorizeURL is the consent page the install ceremony sends a
// workspace admin to: actor=app, so the grant is to the app user rather than
// the admin, and prompt=consent so the scopes are shown every time. state
// carries the ceremony's CSRF nonce.
func InstallAuthorizeURL(app linear.OAuthApp, redirectURI, state string) string {
	q := url.Values{}
	q.Set("client_id", app.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	q.Set("scope", InstallScopes)
	q.Set("actor", "app")
	q.Set("state", state)
	q.Set("prompt", "consent")
	return authorizeEndpoint + "?" + q.Encode()
}

// ExchangeCode trades an authorization code for the first token pair.
// redirectURI must be the one the authorize redirect named.
func (m *Minter) ExchangeCode(ctx context.Context, app linear.OAuthApp, code, redirectURI string) (Token, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", app.ClientID)
	form.Set("client_secret", app.ClientSecret)
	return m.requestToken(ctx, form)
}

// Refresh trades a refresh token for a new pair. The returned RefreshToken is
// the rotated one, which the caller persists. A revoked install answers
// invalid_grant (RefusedGrant).
func (m *Minter) Refresh(ctx context.Context, app linear.OAuthApp, refreshToken string) (Token, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", app.ClientID)
	form.Set("client_secret", app.ClientSecret)
	return m.requestToken(ctx, form)
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
}

func (m *Minter) requestToken(ctx context.Context, form url.Values) (_ Token, err error) {
	// grant_type separates a first exchange from a rotation; every other form
	// field is a credential and stays off the span.
	ctx, span := tracer.Start(ctx, "linearoauth.token_request",
		trace.WithAttributes(telemetry.Disposition(form.Get("grant_type"))))
	defer span.End()
	defer func() {
		if err != nil {
			span.SetStatus(codes.Error, "token request failed")
		}
	}()

	resp, body, err := m.post(ctx, opToken, m.tokenURL, form)
	if err != nil {
		return Token{}, err
	}
	var parsed tokenResponse
	if json.Unmarshal(body, &parsed) != nil {
		return Token{}, fmt.Errorf("linearoauth: parse token response: %s", upstream.Excerpt(body))
	}
	if parsed.Error != "" {
		se := newStatusError(opToken, resp, body)
		se.Class = upstream.Rejected
		return Token{}, se
	}
	if parsed.AccessToken == "" {
		return Token{}, fmt.Errorf("%w: response missing access_token", ErrTokenEndpoint)
	}
	if parsed.RefreshToken == "" {
		// An install without a refresh token dies in 24 hours with nothing
		// to renew it, so it is not stored at all.
		return Token{}, fmt.Errorf("%w: response missing refresh_token", ErrTokenEndpoint)
	}
	return Token{
		AccessToken:  parsed.AccessToken,
		RefreshToken: parsed.RefreshToken,
		ExpiresAt:    m.timeNow().Add(time.Duration(parsed.ExpiresIn) * time.Second),
	}, nil
}

// Revoke asks Linear to revoke token. Revoking an install's refresh token ends
// the install's grant; the app user stays in the workspace's member list until
// an admin removes the app in Linear.
func (m *Minter) Revoke(ctx context.Context, app linear.OAuthApp, token string, hint TokenTypeHint) error {
	form := url.Values{}
	form.Set("token", token)
	form.Set("token_type_hint", string(hint))
	form.Set("client_id", app.ClientID)
	form.Set("client_secret", app.ClientSecret)
	_, _, err := m.post(ctx, opRevoke, m.revokeURL, form)
	return err
}

// post sends form to endpoint and returns a 200's body. Any other status is a
// *StatusError naming op; a transport failure is an *upstream.TransportError.
func (m *Minter) post(ctx context.Context, op, endpoint string, form url.Values) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, nil, fmt.Errorf("linearoauth: build %s: %w", op, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, nil, transportError(ctx, fmt.Errorf("linearoauth: %s: %w", op, err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, rerr := upstream.ReadErrorBody(resp.Body)
		if rerr != nil {
			return nil, nil, transportError(ctx, fmt.Errorf("linearoauth: read %s response: %w", op, rerr))
		}
		return nil, nil, newStatusError(op, resp, body)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, nil, transportError(ctx, fmt.Errorf("linearoauth: read %s response: %w", op, err))
	}
	return resp, body, nil
}
