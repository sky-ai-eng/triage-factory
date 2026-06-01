package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
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
// GET /user, records the login, and discards the token — there's no per-user
// API consumer yet, so nothing is persisted beyond the identity row.
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

// resolveGitHubOrigin returns the validated scheme://host the org's GitHub
// lives on. This is the canonical key user_github_identities rows are stored
// under (and read back by) for that org: ResolveBaseURL maps an empty config
// to github.com, and gitHubWebOrigin both validates the result and strips it
// to a bare origin. Connect (the writer) and the identity-status gate (the
// reader) both route through here so the (user, host) key always agrees.
func resolveGitHubOrigin(orgBase string) (string, bool) {
	return gitHubWebOrigin(ghclient.ResolveBaseURL(orgBase))
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
	orgID, userID, ok := s.requireOrgMember(w, r)
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

	origin, okHost := resolveGitHubOrigin(orgSet.GitHubBaseURL)
	if !okHost {
		log.Printf("[github-connect] invalid github base url for org %s", orgID)
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

	// Only the CSRF nonce travels through GitHub; the orgID/userID/return_to
	// stay in the signed cookie, off GitHub's logs and the URL bar.
	q := url.Values{}
	q.Set("client_id", app.ClientID)
	q.Set("redirect_uri", s.connectCallbackURL(orgID))
	q.Set("state", st.CSRF)
	target := origin + "/login/oauth/authorize?" + q.Encode()

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
	orgID, userID, ok := s.requireOrgMember(w, r)
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
		log.Printf("[github-connect] state cookie: %v", err)
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
		log.Printf("[github-connect] github error=%s org=%s", ghErr, orgID)
		s.redirectConnect(w, r, orgID, returnTo, "denied")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		s.redirectConnect(w, r, orgID, returnTo, "connect_failed")
		return
	}

	// Read App credentials + host. The client_secret lives in Vault; the Get
	// runs under the claims tx so the multi-mode vault wrapper can decrypt.
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

	origin, okHost := resolveGitHubOrigin(orgSet.GitHubBaseURL)
	if !okHost {
		s.redirectConnect(w, r, orgID, returnTo, "bad_host")
		return
	}

	// Exchange the code for a user-to-server token, then whoami. The token is
	// used only for this GET /user and never stored — identity binding, not
	// an API consumer. A network failure on either call surfaces as
	// host_unreachable so the FE shows the infra state, not "you didn't
	// connect."
	token, err := auth.ExchangeGitHubOAuthCode(r.Context(), origin, app.ClientID, clientSecret, code, s.connectCallbackURL(orgID))
	if err != nil {
		log.Printf("[github-connect] token exchange org=%s: %v", orgID, err)
		s.redirectConnect(w, r, orgID, returnTo, connectErrCode(err))
		return
	}
	ghUser, err := auth.ValidateGitHub(origin, token)
	if err != nil {
		log.Printf("[github-connect] whoami org=%s: %v", orgID, err)
		s.redirectConnect(w, r, orgID, returnTo, connectErrCode(err))
		return
	}
	if ghUser.Login == "" {
		s.redirectConnect(w, r, orgID, returnTo, "connect_failed")
		return
	}

	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return tx.Users.UpsertGitHubIdentity(r.Context(), userID, origin, ghUser.Login, "connect_oauth")
	}); err != nil {
		internalError(w, "github-connect", err)
		return
	}

	log.Printf("[github-connect] bound user=%s login=%s host=%s org=%s source=connect_oauth",
		userID, ghUser.Login, origin, orgID)
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
// GET /api/orgs/{org_id}/identity/github
func (s *Server) handleGitHubIdentityStatus(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.requireOrgMember(w, r)
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
		origin, okHost := resolveGitHubOrigin(orgSet.GitHubBaseURL)
		if okHost {
			host = origin
			login, lerr = tx.Users.GetGitHubLogin(r.Context(), userID, origin)
			if lerr != nil {
				return lerr
			}
		} else {
			// Malformed host config — surface the raw value for display but
			// report not-connected (we can't key a lookup off a bad host).
			host = ghclient.ResolveBaseURL(orgSet.GitHubBaseURL)
		}
		app, lerr := tx.GitHubApps.GetForOrg(r.Context(), orgID)
		if lerr != nil {
			return lerr
		}
		connectAvailable = app != nil && app.ClientID != ""
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
