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
//   - GoTrue "Sign in with GitHub" is *login* — it mints a TF session, and
//     is structurally pinned to github.com (the social provider can't target
//     GHES, and under Entra SAML there's no GitHub leg at all).
//   - This handler is *identity* — a one-time consent that answers whoami
//     against the org's actual host (github.com OR a GHES host), writing
//     user_github_identities with source='connect_oauth'.
//
// It reuses the org App's stored client_id/secret (the App's user-to-server
// OAuth credentials), exchanges the code for a `ghu_` user token, calls
// GET /user plus /user/emails, records the identity, and discards the token —
// there's no per-user API consumer yet, so nothing is persisted beyond the
// identity row.
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
// factory/me/settings readers key on: orgSet.GitHubBaseURL (the store trims a
// trailing slash via NormalizeGitHubHost), with an empty config resolving to
// github.com exactly as the login_claim writer does. Connect (writer) and the
// identity-status gate (reader) both route through here, so the (user, host)
// key always agrees with the other sites — crucially including a base URL
// that carries a path, which a bare-origin derivation would silently strip
// (writing/reading a different key than everyone else).
//
// gitHubWebOrigin is used only to *validate* the configured base URL is a real
// http(s) origin; its path-stripped output is intentionally discarded so the
// returned value matches what the other writers/readers store. ok=false on a
// malformed base URL.
func resolveGitHubHost(orgBase string) (string, bool) {
	host := ghbase.ResolveBaseURL(orgBase)
	if _, ok := gitHubWebOrigin(host); !ok {
		return "", false
	}
	return host, true
}

// orgGitHubHost fetches the org's configured base URL and resolves it to that
// same canonical host, for callers that hold only an orgID. It is the fetching
// sibling of resolveGitHubHost and lands on the identical string for any base
// URL that resolveGitHubHost accepts — db.EffectiveGitHubHost applies the same
// two rules (trim trailing slashes; an unset setting is the deployment default), and
// is what the poller and the routing subscribers already key on.
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

// handleGitHubConnectStart kicks off the user-to-server OAuth dance:
// redirect the browser to {github_base_url}/login/oauth/authorize with the
// org App's client_id and an HMAC-signed CSRF state. Deliberately targets the
// org's host, NOT github.com — the whole point is binding the correct GHES
// login for a GHES org. Any org member may bind their own identity.
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
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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

	// Connect reuses the App's client_id, so it's only available once an App
	// is registered. Bounce back to the gate page with a code the FE turns
	// into "your admin needs to finish GitHub setup" rather than dead-ending.
	if app == nil || app.ClientID == "" {
		s.redirectConnect(w, r, orgID, returnTo, "no_app")
		return
	}

	ghWeb, okHost := resolveGitHubHost(orgSet.GitHubBaseURL)
	if !okHost {
		githubConnectLog.Warn("invalid github base url", "org", orgID)
		s.redirectConnect(w, r, orgID, returnTo, "bad_host")
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
	http.SetCookie(w, &http.Cookie{
		Name:     connectStateCookieName,
		Value:    signed,
		Path:     connectStatePath(orgID),
		HttpOnly: true,
		Secure:   s.cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   600,
	})

	// No `scope`: this is a GitHub *App* user-to-server flow, where the token's
	// capabilities come from the App's configured permissions, not an OAuth
	// scope string (the OAuth-App model). This flow calls /user/emails, so the
	// manifest grants read access to Email addresses. Only the CSRF nonce travels through GitHub; the
	// orgID/userID/return_to stay in the signed cookie, off GitHub's logs and
	// the URL bar.
	q := url.Values{}
	q.Set("client_id", app.ClientID)
	q.Set("redirect_uri", s.connectCallbackURL(orgID))
	q.Set("state", st.CSRF)
	target := ghWeb + "/login/oauth/authorize?" + q.Encode()

	http.Redirect(w, r, target, http.StatusFound)
}

// handleGitHubConnectCallback completes the dance: validate state/CSRF,
// exchange the code for a `ghu_` token, GET /user against the org's host,
// upsert user_github_identities(source='connect_oauth'), discard the token.
// On any failure it redirects back to the gate page with a distinguishing
// error code (host_unreachable vs connect_failed vs denied) rather than
// dead-ending a top-level navigation in a JSON error.
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
	s.clearConnectCookie(w, r, orgID)
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

	returnTo := normalizeReturnTo(cs.ReturnTo)
	if returnTo == "/" {
		returnTo = "/orgs/" + orgID
	}

	// User denied consent (or GitHub reported another OAuth error).
	if ghErr := r.URL.Query().Get("error"); ghErr != "" {
		githubConnectLog.Warn("github oauth error", "github_error", ghErr, "org", orgID)
		s.redirectConnect(w, r, orgID, returnTo, "denied")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		s.redirectConnect(w, r, orgID, returnTo, "connect_failed")
		return
	}

	// Read App credentials + host. The client_secret lives in the SecretStore;
	// the Get runs under the claims tx so the multi-mode org_secrets read is
	// claims-gated (RLS) before TF decrypts it app-side.
	var app *domain.OrgGitHubApp
	var clientSecret string
	var orgSet domain.OrgSettings
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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

	// Exchange the code for a user-to-server token, then whoami. The token is
	// used only for this GET /user and never stored — identity binding, not
	// an API consumer. A network failure on either call surfaces as
	// host_unreachable so the FE shows the infra state, not "you didn't
	// connect."
	token, err := auth.ExchangeGitHubOAuthCode(r.Context(), ghWeb, app.ClientID, clientSecret, code, s.connectCallbackURL(orgID))
	if err != nil {
		githubConnectLog.Warn("token exchange failed", "org", orgID, "error", err)
		s.redirectConnect(w, r, orgID, returnTo, connectErrCode(err))
		return
	}
	ghUser, err := auth.CaptureGitHubIdentity(r.Context(), ghWeb, token)
	if err != nil {
		githubConnectLog.Warn("whoami failed", "org", orgID, "error", err)
		s.redirectConnect(w, r, orgID, returnTo, connectErrCode(err))
		return
	}
	if ghUser.Login == "" {
		s.redirectConnect(w, r, orgID, returnTo, "connect_failed")
		return
	}

	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if err := tx.Users.UpsertGitHubIdentity(r.Context(), userID, ghWeb, ghUser.Login, ghUser.UserID(), ghUser.PrimaryEmail, "connect_oauth"); err != nil {
			return err
		}
		return recordGitHubIdentityBind(r.Context(), tx, orgID, userID, ghWeb, ghUser.Login)
	}); err != nil {
		internalError(w, "github-connect", err)
		return
	}

	githubConnectLog.Info("bound user",
		"user", userID, "login", ghUser.Login, "host", ghWeb, "org", orgID, "source", "connect_oauth")
	// Seed this login's trailing-window PR history so the personal dashboard
	// isn't blank for activity that predates tracking (TFAC-396). Fire-and-
	// forget + marker-guarded; multi-mode only.
	s.kickDashboardBackfill(orgID, userID, ghUser.Login, ghWeb)
	http.Redirect(w, r, returnTo, http.StatusFound)
}

// githubIdentityStatusResponse is the read-only shape the onboarding gate
// polls to decide whether to block. `connected` is the single load-bearing
// bit; `connect_available` lets the gate page choose between offering the
// Connect button and telling the user their admin must register an App first.
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
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		orgSet, lerr := tx.Orgs.GetSettings(r.Context(), orgID)
		if lerr != nil {
			return lerr
		}
		ghWeb, okHost := resolveGitHubHost(orgSet.GitHubBaseURL)
		if okHost {
			host = ghWeb
			login, lerr = tx.Users.GetGitHubLogin(r.Context(), userID, ghWeb)
			if lerr != nil {
				return lerr
			}
		} else {
			// Malformed host config — surface the raw value for display but
			// report not-connected (we can't key a lookup off a bad host).
			host = ghbase.ResolveBaseURL(orgSet.GitHubBaseURL)
		}
		app, lerr := tx.GitHubApps.GetForOrg(r.Context(), orgID)
		if lerr != nil {
			return lerr
		}
		// Connect (the user-to-server OAuth code exchange) needs BOTH the App's
		// client_id AND a stored client_secret — the exchange at :259 already
		// requires the secret. An imported App may have no secret
		// (the BYOA owner often skips it), so requiring only client_id here would
		// advertise an OAuth flow that can't complete; the gate page would offer
		// Connect, the user would consent, and the callback would dead-end on the
		// missing secret. Requiring the ref too makes the gate fall back to the
		// always-available PAT-capture identity path instead.
		connectAvailable = app != nil && app.ClientID != "" && app.ClientSecretRef != ""
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
// numeric id that binding records together — WITHOUT storing the token. It is
// the shared validation core of the PAT-capture HTTP handler and the headless
// bootstrap; each caller writes user_github_identities in its own tx (the tx
// context differs). Errors wrap auth.CaptureGitHubIdentity's (callers can
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
// and stored independently). It's the always-available fallback to Connect, and
// the only capture path when no GitHub App is registered.
//
// POST /api/orgs/{org_id}/github/identity/pat
func (s *Server) handleGitHubIdentityPAT(w http.ResponseWriter, r *http.Request) {
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
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var lerr error
		orgSet, lerr = tx.Orgs.GetSettings(r.Context(), orgID)
		return lerr
	}); err != nil {
		internalError(w, "github-identity", err)
		return
	}
	ghWeb, okHost := resolveGitHubHost(orgSet.GitHubBaseURL)
	if !okHost {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonNotConfigured, Message: "Your workspace's GitHub URL looks misconfigured. Ask your admin to fix it in Workspace Settings.", Field: "github_pat"})
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
func (s *Server) redirectConnect(w http.ResponseWriter, r *http.Request, orgID, returnTo, errCode string) {
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

// clearConnectCookie expires the state cookie. Its Secure/SameSite/Path must
// match the SetCookie in the start handler or the browser may keep a copy.
func (s *Server) clearConnectCookie(w http.ResponseWriter, r *http.Request, orgID string) {
	http.SetCookie(w, &http.Cookie{
		Name:     connectStateCookieName,
		Value:    "",
		Path:     connectStatePath(orgID),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
	})
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
