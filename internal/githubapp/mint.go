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
	"bytes"
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
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
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
// created_at (zero if GitHub omitted it). AccountID is the account's numeric
// id — the half of the account's identity that a rename does not change, so it
// is what the installation mirror keys credential resolution on; 0 when GitHub
// omitted it. SuspendedAt is the installation's suspended_at (zero when the
// account owner has not suspended it) and SuspendedBy the login that did,
// carried so the reconcile can converge suspension that no webhook delivered.
// RepositorySelection is GitHub's "all" / "selected" — whether the grant covers
// every repository on the account or an enumerated set — which is what says
// whether the grant can drift from the tracked set at all.
type Installation struct {
	ID                  int64
	AccountID           int64
	AccountLogin        string
	AccountType         string
	CreatedAt           time.Time
	SuspendedAt         time.Time
	SuspendedBy         string
	RepositorySelection string
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
		client = telemetry.TracedHTTPClient(30*time.Second, "github")
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
// short-lived installation access token carrying the installation's
// FULL granted repositories and permissions (no narrowing — nil request
// body). Callers that want a least-privilege token narrowed to one repo
// (or a permission subset) use MintScopedInstallationToken.
//
// Network failures, non-2xx responses, and malformed JSON all surface
// as errors with the HTTP status and (truncated) body included for
// debuggability. A successful return guarantees Value != "" and
// ExpiresAt is non-zero and in the future at receipt time.
func (m *Minter) MintInstallationToken(ctx context.Context, installationID int64) (Token, error) {
	return m.mintInstallationToken(ctx, installationID, nil)
}

// MintScopedInstallationToken mints an installation access token narrowed
// below the installation's grant: scoped to exactly repos (bare repo
// NAMES, not "owner/repo" — GitHub's API takes names because the
// installation already fixes the account) and, when permissions is
// non-empty, to that permission subset. Both fields can only *narrow* —
// requesting a repo outside the installation or a permission it lacks is a
// 422, which the caller treats as "this credential can't serve this repo"
// and falls through (see the resolver's scoped tier).
//
// An empty permissions map omits the field entirely, yielding the
// installation's full permission set on the single repo — the right
// default for the API channel, whose verb set spans several permissions
// and would break under an over-narrow fixed set. The git proxy passes
// {"contents":"write"}, the only permission a clone/fetch/push needs.
//
// Same response handling as MintInstallationToken.
func (m *Minter) MintScopedInstallationToken(ctx context.Context, installationID int64, repos []string, permissions map[string]string) (Token, error) {
	body := map[string]any{}
	if len(repos) > 0 {
		body["repositories"] = repos
	}
	if len(permissions) > 0 {
		body["permissions"] = permissions
	}
	if len(body) == 0 {
		// No narrowing requested — identical to the full-install mint.
		return m.mintInstallationToken(ctx, installationID, nil)
	}
	return m.mintInstallationToken(ctx, installationID, body)
}

// mintInstallationToken is the shared POST/parse core behind both mint
// entry points. body nil means the full-install token (no request body);
// a non-nil body is JSON-marshalled to narrow the token (repositories /
// permissions).
func (m *Minter) mintInstallationToken(ctx context.Context, installationID int64, body any) (_ Token, err error) {
	// Its own span because of where this is called from: credential
	// resolution. A caller asks for a client and gets one; nothing in that
	// call's shape suggests it may have signed a JWT and round-tripped to
	// GitHub first, so under the caller's span the cost lands on whatever
	// the client was then used for. scoped separates a narrowed token from
	// a full-installation one; installationID stays off, since it
	// identifies the customer's install as surely as its name would.
	ctx, span := tracer.Start(ctx, "githubapp.mint_installation_token",
		trace.WithAttributes(attribute.Bool("scoped", body != nil)))
	defer span.End()
	// Named error return so the failure exits below don't each need a
	// status line. Fixed message, not err.Error() — a failed mint's error
	// text carries a truncated GitHub response body.
	defer func() {
		if err != nil {
			span.SetStatus(codes.Error, "mint failed")
		}
	}()

	if installationID <= 0 {
		return Token{}, fmt.Errorf("githubapp: installationID must be positive, got %d", installationID)
	}
	appJWT, err := m.AppJWT()
	if err != nil {
		return Token{}, err
	}

	var bodyReader io.Reader
	if body != nil {
		b, mErr := json.Marshal(body)
		if mErr != nil {
			return Token{}, fmt.Errorf("githubapp: marshal token request body: %w", mErr)
		}
		bodyReader = bytes.NewReader(b)
	}

	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", m.apiBase, installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bodyReader)
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
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("githubapp: mint installation token: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusCreated {
		return Token{}, fmt.Errorf("githubapp: mint installation token: status %d, body: %s",
			resp.StatusCode, truncate(string(respBody), 512))
	}

	var parsed installationTokenResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
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
// we keep. GitHub returns far more (permissions, events, single_file_name);
// installation mirroring only needs the id, the account it's installed on, when
// it was created, whether the account owner has suspended it, and how wide the
// grant is. Both halves of the account's identity are kept — the numeric id
// (stable) and the login (renameable, and what the UI renders).
//
// suspended_at / suspended_by are the reconcile's half of suspension, and the
// reason it is not webhook-only: GitHub does not re-deliver a missed
// installation.suspend, and local mode receives no deliveries at all. Both are
// nullable on the wire; encoding/json leaves a null as the zero value, so an
// unsuspended installation reads back as a zero time and an empty login
// without any pointer indirection.
type installationListItem struct {
	ID      int64 `json:"id"`
	Account struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"account"`
	CreatedAt   time.Time `json:"created_at"`
	SuspendedAt time.Time `json:"suspended_at"`
	SuspendedBy struct {
		Login string `json:"login"`
	} `json:"suspended_by"`
	// repository_selection is the other half of what the reconcile cannot learn
	// from a webhook it never received: an installation narrowed from "all" to a
	// selection fires installation_repositories, which GitHub does not re-deliver
	// and local mode never receives at all.
	RepositorySelection string `json:"repository_selection"`
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
			inst := Installation{
				ID:                  it.ID,
				AccountID:           it.Account.ID,
				AccountLogin:        it.Account.Login,
				AccountType:         it.Account.Type,
				CreatedAt:           it.CreatedAt.UTC(),
				SuspendedBy:         it.SuspendedBy.Login,
				RepositorySelection: it.RepositorySelection,
			}
			if !it.SuspendedAt.IsZero() {
				inst.SuspendedAt = it.SuspendedAt.UTC()
			}
			out = append(out, inst)
		}
		if next, err = m.nextPage(linkHeader); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// APIStatusError is a non-2xx answer from an App-JWT-authenticated endpoint —
// GetApp, which is where a refused status has to be told apart from a transport
// failure. It carries the status alongside the message so a caller can tell one
// refusal from another. 401 is the one that most needs telling apart: it is what
// GitHub answers a JWT whose iss does not match the signing key, so it means the
// App ID and private key are not a pair — which wants a different operator
// message from a GitHub that is simply not answering.
//
// Op names the operation rather than the URL, since the URL carries the
// configured API base and the message is read by operators, not resolved by
// machines.
type APIStatusError struct {
	Op         string
	StatusCode int
	Body       string
}

func (e *APIStatusError) Error() string {
	return fmt.Sprintf("githubapp: %s: status %d, body: %s", e.Op, e.StatusCode, truncate(e.Body, 512))
}

// App is the App's own metadata, returned by GET /app authenticated with an
// app-level JWT. A GitHub App authenticates itself: minting a JWT off the App's
// private key and calling /app yields the App's slug, owner, granted permission
// set, subscribed events, and client_id in one round trip — which is what the
// bring-your-own-App import validates the App-ID/key pair with and derives the
// registration row from (owner_type from Owner.Type is better provenance than
// the manifest path's query param).
type App struct {
	ID          int64
	Slug        string
	OwnerLogin  string
	OwnerType   string // GitHub's verbatim "User" / "Organization"
	Permissions map[string]string
	Events      []string
	ClientID    string
}

// appResponse is the subset of GET /app we parse. GitHub returns far more
// (name, description, html_url, created_at, …); the import flow only needs the
// identity (id/slug/client_id), the owner (login + account type), and the
// granted permissions + events for the preflight.
type appResponse struct {
	ID       int64  `json:"id"`
	Slug     string `json:"slug"`
	ClientID string `json:"client_id"`
	Owner    struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"owner"`
	Permissions map[string]string `json:"permissions"`
	Events      []string          `json:"events"`
}

// GetApp fetches the App's own metadata via GET /app, authenticated with an
// app-level JWT (no installation needed — the App authenticates as itself).
// This is the read the BYOA import uses to (a) prove the submitted App-ID +
// private key are a valid pair — GitHub 401s a JWT whose iss doesn't match the
// signing key — and (b) derive everything the manifest path would have
// collected (slug, owner login + type, permissions, events, client_id). A
// non-2xx (notably 401 on a bad ID/key pair) or malformed JSON surfaces as an
// error with the HTTP status and a truncated body.
func (m *Minter) GetApp(ctx context.Context) (App, error) {
	appJWT, err := m.AppJWT()
	if err != nil {
		return App{}, err
	}

	url := m.apiBase + "/app"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return App{}, fmt.Errorf("githubapp: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "triage-factory-githubapp")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return App{}, fmt.Errorf("githubapp: get app: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return App{}, &APIStatusError{Op: "get app", StatusCode: resp.StatusCode, Body: string(body)}
	}

	var parsed appResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return App{}, fmt.Errorf("githubapp: parse app response: %w", err)
	}
	return App{
		ID:          parsed.ID,
		Slug:        parsed.Slug,
		OwnerLogin:  parsed.Owner.Login,
		OwnerType:   parsed.Owner.Type,
		Permissions: parsed.Permissions,
		Events:      parsed.Events,
		ClientID:    parsed.ClientID,
	}, nil
}

// nextPage returns the rel="next" URL from a Link header, or "" when there is
// no next page — after checking that it addresses the same origin as the
// configured API base.
//
// The check is what makes following the header safe. Every request this package
// paginates carries the App's signed JWT in an Authorization header, so a Link
// pointing at another host would hand that credential to whoever answers, and
// would make this process fetch a URL of the remote's choosing (the API base
// may be an arbitrary GHES host, which can be misconfigured behind a proxy, or
// simply wrong). The response body is attacker-shaped input; the request target
// must not be. Go's client already strips Authorization across a redirect to a
// different host, so this closes the half the stdlib does not see.
//
// Scheme and host must match; the path is not constrained, since a same-origin
// path is neither a credential leak nor a reachability gain. A mismatch is an
// error rather than a quiet stop: pagination that silently truncates would
// report a partial listing as a complete one, and a Link header pointing off
// the configured host is an anomaly worth surfacing either way.
func (m *Minter) nextPage(link string) (string, error) {
	next := nextPageURL(link)
	if next == "" {
		return "", nil
	}
	nu, err := url.Parse(next)
	if err != nil {
		return "", fmt.Errorf("githubapp: parse pagination link: %w", err)
	}
	// apiBase was parsed and validated in NewMinter, so this cannot fail for a
	// live Minter; the error is kept rather than ignored so a future
	// construction path can't quietly skip the comparison.
	bu, err := url.Parse(m.apiBase)
	if err != nil {
		return "", fmt.Errorf("githubapp: parse API base: %w", err)
	}
	if !strings.EqualFold(nu.Scheme, bu.Scheme) || !strings.EqualFold(nu.Host, bu.Host) {
		return "", fmt.Errorf("githubapp: pagination link points at %s://%s, not the configured API host %s://%s",
			nu.Scheme, nu.Host, bu.Scheme, bu.Host)
	}
	return next, nil
}

// nextPageURL extracts the rel="next" URL from a GitHub Link header, or ""
// when there's no next page. The header looks like:
//
//	<https://api.github.com/app/installations?page=2>; rel="next", <...>; rel="last"
//
// Callers reach it through Minter.nextPage, which additionally pins the URL to
// the configured API origin — this half only parses.
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
