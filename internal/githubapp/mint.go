// Package githubapp mints short-lived installation access tokens for a
// GitHub App.
//
// # Why installation tokens
//
// A GitHub App registers once per GitHub org and is then "installed"
// per-repository-set. Each installation has:
//
//   - A numeric installation ID
//   - A set of repositories the app can act on
//   - A permissions scope (configured on the App, narrowable per-mint)
//
// The App's signing key (an RSA private key) sits server-side. To act
// on behalf of an installation, the server signs a short-lived JWT (≤
// 10min, RS256, iss = App ID), POSTs it to
// /app/installations/{id}/access_tokens, and receives an installation
// access token (string starting with "ghs_") with a 1-hour TTL.
//
// That token is what the rest of the system uses to call the REST API,
// GraphQL API, and — crucially for this package — what the git proxy
// injects as Basic-auth credentials on git protocol requests.
//
// # Threat-model fit
//
// Installation tokens are the right credential class for sandboxed
// agent operations because:
//
//   - 1-hour TTL bounds the blast radius of any leak to minutes-of-
//     useful-life rather than the indefinite lifetime of a PAT.
//   - Repo-scoped: a token for org A's installation cannot read org B's
//     repos, so a compromised proxy cannot exfiltrate cross-tenant.
//   - Revocable per-installation if compromised, without touching the
//     user's PATs or other credentials.
//
// The minter holds the private key in memory; the resulting token is
// what gets handed to the git proxy. The agent never sees either.
//
// # API surface
//
// Two operations:
//
//   - ParsePrivateKey: PEM bytes → *rsa.PrivateKey, supporting both
//     PKCS#1 ("BEGIN RSA PRIVATE KEY", GitHub's default) and PKCS#8
//     ("BEGIN PRIVATE KEY", common after openssl conversion).
//   - Minter.MintInstallationToken: sign an app JWT, POST to the
//     access_tokens endpoint, parse and return the result.
//
// The minter is intentionally stateless (no caching). Callers that
// want token reuse — like internal/gitproxy — wrap a cached layer
// around it.
package githubapp

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// defaultAPIBase is the public-github REST endpoint. GHE installations
// pass a different base via Minter.APIBase.
const defaultAPIBase = "https://api.github.com"

// jwtTTL is how long the app-level JWT we sign lives. GitHub caps this
// at 10 minutes; we use a shorter window so a clock-skew compensation
// (iat backdated 60s) still leaves comfortable headroom.
const jwtTTL = 9 * time.Minute

// jwtIATSkew backdates the iat claim by this amount to tolerate small
// clock drift between this host and api.github.com. GitHub will reject
// JWTs with iat in the future even slightly; backdating eliminates that
// failure mode on hosts with mildly skewed clocks.
const jwtIATSkew = 60 * time.Second

// Token is one minted installation access token plus its expiry.
//
// The Value is opaque (currently starts with "ghs_" but GitHub doesn't
// document this as stable). ExpiresAt comes straight from the
// access_tokens response in UTC; callers compare against time.Now().UTC().
type Token struct {
	Value     string
	ExpiresAt time.Time
}

// Installation is one App installation discovered via GET /app/installations:
// a GitHub account (user or org) on which the App is installed. AccountType
// is GitHub's verbatim "User" / "Organization"; CreatedAt is the installation's
// created_at (zero if GitHub omitted it).
type Installation struct {
	ID           int64
	AccountLogin string
	AccountType  string
	CreatedAt    time.Time
}

// Minter signs JWTs with a GitHub App's RSA private key and exchanges
// them for installation access tokens. Safe for concurrent use — each
// MintInstallationToken call is independent (no shared mutable state
// beyond the immutable key + config).
//
// The minter does not cache tokens. Callers that need caching (e.g.
// the git proxy keeping one token alive across a run) wrap their own
// cache around Mint.
type Minter struct {
	privateKey *rsa.PrivateKey
	appID      int64
	apiBase    string
	httpClient *http.Client

	// now is injectable for tests. Production callers leave it nil and
	// time.Now is used. Both the JWT iat/exp claims and (less critically)
	// jitter calculations flow through this hook so tests can pin a
	// deterministic clock.
	now func() time.Time
}

// Config bundles inputs to NewMinter. Kept separate from positional
// args so future fields (custom Accept header, GHE base, etc.) can land
// without breaking call sites.
type Config struct {
	// PrivateKey is the parsed RSA private key from the App's .pem.
	// Required.
	PrivateKey *rsa.PrivateKey

	// AppID is the GitHub App's numeric ID. Used as the "iss" claim on
	// the JWT. Required.
	//
	// Note: GitHub also accepts the App's client ID (string like
	// "Iv23ll..."). This package uses the numeric AppID for simplicity;
	// callers with only a client ID convert at registration time.
	AppID int64

	// APIBase is the REST endpoint root. Defaults to https://api.github.com.
	// Override for GitHub Enterprise Server, where the API typically
	// lives at "https://<ghe-host>/api/v3".
	APIBase string

	// HTTPClient is the client used for the access_tokens POST.
	// Defaults to a client with a 30s timeout. Override for tests
	// (httptest server) or for integrating retries / observability.
	HTTPClient *http.Client
}

// NewMinter validates the config and returns a ready-to-use Minter.
// Eagerly rejects misconfig so a missing key or zero AppID fails at
// boot rather than on the first mint attempt.
func NewMinter(cfg Config) (*Minter, error) {
	if cfg.PrivateKey == nil {
		return nil, errors.New("githubapp: PrivateKey is required")
	}
	if cfg.AppID <= 0 {
		return nil, fmt.Errorf("githubapp: AppID must be positive, got %d", cfg.AppID)
	}
	base := cfg.APIBase
	if base == "" {
		base = defaultAPIBase
	}
	// Strip any trailing slashes so callers can pass
	// "https://api.github.com", "https://api.github.com/", or even an
	// accidental "https://api.github.com//" without a resulting
	// "//app/installations/..." in the request URL. TrimRight removes
	// all of them, which is what we want — there is no shape where a
	// trailing slash on the API base is semantically meaningful.
	base = strings.TrimRight(base, "/")
	if err := validateAPIBase(base); err != nil {
		return nil, err
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Minter{
		privateKey: cfg.PrivateKey,
		appID:      cfg.AppID,
		apiBase:    base,
		httpClient: client,
	}, nil
}

// validateAPIBase rejects APIBase values that would silently misbehave:
// missing scheme/host, query/fragment, or http to a non-loopback host
// (the JWT we sign is a Bearer-class secret and must not cross
// cleartext on a real network).
//
// A non-empty path IS allowed — GitHub Enterprise Server installations
// pin the API under "https://<ghe-host>/api/v3", and the request URL
// is built by appending "/app/installations/<id>/access_tokens" to the
// base, which handles either shape correctly.
func validateAPIBase(base string) error {
	u, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("githubapp: parse APIBase %q: %w", base, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("githubapp: APIBase %q missing scheme or host (expected e.g. https://api.github.com)", base)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("githubapp: APIBase %q must not include query or fragment", base)
	}
	if u.Scheme != "https" {
		// u.Hostname() strips the port AND the IPv6 brackets, so it
		// works for "127.0.0.1:8080", "[::1]:8080", and "[::1]" alike.
		// Doing this by hand with net.SplitHostPort would reject the
		// port-less IPv6 literal because SplitHostPort returns an error
		// and the bracket form ("[::1]") then fails net.ParseIP.
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("githubapp: APIBase %q must use https (loopback http is allowed for tests)", base)
		}
	}
	return nil
}

// ParsePrivateKey accepts PEM-encoded RSA private key bytes in either
// PKCS#1 ("BEGIN RSA PRIVATE KEY", GitHub's default) or PKCS#8 ("BEGIN
// PRIVATE KEY", common after openssl conversion) format and returns
// the parsed key.
//
// jwt/v5's ParseRSAPrivateKeyFromPEM handles both formats transparently;
// this wrapper exists so callers don't depend on jwt internals and so
// the error message stays in the githubapp namespace.
func ParsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	if len(pemBytes) == 0 {
		return nil, errors.New("githubapp: empty PEM bytes")
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM(pemBytes)
	if err != nil {
		return nil, fmt.Errorf("githubapp: parse private key: %w", err)
	}
	return key, nil
}

// AppJWT signs and returns the app-level JWT used to authenticate to
// the /app/* endpoints. Exposed for tests and for the rare caller that
// wants to hit a non-installation endpoint (e.g. /app/installations to
// enumerate installs); production token minting flows through
// MintInstallationToken.
//
// Claim shape per GitHub docs:
//
//	iss = appID
//	iat = now - 60s (skew tolerance)
//	exp = now + 9m  (GitHub max is 10m; 9m leaves headroom)
//	alg = RS256
//
// No "aud" or "sub" — GitHub doesn't require either and adding unknown
// claims has no benefit.
func (m *Minter) AppJWT() (string, error) {
	now := m.timeNow()
	claims := jwt.MapClaims{
		"iss": m.appID,
		"iat": now.Add(-jwtIATSkew).Unix(),
		"exp": now.Add(jwtTTL).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, err := tok.SignedString(m.privateKey)
	if err != nil {
		return "", fmt.Errorf("githubapp: sign app JWT: %w", err)
	}
	return signed, nil
}

// installationTokenResponse is the subset of /app/installations/{id}/access_tokens
// we parse. GitHub returns more (permissions, repositories, repository_selection)
// but the proxy only needs token + expiry.
type installationTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// MintInstallationToken signs an app JWT and exchanges it for a
// short-lived installation access token, scoped to the given
// installationID's repositories and permissions.
//
// Network failures, non-2xx responses, and malformed JSON all surface
// as errors with the HTTP status and (truncated) body included for
// debuggability. A successful return guarantees Value != "" and
// ExpiresAt is non-zero and in the future at receipt time.
func (m *Minter) MintInstallationToken(ctx context.Context, installationID int64) (Token, error) {
	if installationID <= 0 {
		return Token{}, fmt.Errorf("githubapp: installationID must be positive, got %d", installationID)
	}
	appJWT, err := m.AppJWT()
	if err != nil {
		return Token{}, err
	}

	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", m.apiBase, installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return Token{}, fmt.Errorf("githubapp: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	// User-Agent is required by github.com; without it some endpoints
	// return 403. The string is informational — GitHub doesn't validate
	// it beyond "non-empty".
	req.Header.Set("User-Agent", "triage-factory-githubapp")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("githubapp: mint installation token: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusCreated {
		return Token{}, fmt.Errorf("githubapp: mint installation token: status %d, body: %s",
			resp.StatusCode, truncate(string(body), 512))
	}

	var parsed installationTokenResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Token{}, fmt.Errorf("githubapp: parse installation token response: %w", err)
	}
	if parsed.Token == "" {
		return Token{}, errors.New("githubapp: installation token response missing token field")
	}
	if parsed.ExpiresAt.IsZero() {
		return Token{}, errors.New("githubapp: installation token response missing expires_at field")
	}
	expiresAt := parsed.ExpiresAt.UTC()
	if !expiresAt.After(m.timeNow()) {
		return Token{}, fmt.Errorf("githubapp: installation token response expires_at is not in the future: %s", expiresAt.Format(time.RFC3339))
	}
	return Token{Value: parsed.Token, ExpiresAt: expiresAt}, nil
}

// installationListItem is the subset of an /app/installations array entry
// we keep. GitHub returns far more (permissions, events, repository_selection);
// installation mirroring only needs the id, the account it's installed on,
// and when it was created.
type installationListItem struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"account"`
	CreatedAt time.Time `json:"created_at"`
}

// ListInstallations enumerates every installation of the App via
// GET /app/installations, authenticated with an app-level JWT. It follows
// Link-header pagination so an App installed on more than one page of
// accounts (100/page) is returned in full.
//
// This is the read side of the installation mirror: the backfill reconcile
// upserts whatever this returns. A non-2xx response or malformed JSON
// surfaces as an error with the HTTP status and a truncated body.
func (m *Minter) ListInstallations(ctx context.Context) ([]Installation, error) {
	appJWT, err := m.AppJWT()
	if err != nil {
		return nil, err
	}

	next := m.apiBase + "/app/installations?per_page=100"
	var out []Installation
	for next != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return nil, fmt.Errorf("githubapp: build request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+appJWT)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		req.Header.Set("User-Agent", "triage-factory-githubapp")

		resp, err := m.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("githubapp: list installations: %w", err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		linkHeader := resp.Header.Get("Link")
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("githubapp: list installations: status %d, body: %s",
				resp.StatusCode, truncate(string(body), 512))
		}

		var page []installationListItem
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("githubapp: parse installations response: %w", err)
		}
		for _, it := range page {
			out = append(out, Installation{
				ID:           it.ID,
				AccountLogin: it.Account.Login,
				AccountType:  it.Account.Type,
				CreatedAt:    it.CreatedAt.UTC(),
			})
		}
		next = nextPageURL(linkHeader)
	}
	return out, nil
}

// nextPageURL extracts the rel="next" URL from a GitHub Link header, or ""
// when there's no next page. The header looks like:
//
//	<https://api.github.com/app/installations?page=2>; rel="next", <...>; rel="last"
func nextPageURL(link string) string {
	for _, part := range strings.Split(link, ",") {
		segs := strings.Split(strings.TrimSpace(part), ";")
		if len(segs) < 2 {
			continue
		}
		urlPart := strings.TrimSpace(segs[0])
		if !strings.HasPrefix(urlPart, "<") || !strings.HasSuffix(urlPart, ">") {
			continue
		}
		for _, attr := range segs[1:] {
			if strings.TrimSpace(attr) == `rel="next"` {
				return urlPart[1 : len(urlPart)-1]
			}
		}
	}
	return ""
}

// timeNow returns the current time, honoring the testable now hook.
func (m *Minter) timeNow() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// truncate returns at most n bytes of s, with an ellipsis if cut.
// Local helper to keep error messages bounded when the GitHub error
// response is large.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
