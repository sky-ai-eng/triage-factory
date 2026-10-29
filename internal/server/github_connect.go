package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// Connect GitHub — the user-to-server OAuth handler that binds a
// host-verified GitHub `login` to the signed-in TF user.
//
// Why this exists separately from everything else GitHub-shaped: TF touches
// GitHub through three credentials that must not be conflated.
//
//   - App installation token (internal/githubapp) is *access* — read/write
//     the org's repos. It authenticates as the app against the org, so it
//     carries no person.
//   - GoTrue "Sign in with GitHub" is *login* — it mints a TF session on the
//     deployment's GitHub (TF_DEFAULT_GITHUB_HOST), which need not be the
//     org's host, and under Entra SAML there's no GitHub leg at all.
//   - This handler is *identity* — a one-time consent that answers whoami
//     against the org's actual host (github.com OR a GHES host), writing
//     user_github_identities with source='connect_oauth'.
//
// It runs through one of two OAuth clients. An org with its own App uses
// that App's stored client_id/secret and its per-org callback. In multi
// mode, an org without one, on the deployment App's GitHub or on none yet,
// uses the deployment App's client and returns to the one callback that App
// registers (ManagedBindCallbackPath), where the front door tells a Connect from
// a bind by this flow's state cookie. Either way the code is exchanged for a
// `ghu_` user token, GET /user plus /user/emails capture the account, the
// identity is recorded, and the token is discarded — there's no per-user API
// consumer yet, so nothing is persisted beyond the identity row.
//
// This is NOT a TF session: it's attribute-binding, not login. The session
// the user already holds (from GoTrue or SAML) is untouched.

const connectStateCookieName = "tf_gh_connect_state"

// connectStatePath scopes the state cookie to the connect callback for a
// specific org, so it travels on the GitHub→TF redirect (SameSite=Lax sends
// it on top-level GETs) but nowhere else.
func connectStatePath(orgID string) string {
	return "/api/orgs/" + orgID + "/github/connect/"
}

// connectCallbackURL is the absolute redirect_uri GitHub bounces the user
// back to after consent. It must match a callback registered on the App, so
// it's also emitted into the manifest's callback_urls — one source of truth
// shared by buildManifestAndState and the start handler so they can't drift.
func (s *Server) connectCallbackURL(orgID string) string {
	return s.deployCfg.publicURL + "/api/orgs/" + orgID + "/github/connect/callback"
}

// resolveGitHubHost returns the org's GitHub base URL resolved to the
// canonical value user_github_identities rows are keyed under for that org —
// the *same* value the PAT writer (settings_handlers) and the
// factory/me/settings readers key on: orgSet.GitHubBaseURL in its canonical
// form (ghbase.CanonicalBaseURL: trailing slashes trimmed, scheme and host
// lowercased). An org with no GitHub URL has no host: ok=false.
// Connect (writer) and the identity-status gate (reader) both route through
// here, so the (user, host) key always agrees with the other sites —
// crucially including a base URL
// that carries a path, which a bare-origin derivation would silently strip
// (writing/reading a different key than everyone else).
//
// gitHubWebOrigin is used only to *validate* the configured base URL is a real
// http(s) origin; its path-stripped output is intentionally discarded so the
// returned value matches what the other writers/readers store. ok=false on an
// empty or malformed base URL.
func resolveGitHubHost(orgBase string) (string, bool) {
	host := ghbase.CanonicalBaseURL(orgBase)
	if _, ok := gitHubWebOrigin(host); !ok {
		return "", false
	}
	return host, true
}

// orgGitHubHost fetches the org's configured base URL and resolves it to that
// same canonical host, for callers that hold only an orgID. It is the fetching
// sibling of resolveGitHubHost and lands on the identical string for any base
// URL that resolveGitHubHost accepts — db.EffectiveGitHubHost applies the same
// canonical form, and is what the poller and the routing subscribers already
// key on. "" for an org with no GitHub URL.
//
// It does NOT re-validate the base URL the way resolveGitHubHost does. That
// gate belongs to the settings writer, and a caller that is recording which
// GitHub a row came from wants the org's answer whatever it is — refusing a
// host the rest of the system happily uses would key this row differently from
// every other row about the same GitHub.
//
// System (claims-free) read: the caller is the pre-auth webhook receiver, which
// has a trusted org_id from the URL and no session.
func (s *Server) orgGitHubHost(ctx context.Context, orgID string) (string, error) {
	if s.orgs == nil {
		return "", fmt.Errorf("read github host: orgs store not wired")
	}
	set, err := s.orgs.GetSettingsSystem(ctx, orgID)
	if err != nil {
		return "", fmt.Errorf("read github host: %w", err)
	}
	return db.EffectiveGitHubHost(set.GitHubBaseURL), nil
}

// noGitHubURLMessage answers a request that needs the org's GitHub host from
// an org with no GitHub URL.
const noGitHubURLMessage = "this organization has no GitHub URL; an admin sets it in organization settings"

// requireOrgGitHubHost is orgGitHubHost for a route that cannot answer without
// a host: a read failure is a 500 and an org with no GitHub URL is a 409
// NOT_CONFIGURED. ok=false means the response is written.
func (s *Server) requireOrgGitHubHost(ctx context.Context, w http.ResponseWriter, orgID, area string) (string, bool) {
	host, err := s.orgGitHubHost(ctx, orgID)
	if err != nil {
		internalError(w, area, err)
		return "", false
	}
	if host == "" {
		writeNotConfigured(w, noGitHubURLMessage)
		return "", false
	}
	return host, true
}

// connectClient is the OAuth client a Connect runs through and the GitHub it
// captures the identity on: the org's own App, or the deployment App.
type connectClient struct {
	ghWeb        string
	clientID     string
	clientSecret string // only the callback's code exchange uses it
	redirectURI  string
	// statePath scopes the state cookie to the callback this client returns
	// to, so the cookie travels on the GitHub→TF redirect and nowhere else.
	statePath string
}

// ownAppCanConnect reports whether the org's own App can run Connect's
// code exchange: it needs the client id and a stored client secret both.
func ownAppCanConnect(app *domain.OrgGitHubApp) bool {
	return app != nil && app.ClientID != "" && app.ClientSecretRef != ""
}

// ownAppConnectClient is the org's own App as Connect's client, on the
// org's host and its per-org callback.
func (s *Server) ownAppConnectClient(orgID, ghWeb string, app *domain.OrgGitHubApp, clientSecret string) connectClient {
	return connectClient{
		ghWeb:        ghWeb,
		clientID:     app.ClientID,
		clientSecret: clientSecret,
		redirectURI:  s.connectCallbackURL(orgID),
		statePath:    connectStatePath(orgID),
	}
}

// deploymentAppServesConnect reports whether Connect runs through the
// deployment App for an org whose GitHub URL is orgBase: multi mode, a
// configured deployment App, and an org on that App's GitHub or on none
// yet. An org with no host links on the App's GitHub, which is the host the
// bind then gives it. Decided from configuration and the stored host alone, so
// the identity status read can answer it without asking GitHub anything.
func (s *Server) deploymentAppServesConnect(orgBase string) bool {
	if runmode.Current() != runmode.ModeMulti || !s.deploymentApp.Configured() {
		return false
	}
	host := ghbase.CanonicalBaseURL(orgBase)
	return host == "" || host == ghbase.DefaultBaseURL()
}

// deploymentAppConnectClient is the deployment App as Connect's client. The
// client id is not configured but reported by GET /app, so this runs the App's
// preflight, which also refuses an App missing a permission the capture needs.
// On failure it returns the gate page's error code instead.
func (s *Server) deploymentAppConnectClient(ctx context.Context, orgID string) (connectClient, string) {
	ghWeb := ghbase.DefaultBaseURL()
	minter, err := s.deploymentApp.Minter(ghbase.APIBase(ghWeb))
	if err != nil {
		githubConnectLog.Warn("deployment app minter unavailable", "org", orgID, "error", err)
		return connectClient{}, "deployment_app_unavailable"
	}
	identity, err := githubapp.PreflightDeploymentApp(ctx, minter)
	if err != nil {
		githubConnectLog.Warn("deployment app preflight failed", "org", orgID, "error", err)
		if errors.Is(err, githubapp.ErrDeploymentAppUnreachable) {
			return connectClient{}, "host_unreachable"
		}
		return connectClient{}, "deployment_app_unavailable"
	}
	if identity.ClientID == "" {
		githubConnectLog.Warn("deployment app reports no client id", "org", orgID)
		return connectClient{}, "deployment_app_unavailable"
	}
	return connectClient{
		ghWeb:        ghWeb,
		clientID:     identity.ClientID,
		clientSecret: s.deploymentApp.ClientSecret,
		redirectURI:  s.deployCfg.publicURL + ManagedBindCallbackPath,
		statePath:    ManagedBindCallbackPath,
	}, ""
}

// handleGitHubConnectStart kicks off the user-to-server OAuth dance:
// redirect the browser to {host}/login/oauth/authorize with the client_id of
// the client that serves this org and an HMAC-signed CSRF state. The
// org's own App targets the org's host, NOT github.com — the whole
// point is binding the correct GHES login for a GHES org; the deployment App
// targets its own GitHub, which is the org's or will be. Any org member
// may bind their own identity.
//
// GET /api/orgs/{org_id}/github/connect/start?return_to=/some/path
func (s *Server) handleGitHubConnectStart(w http.ResponseWriter, r *http.Request) {
	if s.deployCfg == nil {
		http.NotFound(w, r)
		return
	}
	orgID, userID, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}

	returnTo := normalizeReturnTo(r.URL.Query().Get("return_to"))

	var app *domain.OrgGitHubApp
	var orgSet domain.OrgSettings
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var lerr error
		app, lerr = tx.GitHubApps.GetForOrg(r.Context(), orgID)
		if lerr != nil {
			return lerr
		}
		orgSet, lerr = tx.Orgs.GetSettings(r.Context(), orgID)
		return lerr
	}); err != nil {
		internalError(w, "github-connect", err)
		return
	}

	// The org's own App first, then the deployment App. With neither
	// there is no OAuth client to run, so bounce back to the gate page with a
	// code rather than dead-ending.
	var client connectClient
	switch {
	case ownAppCanConnect(app):
		ghWeb, okHost := resolveGitHubHost(orgSet.GitHubBaseURL)
		if !okHost {
			githubConnectLog.Warn("invalid github base url", "org", orgID)
			s.redirectConnect(w, r, orgID, returnTo, "bad_host")
			return
		}
		client = s.ownAppConnectClient(orgID, ghWeb, app, "")
	case s.deploymentAppServesConnect(orgSet.GitHubBaseURL):
		var errCode string
		client, errCode = s.deploymentAppConnectClient(r.Context(), orgID)
		if errCode != "" {
			s.redirectConnect(w, r, orgID, returnTo, errCode)
			return
		}
	default:
		s.redirectConnect(w, r, orgID, returnTo, "no_app")
		return
	}

	csrfRaw := make([]byte, 16)
	if _, err := rand.Read(csrfRaw); err != nil {
		internalError(w, "github-connect", err)
		return
	}
	st := connectState{
		OrgID:     orgID,
		UserID:    userID,
		CSRF:      hex.EncodeToString(csrfRaw),
		ReturnTo:  returnTo,
		ExpiresAt: timeNow().Add(10 * time.Minute).Unix(),
	}
	signed, err := st.sign(s.deployCfg.hmacKey)
	if err != nil {
		internalError(w, "github-connect", err)
		return
	}
	http.SetCookie(w, s.connectStateCookie(r, client.statePath, signed, 600))

	// No `scope`: this is a GitHub *App* user-to-server flow, where the token's
	// capabilities come from the App's configured permissions, not an OAuth
	// scope string (the OAuth-App model). This flow calls /user/emails, so the
	// manifest grants read access to Email addresses. Only the CSRF nonce travels through GitHub; the
	// orgID/userID/return_to stay in the signed cookie, off GitHub's logs and
	// the URL bar.
	q := url.Values{}
	q.Set("client_id", client.clientID)
	q.Set("redirect_uri", client.redirectURI)
	q.Set("state", st.CSRF)
	target := client.ghWeb + "/login/oauth/authorize?" + q.Encode()

	http.Redirect(w, r, target, http.StatusFound)
}

// handleGitHubConnectCallback completes the dance for the org's own
// App: validate state/CSRF, then finishGitHubConnect. On any failure it
// redirects back to the gate page with a distinguishing error code
// (host_unreachable vs connect_failed vs denied) rather than dead-ending a
// top-level navigation in a JSON error. The deployment App's Connect returns
// to ManagedBindCallbackPath instead (completeDeploymentAppConnect).
//
// GET /api/orgs/{org_id}/github/connect/callback?code=...&state=...
func (s *Server) handleGitHubConnectCallback(w http.ResponseWriter, r *http.Request) {
	if s.deployCfg == nil {
		http.NotFound(w, r)
		return
	}
	orgID, userID, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}

	// State cookie carries the signed flow context; clear it once read so a
	// stale cookie can't be replayed.
	cookie, cookieErr := r.Cookie(connectStateCookieName)
	http.SetCookie(w, s.connectStateCookie(r, connectStatePath(orgID), "", -1))
	if cookieErr != nil {
		s.redirectConnect(w, r, orgID, "", "state")
		return
	}
	cs, err := parseConnectState(cookie.Value, s.deployCfg.hmacKey)
	if err != nil {
		githubConnectLog.Warn("state cookie parse failed", "error", err)
		s.redirectConnect(w, r, orgID, "", "state")
		return
	}
	// CSRF: the nonce GitHub echoed must match the cookie's. Org + user
	// binding closes the cross-user attack (an attacker's signed state
	// carries their userID, which won't match the victim's session).
	if cs.CSRF == "" || r.URL.Query().Get("state") != cs.CSRF || cs.OrgID != orgID || cs.UserID != userID {
		s.redirectConnect(w, r, orgID, cs.ReturnTo, "state")
		return
	}

	returnTo := connectReturnTo(cs.ReturnTo, orgID)
	code, errCode := connectCode(r, orgID)
	if errCode != "" {
		s.redirectConnect(w, r, orgID, returnTo, errCode)
		return
	}

	// Read App credentials + host. The client_secret lives in the SecretStore;
	// the Get runs under the claims tx so the multi-mode org_secrets read is
	// claims-gated (RLS) before TF decrypts it app-side.
	var app *domain.OrgGitHubApp
	var clientSecret string
	var orgSet domain.OrgSettings
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var lerr error
		app, lerr = tx.GitHubApps.GetForOrg(r.Context(), orgID)
		if lerr != nil {
			return lerr
		}
		orgSet, lerr = tx.Orgs.GetSettings(r.Context(), orgID)
		if lerr != nil {
			return lerr
		}
		if app != nil && app.ClientSecretRef != "" {
			clientSecret, lerr = tx.Secrets.Get(r.Context(), orgID, app.ClientSecretRef)
		}
		return lerr
	}); err != nil {
		internalError(w, "github-connect", err)
		return
	}
	if app == nil || app.ClientID == "" || clientSecret == "" {
		s.redirectConnect(w, r, orgID, returnTo, "no_app")
		return
	}

	ghWeb, okHost := resolveGitHubHost(orgSet.GitHubBaseURL)
	if !okHost {
		s.redirectConnect(w, r, orgID, returnTo, "bad_host")
		return
	}

	s.finishGitHubConnect(w, r, orgID, userID, returnTo, s.ownAppConnectClient(orgID, ghWeb, app, clientSecret), code)
}

// completeDeploymentAppConnect completes a Connect that ran through the
// deployment App. It arrives at ManagedBindCallbackPath, the one callback that
// App registers, and the front door (managedBindCallback) hands it here because
// the browser holds this flow's state cookie, cs, and the query's state is not
// the bind ceremony's (deploymentAppConnectReturn).
//
// That state need not be cs's nonce. Every Connect through the deployment App
// shares one cookie name and path, so a Connect started in a second tab
// replaces the first's cookie, and the first tab's return then carries a state
// the cookie no longer holds. That return is refused as an expired attempt and
// leaves the cookie alone, because the cookie is the later Connect's, still in
// flight. Nothing is lost by it: every Connect here targets the App's one
// GitHub, and the identity row is keyed by user and host, so the later Connect
// links the same row.
//
// The org comes from the cookie rather than the path, so this re-establishes
// what a path-scoped route gets from RequireOrgMember: a session rather than an
// API token, the cookie's user as the session's user, that user still a member
// of the cookie's org, and the org's "Require SSO" asked here. Open to any
// member, like the per-org callback, so it runs before and independently
// of the bind's admin re-check.
func (s *Server) completeDeploymentAppConnect(w http.ResponseWriter, r *http.Request, cs *connectState) {
	ctx := r.Context()
	// A session, specifically, for the reason completeManagedBindCallback
	// gives: this path names no org for a token's sealed org to be checked
	// against. Checked before the cookie is cleared, as the bind checks it
	// before spending its record: a stray header must not end the browser's
	// Connect.
	claims := httpx.ClaimsFrom(ctx)
	if SessionFrom(ctx) == nil || claims == nil || claims.Subject == "" {
		s.renderBindOutcome(w, "", refuseSessionRequired)
		return
	}
	userID := claims.Subject
	// A refusal before the cookie's org is established goes to the session's
	// own org, never the cookie's: the caller has proven no relationship
	// to it.
	fallbackOrg := httpx.OrgIDFrom(ctx)
	if r.URL.Query().Get("state") != cs.CSRF {
		s.redirectConnect(w, r, fallbackOrg, "", "state")
		return
	}
	// This return is the cookie's own, so it spends the cookie: cleared before
	// anything else is decided, so it can never be replayed.
	http.SetCookie(w, s.connectStateCookie(r, ManagedBindCallbackPath, "", -1))
	if cs.UserID != userID {
		s.redirectConnect(w, r, fallbackOrg, "", "state")
		return
	}
	// From here the org is the cookie's, and membership is read again rather
	// than trusted from the start: minutes have passed.
	orgID := cs.OrgID
	member, err := s.az.UserHasOrgAccess(ctx, userID, orgID)
	if err != nil {
		internalError(w, "github-connect", err)
		return
	}
	if !member {
		s.redirectConnect(w, r, fallbackOrg, "", "state")
		return
	}
	returnTo := connectReturnTo(cs.ReturnTo, orgID)
	gate, err := s.requestSSOGate(ctx, orgID)
	if err != nil {
		internalError(w, "github-connect", err)
		return
	}
	if gate.Barred {
		s.redirectConnect(w, r, orgID, returnTo, "sso_required")
		return
	}

	code, errCode := connectCode(r, orgID)
	if errCode != "" {
		s.redirectConnect(w, r, orgID, returnTo, errCode)
		return
	}

	// The org may have moved to another GitHub since the start; the
	// deployment App then no longer serves it.
	var orgSet domain.OrgSettings
	if err := s.tx.WithReadTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var lerr error
		orgSet, lerr = tx.Orgs.GetSettings(ctx, orgID)
		return lerr
	}); err != nil {
		internalError(w, "github-connect", err)
		return
	}
	if !s.deploymentAppServesConnect(orgSet.GitHubBaseURL) {
		s.redirectConnect(w, r, orgID, returnTo, "no_app")
		return
	}
	client, errCode := s.deploymentAppConnectClient(ctx, orgID)
	if errCode != "" {
		s.redirectConnect(w, r, orgID, returnTo, errCode)
		return
	}

	s.finishGitHubConnect(w, r, orgID, userID, returnTo, client, code)
}

// connectReturnTo is where a completed Connect sends the browser: the start's
// return_to, or the org's home when it named none.
func connectReturnTo(raw, orgID string) string {
	returnTo := normalizeReturnTo(raw)
	if returnTo == "/" {
		returnTo = "/orgs/" + orgID
	}
	return returnTo
}

// connectCode reads the authorization code GitHub's redirect carries, or the
// gate page's error code when it carries none: denied when GitHub reports an
// OAuth error (the person declined, typically), connect_failed otherwise.
func connectCode(r *http.Request, orgID string) (code, errCode string) {
	if ghErr := r.URL.Query().Get("error"); ghErr != "" {
		githubConnectLog.Warn("github oauth error", "github_error", ghErr, "org", orgID)
		return "", "denied"
	}
	code = r.URL.Query().Get("code")
	if code == "" {
		return "", "connect_failed"
	}
	return code, ""
}

// finishGitHubConnect is the half of Connect both clients share: exchange the
// code for a user-to-server token, capture the account it belongs to on the
// client's GitHub, upsert user_github_identities(source='connect_oauth'), and
// send the browser to returnTo.
//
// The token is used only for the whoami and never stored — identity binding,
// not an API consumer. A network failure on either call surfaces as
// host_unreachable so the FE shows the infra state, not "you didn't connect."
func (s *Server) finishGitHubConnect(w http.ResponseWriter, r *http.Request, orgID, userID, returnTo string, client connectClient, code string) {
	ctx := r.Context()
	token, err := auth.ExchangeGitHubOAuthCode(ctx, client.ghWeb, client.clientID, client.clientSecret, code, client.redirectURI)
	if err != nil {
		githubConnectLog.Warn("token exchange failed", "org", orgID, "error", err)
		s.redirectConnect(w, r, orgID, returnTo, connectErrCode(err))
		return
	}
	ghUser, err := auth.CaptureGitHubIdentity(ctx, client.ghWeb, token)
	if err != nil {
		githubConnectLog.Warn("whoami failed", "org", orgID, "error", err)
		s.redirectConnect(w, r, orgID, returnTo, connectErrCode(err))
		return
	}
	if ghUser.Login == "" {
		s.redirectConnect(w, r, orgID, returnTo, "connect_failed")
		return
	}

	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		if err := tx.Users.UpsertGitHubIdentity(ctx, userID, client.ghWeb, ghUser.Login, ghUser.UserID(), ghUser.PrimaryEmail, "connect_oauth"); err != nil {
			return err
		}
		return recordGitHubIdentityBind(ctx, tx, orgID, userID, client.ghWeb, ghUser.Login)
	}); err != nil {
		internalError(w, "github-connect", err)
		return
	}

	githubConnectLog.Info("bound user",
		"user", userID, "login", ghUser.Login, "host", client.ghWeb, "org", orgID, "source", "connect_oauth")
	// Seed this login's trailing-window PR history so the personal dashboard
	// isn't blank for activity that predates tracking. Fire-and-forget +
	// marker-guarded; multi-mode only.
	s.kickDashboardBackfill(orgID, userID, ghUser.Login, client.ghWeb)
	http.Redirect(w, r, returnTo, http.StatusFound)
}

// githubIdentityStatusResponse is the read-only shape the onboarding gate
// polls to decide whether to block. `connected` is the single load-bearing
// bit; `connect_available` says whether Connect has an OAuth client to run
// for this org, its own App's or the deployment App's.
type githubIdentityStatusResponse struct {
	Connected        bool   `json:"connected"`
	Login            string `json:"login,omitempty"`
	Host             string `json:"host"`
	ConnectAvailable bool   `json:"connect_available"`
}

// handleGitHubIdentityStatus reports whether the caller has a host-verified
// GitHub identity for the active org's host — the gate's "is this user set
// up" check. Read-only; any org member. An absent row is connected=false,
// the durable supported state runtime tolerates (this endpoint never asserts
// a row must exist; it just reports presence).
//
// An org with no GitHub URL that the deployment App serves reports the
// App's host and reads the user's identity there: that is where Connect links
// it, and the host the bind gives the org, so an admin who linked before
// binding reads as connected.
//
// GET /api/orgs/{org_id}/github/identity
func (s *Server) handleGitHubIdentityStatus(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}

	var (
		login            string
		host             string
		connectAvailable bool
	)
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		orgSet, lerr := tx.Orgs.GetSettings(r.Context(), orgID)
		if lerr != nil {
			return lerr
		}
		ghWeb, okHost := resolveGitHubHost(orgSet.GitHubBaseURL)
		deploymentServes := s.deploymentAppServesConnect(orgSet.GitHubBaseURL)
		if !okHost && deploymentServes {
			ghWeb, okHost = ghbase.DefaultBaseURL(), true
		}
		if okHost {
			host = ghWeb
			login, lerr = tx.Users.GetGitHubLogin(r.Context(), userID, ghWeb)
			if lerr != nil {
				return lerr
			}
		} else {
			// No or malformed host config — surface the value for display but
			// report not-connected (we can't key a lookup off a bad host).
			host = ghbase.CanonicalBaseURL(orgSet.GitHubBaseURL)
		}
		app, lerr := tx.GitHubApps.GetForOrg(r.Context(), orgID)
		if lerr != nil {
			return lerr
		}
		// The same two clients the start chooses between, decided from
		// configuration and the stored host alone. An own App needs its stored
		// client secret as well as its client id, or the gate would offer a
		// Connect whose callback dead-ends on the missing secret.
		connectAvailable = ownAppCanConnect(app) || deploymentServes
		return nil
	}); err != nil {
		internalError(w, "github-identity", err)
		return
	}

	writeJSON(w, http.StatusOK, githubIdentityStatusResponse{
		Connected:        login != "",
		Login:            login,
		Host:             host,
		ConnectAvailable: connectAvailable,
	})
}

// githubIdentityPATRequest carries a user-supplied PAT for the capture-and-
// discard identity path (PAT_2). The token proves the caller's login and is
// then thrown away — never stored.
type githubIdentityPATRequest struct {
	PAT string `json:"pat"`
}

// githubIdentityCaptureResponse is the success shape of the PAT-capture path:
// the bound login and the host it's keyed under, so the caller can reflect the
// just-captured identity without re-reading the status endpoint.
type githubIdentityCaptureResponse struct {
	Login string `json:"login"`
	Host  string `json:"host"`
}

// errGitHubNoLogin is returned by validateGitHubIdentityPAT when the host
// accepts the token but yields no username — distinct from a rejected token so
// callers can render the right message.
var errGitHubNoLogin = errors.New("github returned no username for the token")

// recordGitHubIdentityBind audits a per-user GitHub identity binding, shared by
// the Connect OAuth callback and the PAT-capture handler. Neither retains a
// token, but "which GitHub account does this TF user act as" is
// squarely an access fact — every review and comment the user authorizes goes
// out under that login — so it belongs in the change-log alongside the org
// credentials. Actor and target are both the user: identity is self-bound.
func recordGitHubIdentityBind(ctx context.Context, tx db.TxStores, orgID, userID, host, login string) error {
	return tx.AccessChangeLog.Record(ctx, orgID, domain.AccessChange{
		ActorUserID:  userID,
		TargetUserID: userID,
		Action:       domain.AccessActionCredentialSet,
		DetailJSON:   accessDetailCredentialNamed(domain.CredentialKindGitHubIdentity, host, "@"+login),
	})
}

// validateGitHubIdentityPAT proves a PAT against the org's resolved GitHub host
// (GET /user) and returns the account it authenticates as — the @login and the
// numeric id that binding records together — WITHOUT storing the token, so the
// network call happens before the caller opens the tx that writes
// user_github_identities. Errors wrap auth.CaptureGitHubIdentity's (callers can
// errors.Is auth.ErrGitHubHostUnreachable) or are errGitHubNoLogin. The login
// is what proves the token; a host that answers with no id still binds, and the
// id fills in on a later capture.
func validateGitHubIdentityPAT(ctx context.Context, ghWeb, pat string) (auth.GitHubUser, error) {
	ghUser, err := auth.CaptureGitHubIdentity(ctx, ghWeb, pat)
	if err != nil {
		return auth.GitHubUser{}, err
	}
	if ghUser.Login == "" {
		return auth.GitHubUser{}, errGitHubNoLogin
	}
	return *ghUser, nil
}

// handleGitHubIdentityPAT binds the caller's GitHub identity from a personal
// access token they supply, WITHOUT storing the token. It captures the account
// against the org's resolved host (GET /user and /user/emails), writes
// user_github_identities(source='pat'), and discards the token — the same end
// state as the Connect OAuth flow, just proven by a token instead of a consent
// click. This is PAT_2: the per-user identity credential, independent of the
// org's PAT_1 access credential (they may be the same token; they are captured
// and stored independently). Local mode only: it is the fallback to Connect
// there, and the only capture path when no GitHub App is registered. In multi
// mode every set-up org has an OAuth client on its own GitHub (its own
// App, or the deployment App), so Connect is the one way in, and this route
// does not exist.
//
// POST /api/orgs/{org_id}/github/identity/pat
func (s *Server) handleGitHubIdentityPAT(w http.ResponseWriter, r *http.Request) {
	if runmode.Current() == runmode.ModeMulti {
		notFound(w, "route")
		return
	}
	orgID, userID, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	var req githubIdentityPATRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	pat := strings.TrimSpace(req.PAT)
	if pat == "" {
		badRequest(w, "A GitHub personal access token is required.")
		return
	}

	// Resolve the org's host the same way the status reader / Connect writer do,
	// so the (user, host) key always agrees across surfaces.
	var orgSet domain.OrgSettings
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var lerr error
		orgSet, lerr = tx.Orgs.GetSettings(r.Context(), orgID)
		return lerr
	}); err != nil {
		internalError(w, "github-identity", err)
		return
	}
	ghWeb, okHost := resolveGitHubHost(orgSet.GitHubBaseURL)
	if !okHost {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonNotConfigured, Message: "Your organization's GitHub URL looks misconfigured. Ask your admin to fix it in organization settings.", Field: "github_pat"})
		return
	}

	ghUser, err := validateGitHubIdentityPAT(r.Context(), ghWeb, pat)
	if err != nil {
		// Keep the two failure shapes distinct, like the Connect flow: a host we
		// couldn't reach (infra) vs. a token the host rejected (your action).
		if errors.Is(err, auth.ErrGitHubHostUnreachable) {
			httpx.WriteErrors(w, http.StatusBadGateway, httpx.ErrorItem{Reason: httpx.ReasonUpstreamUnavailable, Message: fmt.Sprintf("Couldn't reach %s. This is a connectivity issue between Triage Factory and your GitHub server, not the token — try again.", ghWeb), Field: "github_pat"})
			return
		}
		if errors.Is(err, errGitHubNoLogin) {
			httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{Reason: httpx.ReasonUpstreamRejected, Message: "GitHub didn't return a username for that token.", Field: "github_pat"})
			return
		}
		if errors.Is(err, auth.ErrGitHubEmailPermission) {
			httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{Reason: httpx.ReasonUpstreamRejected, Message: "That token needs read access to your GitHub email addresses. For a classic token, add the user:email scope.", Field: "github_pat"})
			return
		}
		if errors.Is(err, auth.ErrGitHubPrimaryEmailUnavailable) {
			httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{Reason: httpx.ReasonUpstreamRejected, Message: "GitHub did not return a verified primary email for that account.", Field: "github_pat"})
			return
		}
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{Reason: httpx.ReasonUpstreamRejected, Message: "That token didn't validate against " + ghWeb + ". Double-check it and try again.", Field: "github_pat"})
		return
	}

	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if err := tx.Users.UpsertGitHubIdentity(r.Context(), userID, ghWeb, ghUser.Login, ghUser.UserID(), ghUser.PrimaryEmail, "pat"); err != nil {
			return err
		}
		return recordGitHubIdentityBind(r.Context(), tx, orgID, userID, ghWeb, ghUser.Login)
	}); err != nil {
		internalError(w, "github-identity", err)
		return
	}
	// The token is intentionally not persisted anywhere — it existed only for
	// the whoami above. Identity captured; org access untouched.
	githubIdentityLog.Info("bound user", "user", userID, "login", ghUser.Login, "host", ghWeb, "org", orgID, "source", "pat")
	// Seed this login's trailing-window PR history for the dashboard (TFAC-396).
	s.kickDashboardBackfill(orgID, userID, ghUser.Login, ghWeb)

	writeJSON(w, http.StatusOK, githubIdentityCaptureResponse{Login: ghUser.Login, Host: ghWeb})
}

// redirectConnect bounces a failed/aborted Connect flow back to the FE gate
// page with an error code (and the original return_to so a retry resumes the
// right destination). errCode "" means a bare redirect with no error banner.
// With no org there is no gate page to name, so the browser goes to the app
// root, where the SPA resolves one.
func (s *Server) redirectConnect(w http.ResponseWriter, r *http.Request, orgID, returnTo, errCode string) {
	if orgID == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	q := url.Values{}
	if errCode != "" {
		q.Set("error", errCode)
	}
	if rt := normalizeReturnTo(returnTo); rt != "" && rt != "/" {
		q.Set("return_to", rt)
	}
	dest := "/orgs/" + orgID + "/connect-github"
	if enc := q.Encode(); enc != "" {
		dest += "?" + enc
	}
	http.Redirect(w, r, dest, http.StatusFound)
}

// connectStateCookie builds the state cookie — the set and the clear, from one
// place, because a clear whose Secure/SameSite/Path differ from the set may
// leave the browser holding a copy. path is the client's statePath.
func (s *Server) connectStateCookie(r *http.Request, path, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     connectStateCookieName,
		Value:    value,
		Path:     path,
		HttpOnly: true,
		Secure:   s.cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	}
}

// deploymentAppConnectReturn reports whether a request to
// ManagedBindCallbackPath is a Connect's return, and returns the state cookie
// the deployment App's Connect start set when it is: the browser holds that
// cookie, and the query carries a state that is not the bind ceremony's. The
// bind's install leg carries no state, and its authorize leg carries the hash
// of its cookie's nonce (hashBindNonce), which is computed here from the cookie
// alone, so the choice reads nothing. nil leaves the request to the bind.
//
// Whether the state is this cookie's own nonce is not decided here: a return
// from a Connect whose cookie a later one replaced is still a Connect's, and
// completeDeploymentAppConnect refuses it as one rather than letting it reach a
// bind ceremony it would spend.
func (s *Server) deploymentAppConnectReturn(r *http.Request) *connectState {
	state := r.URL.Query().Get("state")
	if state == "" {
		return nil
	}
	if bind, err := r.Cookie(managedBindCookieName); err == nil && bind.Value != "" && state == hashBindNonce(bind.Value) {
		return nil
	}
	cookie, err := r.Cookie(connectStateCookieName)
	if err != nil || cookie.Value == "" {
		return nil
	}
	cs, err := parseConnectState(cookie.Value, s.deployCfg.hmacKey)
	if err != nil || cs.CSRF == "" {
		return nil
	}
	return cs
}

// connectErrCode maps an exchange/whoami error to the FE error code. A
// network-level failure (couldn't reach the host) is host_unreachable — the
// infra state the gate renders distinctly; everything else is connect_failed.
func connectErrCode(err error) string {
	if errors.Is(err, auth.ErrGitHubHostUnreachable) {
		return "host_unreachable"
	}
	return "connect_failed"
}

// ---- state token (HMAC-signed, ~10min TTL) ----
//
// Mirrors appRegisterState's scheme. Carries the user + org binding plus a
// CSRF nonce; only the nonce echoes through GitHub.

type connectState struct {
	OrgID     string `json:"org_id"`
	UserID    string `json:"uid"`
	CSRF      string `json:"csrf"`
	ReturnTo  string `json:"return_to"`
	ExpiresAt int64  `json:"exp"`
}

func (c connectState) sign(key [32]byte) (string, error) {
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key[:])
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) +
		"." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func parseConnectState(raw string, key [32]byte) (*connectState, error) {
	parts := strings.SplitN(raw, ".", 2)
	if len(parts) != 2 {
		return nil, errors.New("malformed state")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	gotMAC, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode mac: %w", err)
	}
	mac := hmac.New(sha256.New, key[:])
	mac.Write(payload)
	if !hmac.Equal(gotMAC, mac.Sum(nil)) {
		return nil, errors.New("mac mismatch")
	}
	var c connectState
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	if timeNow().Unix() > c.ExpiresAt {
		return nil, errors.New("expired")
	}
	return &c, nil
}
