package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	tfdb "github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/sessions"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// tfdb is aliased (rather than imported as `db`) to avoid colliding
// with `db *sql.DB` parameter names inside this file.
var _ = tfdb.Claims{}

// gotrueHTTPClient is the shared client for server-to-server calls to
// GoTrue (/token exchange, /token refresh, /logout). Bounded timeout so
// a hung or slow GoTrue can't tie up a user-facing request indefinitely
// — http.DefaultClient has no timeout, which is the exact wrong default
// when the upstream is on the request critical path.
//
// 30s is generous: real /token calls complete in well under a second.
// The request context still cancels earlier on client disconnect; this
// is the upper bound when the context happens to be Background (e.g.
// reaper, future async revoke).
var gotrueHTTPClient = &http.Client{Timeout: 30 * time.Second}

// gotrueSSOClient drives the SP-initiated SAML start (POST /sso). Unlike
// gotrueHTTPClient it must NOT follow redirects: GoTrue answers /sso with a 303
// whose Location is the signed SAML AuthnRequest URL at the IdP, and TF forwards
// that Location to the browser rather than fetching the IdP server-side.
// http.ErrUseLastResponse makes Do() return the 303 itself instead of chasing
// it (which would have TF GET login.microsoftonline.com from inside the
// network). Same bounded timeout rationale as gotrueHTTPClient.
var gotrueSSOClient = &http.Client{
	Timeout: 30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// deployConfig holds deployment-identity configuration that is
// meaningful in both local and multi mode. Populated at boot via
// SetDeployConfig (local mode) or SetAuthDeps (multi mode, which
// calls SetDeployConfig internally).
type deployConfig struct {
	// publicURL is the externally-visible base for the TF deployment
	// (e.g. "https://app.triagefactory.com" in multi,
	// "http://localhost:3000" in local).
	publicURL string

	// hmacKey signs short-lived tokens (OAuth state cookies, manifest
	// registration state). Loaded from TF_COOKIE_SECRET in multi mode;
	// generated randomly at boot in local mode.
	hmacKey [32]byte

	// secureCookies hardens cookie attributes when the deployment is
	// HTTPS. Derived from publicURL at init time.
	secureCookies bool

	// trustDevFrontendOrigin additionally trusts devFrontendOrigin (see
	// middleware.go) in withCSRFOriginCheck. Set only via
	// AllowDevFrontendOrigin, which only local-mode boot calls — never
	// wired into multi-mode's deploy config.
	trustDevFrontendOrigin bool
}

// authConfig holds GoTrue-only configuration for the multi-mode auth
// flow. Nil in local mode.
type authConfig struct {
	// gotrueURL is the in-network base URL (e.g. http://gotrue:9999)
	// for server-side calls — token exchange, refresh, etc.
	gotrueURL string
}

// SetDeployConfig wires deployment-identity configuration that is
// meaningful in both modes. Local-mode boot calls this directly;
// multi-mode boot calls SetAuthDeps, which delegates here internally.
func (s *Server) SetDeployConfig(publicURL string, hmacKey [32]byte) {
	pub := strings.TrimRight(publicURL, "/")
	s.deployCfg = &deployConfig{
		publicURL:     pub,
		hmacKey:       hmacKey,
		secureCookies: strings.HasPrefix(pub, "https://"),
	}
}

// AllowDevFrontendOrigin additionally trusts devFrontendOrigin (Vite's
// dev server — see middleware.go) in withCSRFOriginCheck. Local-mode
// boot only: `cd frontend && pnpm run dev` serves the UI from port 5173
// while the backend listens on publicURL's port (3000 by default), so a
// mutating request proxied through Vite arrives with an Origin that
// legitimately differs from publicURL. Call only after SetDeployConfig;
// a no-op if deployCfg isn't set yet.
func (s *Server) AllowDevFrontendOrigin() {
	if s.deployCfg == nil {
		return
	}
	s.deployCfg.trustDevFrontendOrigin = true
}

// SetAuthDeps wires the multi-mode auth dependencies into the server.
// Local mode never calls this; multi-mode boot calls it once after
// constructing the verifier + session store and before ListenAndServe.
//
// Also builds the /auth/v1/* reverse proxy and spawns the session
// reaper goroutine. The goroutine's lifetime is bound to ctx — pass
// the server's shutdown context so reaping exits cleanly. Tests pass
// a context with t.Cleanup-bound cancel to avoid leaking goroutines.
func (s *Server) SetAuthDeps(
	ctx context.Context,
	verifier *verify.Verifier,
	sessionStore *sessions.Store,
	gotrueURL, publicURL string,
	cookieSecret [32]byte,
) error {
	s.SetDeployConfig(publicURL, cookieSecret)

	gtURL := strings.TrimRight(gotrueURL, "/")
	cfg := &authConfig{
		gotrueURL: gtURL,
	}

	proxy, err := newGotrueProxy(gtURL)
	if err != nil {
		return fmt.Errorf("gotrue proxy: %w", err)
	}

	s.authCfg = cfg
	s.authProxy = proxy
	s.authDeps = &authDeps{
		verifier:       verifier,
		sessions:       sessionStore,
		gotrueRefresh:  s.gotrueRefreshFunc(cfg),
		gotrueExchange: s.gotrueExchangeFunc(cfg),
		gotrueSSO:      s.gotrueSSOFunc(cfg),
		gotrueLogout:   s.gotrueLogoutFunc(cfg),
	}

	// Spawn the session reaper. Cadence + retention follow the arch
	// doc (10-minute tick, 30-day retention window); the goroutine
	// exits when ctx is cancelled, so server shutdown / test cleanup
	// drains it without further wiring.
	go sessionStore.RunReaper(ctx, 10*time.Minute, 30*24*time.Hour)

	return nil
}

// handleOAuthStart begins a login flow. Both providers share one PKCE
// state cookie — the CSRF token, the PKCE code_verifier, the return_to
// path (and, for SAML, the provider_id) ride through the roundtrip so the
// callback completes the dance with no per-flow database row.
//
// PKCE (RFC 7636) means tokens never traverse the URL bar — gotrue
// hands back an opaque `code`, which the callback exchanges via a
// server-to-server POST. Tokens stay off referer headers, server
// access logs, and browser history.
//
//   - github → browser-redirect to gotrue's /authorize (the OAuth dance).
//   - saml   → StartSSO: server-side POST to gotrue's public /sso,
//     forwarding the 303 to the IdP (the SP-initiated SAML dance).
//
// GET /api/auth/oauth/{provider}?return_to=/some/path
// GET /api/auth/oauth/saml?provider_id=<gotrue-uuid>&return_to=/some/path
func (s *Server) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	if s.authDeps == nil {
		// The whole hosted-auth surface is absent in local mode: a route that
		// doesn't exist in this deployment mode answers 404.
		notFound(w, "route")
		return
	}
	provider := r.PathValue("provider")
	returnTo := normalizeReturnTo(r.URL.Query().Get("return_to"))

	switch provider {
	case "github":
		codeVerifier, csrf, ok := s.issueOAuthStateCookie(w, r, returnTo, "", false)
		if !ok {
			return
		}
		q := url.Values{}
		q.Set("provider", provider)
		// PKCE wiring: gotrue accepts code_challenge + code_challenge_method
		// and flow_type=pkce on /authorize. After the provider dance, gotrue
		// redirects to redirect_to with ?code=<authcode>, which the callback
		// trades for tokens via /token?grant_type=pkce.
		q.Set("code_challenge", pkceChallenge(codeVerifier))
		q.Set("code_challenge_method", "S256")
		q.Set("flow_type", "pkce")
		q.Set("redirect_to", s.deployCfg.publicURL+"/api/auth/callback?state="+csrf)
		target := s.deployCfg.publicURL + "/auth/v1/authorize?" + q.Encode()
		http.Redirect(w, r, target, http.StatusFound)
	default:
		// Non-"github" providers (e.g. "saml") are handled by the registered
		// login extension (Enterprise Edition). Absent one — community build or
		// unlicensed deployment — the provider is genuinely unsupported.
		if s.loginExtension().StartSSO(w, r, provider, returnTo) {
			return
		}
		notFound(w, "provider")
	}
}

// issueOAuthStateCookie generates a fresh CSRF token + PKCE verifier, signs
// them into the HMAC state cookie (with the optional SSO provider_id and the
// verify-before-enforce test flag), and sets it. Returns the verifier (to
// derive the code_challenge) and the CSRF token (to stitch into redirect_to).
// On a crypto/marshal failure it writes a 500 and returns ok=false — the caller
// just returns. Shared by the GitHub and SAML start paths (test=false) and the
// SSO test-start path (test=true) so the state-cookie contract is defined once.
func (s *Server) issueOAuthStateCookie(w http.ResponseWriter, r *http.Request, returnTo, providerID string, test bool) (codeVerifier, csrf string, ok bool) {
	csrfRaw := make([]byte, 16)
	if _, err := rand.Read(csrfRaw); err != nil {
		internalError(w, "auth", fmt.Errorf("read csrf entropy: %w", err))
		return "", "", false
	}
	codeVerifier, err := generatePKCEVerifier()
	if err != nil {
		internalError(w, "auth", fmt.Errorf("generate pkce verifier: %w", err))
		return "", "", false
	}
	csrf = hex.EncodeToString(csrfRaw)
	state := stateClaims{
		ReturnTo:     returnTo,
		CSRF:         csrf,
		CodeVerifier: codeVerifier,
		ProviderID:   providerID,
		Test:         test,
		ExpiresAt:    timeNow().Add(10 * time.Minute).Unix(),
	}
	signed, err := state.sign(s.deployCfg.hmacKey)
	if err != nil {
		internalError(w, "auth", fmt.Errorf("sign oauth state: %w", err))
		return "", "", false
	}
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    signed,
		Path:     "/api/auth/",
		HttpOnly: true,
		Secure:   s.cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   600,
	})
	return codeVerifier, csrf, true
}

// redirectLoginError bounces a recoverable callback failure to the /login page
// as ?error=<code> — a 302 the SPA renders as a friendly banner, never a bare
// 4xx. A meaningful returnTo (anything but the root) is preserved so the user
// lands back where they were headed once they retry. The single place the
// callback shapes these redirects, shared by the provider-error and seat-limit
// branches.
func (s *Server) redirectLoginError(w http.ResponseWriter, r *http.Request, code, returnTo string) {
	q := url.Values{"error": {code}}
	if returnTo != "" && returnTo != "/" {
		q.Set("return_to", returnTo)
	}
	http.Redirect(w, r, "/login?"+q.Encode(), http.StatusFound)
}

// handleOAuthCallback completes the PKCE dance: validates state,
// exchanges the auth code via server-side POST to gotrue's /token,
// verifies the returned JWT, upserts public.users, creates an
// encrypted session, sets the sid cookie, and redirects to return_to.
//
// GET /api/auth/callback?state=<csrf>&code=<auth_code>
func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if s.authDeps == nil {
		// The whole hosted-auth surface is absent in local mode: a route that
		// doesn't exist in this deployment mode answers 404.
		notFound(w, "route")
		return
	}

	stateCookie, err := r.Cookie(stateCookieName)
	if err != nil {
		// No state cookie yet → method unknown (state not parsed).
		s.recordLoginFailure(r, "missing_state_cookie", "", "")
		badRequest(w, "missing state cookie")
		return
	}
	state, err := parseStateCookie(stateCookie.Value, s.deployCfg.hmacKey)
	if err != nil {
		authLog.Warn("state cookie parse failed", "error", err)
		s.recordLoginFailure(r, "state_parse_failed", "", "")
		badRequest(w, "invalid state")
		return
	}
	if r.URL.Query().Get("state") != state.CSRF {
		s.recordLoginFailure(r, "state_mismatch", authMethod(state.ProviderID != ""), "")
		badRequest(w, "state mismatch")
		return
	}

	// State done — clear the cookie. (Cookie's secure-flag derivation
	// must match the original SetCookie or the browser may keep both;
	// see s.cookieSecure for the per-request resolution.)
	http.SetCookie(w, &http.Cookie{
		Name: stateCookieName, Value: "", Path: "/api/auth/",
		MaxAge: -1, HttpOnly: true, Secure: s.cookieSecure(r), SameSite: http.SameSiteLaxMode,
	})

	// Verify-before-enforce SSO test (TFAC-431): the admin-gated test-start set
	// Test in TF's signed state, so this round-trip is a not-yet-enabled
	// connection's "does my IdP actually work?" check, NOT a login. Complete the
	// exchange + verify to prove the assertion is valid and readable, then render
	// pass/fail — OnTestCallback short-circuits before any writes (no
	// principal, no JIT, no session), so a test mints nothing. Branch here, before
	// authCode is even read, because the test path handles a GoTrue ?error= ACS
	// rejection as a fail result rather than the plain 400 a real login returns.
	// Verify-before-enforce SSO test: when TF's signed state carries Test, the
	// login extension completes the exchange/verify and renders a pass/fail
	// page (no principal, no JIT, no session). No extension / not a test → false,
	// and a normal login continues. Branch here, before authCode is read,
	// because the test path handles a GoTrue ?error= ACS rejection itself.
	if s.loginExtension().OnTestCallback(w, r, toLoginState(state)) {
		return
	}

	// A provider-side OAuth error — GoTrue relays the IdP's ?error=… back to this
	// callback instead of a code — is a login failure to recover from, not a
	// malformed request. The two shapes worth distinguishing for the user are the
	// person declining consent on the provider's screen (access_denied) and a
	// transient provider/GoTrue fault (server_error / unexpected_failure — e.g. an
	// upstream GitHub incident that blocks the profile fetch, which surfaces here
	// as an empty code). Bounce to /login with a friendly, category-specific
	// banner rather than the bare "missing code" 400 the empty-code branch below
	// would emit. The SSO verify-before-enforce test path (OnTestCallback, above)
	// already consumed its own ?error=, so this only reaches real logins.
	if provErr := strings.TrimSpace(r.URL.Query().Get("error")); provErr != "" {
		method := authMethod(state.ProviderID != "")
		cancelled := provErr == "access_denied"
		reason := "provider_error"
		loginError := "login_failed"
		if cancelled {
			reason = "provider_denied"
			loginError = "login_cancelled"
		}
		authLog.Warn("oauth provider error on callback",
			"provider_error", provErr,
			"error_code", r.URL.Query().Get("error_code"),
			"error_description", r.URL.Query().Get("error_description"),
			"method", method)
		s.recordLoginFailure(r, reason, method, "")
		s.redirectLoginError(w, r, loginError, state.ReturnTo)
		return
	}

	authCode := r.URL.Query().Get("code")
	if authCode == "" {
		s.recordLoginFailure(r, "missing_code", authMethod(state.ProviderID != ""), "")
		badRequest(w, "missing code")
		return
	}

	// Server-side code exchange. gotrue verifies code_verifier matches
	// the code_challenge it stored against the auth code; if not, this
	// returns an error before we ever see tokens. After this call the
	// access_token + refresh_token exist only in this handler's memory
	// and in the encrypted session row — never on the URL bar.
	accessToken, refreshToken, _, err := s.authDeps.gotrueExchange(r.Context(), authCode, state.CodeVerifier)
	if err != nil {
		authLog.Warn("pkce exchange failed", "error", err)
		s.recordLoginFailure(r, "exchange_failed", authMethod(state.ProviderID != ""), "")
		badRequest(w, "token exchange failed")
		return
	}

	claims, err := s.authDeps.verifier.Verify(accessToken)
	if err != nil {
		authLog.Warn("verify callback jwt failed", "error", err)
		s.recordLoginFailure(r, "verify_failed", authMethod(state.ProviderID != ""), "")
		writeUnauth(w)
		return
	}

	authUserID, err := uuid.Parse(claims.Subject)
	if err != nil {
		// claims verified, so the email is known even though the sub is malformed.
		s.recordLoginFailure(r, "bad_sub", authMethod(state.ProviderID != ""), claims.Email)
		badRequest(w, "invalid sub")
		return
	}

	// The provider_id in TF's OWN signed state (set by StartSSO) is the
	// authoritative "this login is SSO" signal. We deliberately do NOT read the
	// GoTrue JWT's provider / app_metadata / amr to detect SSO or recover the
	// provider — those are GoTrue-internal and version-fragile.
	isSSO := state.ProviderID != ""

	// Resolve the GoTrue login identity to its PRINCIPAL (the public.users row).
	// A new identity links to an existing principal when its verified email
	// matches, else mints one — GoTrue keeps each SAML login as its own
	// auth.users (it excludes SSO from account linking), so TF unifies a human's
	// identities here. From this point userUUID is the principal: memberships,
	// JIT, and the session all key on the human, not the per-provider identity.
	userUUID, err := resolveOrCreatePrincipal(r.Context(), s.db, authUserID, claims, isSSO)
	if err != nil {
		s.recordLoginFailure(r, "principal_resolve_failed", authMethod(isSSO), claims.Email)
		internalError(w, "auth", fmt.Errorf("resolve principal %s: %w", authUserID, err))
		return
	}

	// SSO enforcement + JIT provisioning. The login extension folds the two
	// SSO forks that run after principal resolution but before the session is
	// minted:
	//   - enforcement: a non-SSO (GitHub) login whose VERIFIED email is on an
	//     org's enforced verified domain is rejected (done=true → it wrote a
	//     redirect), unless the principal is break-glass.
	//   - JIT: an SSO login is provisioned into its provider's org, returned as
	//     activeOrg so the session defaults there.
	// No extension (community / unlicensed) → activeOrg invalid, done=false: a
	// plain login with no enforcement or JIT. This runs AFTER
	// resolveOrCreatePrincipal so a rejected first-time user still gets a
	// durable principal + identity row; enforcement blocks only SESSION creation.
	defaultOrg, done, err := s.loginExtension().OnLoginResolved(w, r, LoginResolved{
		State:         toLoginState(state),
		UserID:        userUUID,
		Email:         claims.Email,
		EmailVerified: claims.EmailVerified,
		IsSSO:         isSSO,
	})
	if err != nil {
		internalError(w, "sso", err)
		return
	}
	if done {
		return
	}

	// Per-seat license enforcement (TF_LICENSE maxSeats). Runs after principal
	// resolution + SSO enforcement/JIT, before the session is minted: a new
	// distinct active user beyond the committed cap for this billing period is
	// denied a session — the same block-before-CreateSystem posture SSO domain
	// enforcement uses. Uncapped / unlicensed → no-op. A blocked login records
	// no login_success, so it never consumes a seat.
	if s.enforceSeatLimit(r, userUUID.String()) {
		s.redirectLoginError(w, r, "seat_limit", state.ReturnTo)
		return
	}

	// Non-SSO login (or SSO with no active binding): default the session to the
	// user's earliest existing membership, or NULL for a first-time user, who
	// lands at the zero-membership onboarding entry. Signup provisions nothing —
	// org creation is a deliberate user action (the onboarding "Start your
	// Factory" CTA → the create-org flow), so the callback never mints a tenant.
	// Whether that screen's create affordance is enabled is governed separately
	// by runmode.OrgCreationEnabled() (TF_PREVENT_ORG_CREATION).
	if !defaultOrg.Valid {
		defaultOrg, err = s.lookupEarliestMembership(r.Context(), s.db, userUUID)
		if err != nil {
			internalError(w, "auth", fmt.Errorf("resolve active org for %s: %w", userUUID, err))
			return
		}
	}

	// Trust the JWT's exp claim. The exchange response also carries
	// expires_in, but the signed claim is authoritative and the
	// closure already returns it via Verifier.Claims.ExpiresAt.
	jwtExp := claims.ExpiresAt

	sessExp := timeNow().Add(30 * 24 * time.Hour)
	sess, err := s.authDeps.sessions.CreateSystem(r.Context(), userUUID,
		accessToken, refreshToken, jwtExp, sessExp,
		r.UserAgent(), clientIP(r), defaultOrg,
	)
	if err != nil {
		internalError(w, "auth", fmt.Errorf("create session: %w", err))
		return
	}

	// Session minted → the durable login_success record. defaultOrg is nullable
	// (a zero-membership first login lands with NULL active org).
	s.recordLoginSuccess(r, userUUID, defaultOrg, sess.ID, isSSO)

	http.SetCookie(w, &http.Cookie{
		Name:     s.sidCookieName(),
		Value:    sess.ID.String(),
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((30 * 24 * time.Hour).Seconds()),
	})

	http.Redirect(w, r, state.ReturnTo, http.StatusFound)
}

// handleLogout invalidates the session both upstream (gotrue) and
// locally (sessions row). Idempotent — repeated logouts don't 4xx.
//
// The gotrue call is best-effort: if it fails (network blip, gotrue
// down), we still revoke locally and clear the cookie. Worst-case
// outcome is an upstream refresh-token that lives ~30 days but has
// no client to redeem it from — since the encrypted blob is gone
// from public.sessions, an attacker would need both the master key
// AND the gotrue session to exploit it.
//
// Wrapped at mount time in withCSRFOriginCheck; same-origin POSTs
// only. SameSite=Lax alone doesn't block cross-site form POSTs.
//
// POST /api/auth/logout
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s.authDeps == nil {
		// The whole hosted-auth surface is absent in local mode: a route that
		// doesn't exist in this deployment mode answers 404.
		notFound(w, "route")
		return
	}
	cookie, err := r.Cookie(s.sidCookieName())
	if err == nil {
		if sid, perr := uuid.Parse(cookie.Value); perr == nil {
			// Look up the session so we have the access token for
			// the upstream call (and the user id for the socket kick
			// below). Lookup ignores revoked rows, so a double-logout
			// naturally no-ops here — sess is nil and we skip both.
			sess, lerr := s.authDeps.sessions.LookupSystem(r.Context(), sid)
			if lerr == nil && sess != nil {
				if uerr := s.authDeps.gotrueLogout(r.Context(), sess.JWT); uerr != nil {
					authLog.Warn("upstream logout failed", "error", uerr)
					// Continue — local revoke still happens.
				}
			}
			if rerr := s.authDeps.sessions.RevokeSystem(r.Context(), sid); rerr != nil {
				authLog.Warn("revoke session failed", "error", rerr)
			}
			// Actively close this session's live websocket connections
			// (TFAC-75) so the event stream stops on revoke instead of
			// lingering until the socket drops. Scoped to this sid so the
			// user's other sessions (other devices) keep their sockets.
			// Logged for the revocation audit trail (SOC2).
			if sess != nil {
				n := s.ws.CloseUserConnections(sess.UserID.String(), sid.String(), "",
					websocket.CloseSessionRevoked, "session revoked")
				authLog.Info("kicked ws connections on logout",
					"user", sess.UserID, "sid", sessions.LogID(sid),
					"code", int(websocket.CloseSessionRevoked), "n", n)
				// Cross-pod (TFAC-584): this pod only just closed ITS OWN
				// matching local sockets above — the same session's
				// connections on another control pod need the same close,
				// which only a fleet-wide publish can reach. No-op when
				// wsBackplane is nil (local mode).
				if s.wsBackplane != nil {
					s.wsBackplane.PublishKick(r.Context(), sess.UserID.String(), sid.String(), "",
						int(websocket.CloseSessionRevoked), "session revoked")
				}
				// Durable logout record (best-effort) — only when a live session
				// was actually revoked (a double-logout no-ops with sess == nil).
				// The session's active org scopes the event to the tenant.
				s.recordLogout(r, sess.UserID, sid, sess.ActiveOrgID)
			}
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: s.sidCookieName(), Value: "", Path: "/",
		MaxAge: -1, HttpOnly: true, Secure: s.cookieSecure(r), SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

// handleLogoutAll revokes every active session for the caller and
// best-effort invalidates each one upstream at gotrue. Use case: "I
// think my account is compromised, kill everything." Cookie on the
// current response is cleared too — the caller is effectively logged
// out on this device as well as all others.
//
// Wrapped at mount time in withSession (must be authenticated) +
// withCSRFOriginCheck (same-origin only).
//
// POST /api/auth/logout/all
func (s *Server) handleLogoutAll(w http.ResponseWriter, r *http.Request) {
	if s.authDeps == nil {
		// The whole hosted-auth surface is absent in local mode: a route that
		// doesn't exist in this deployment mode answers 404.
		notFound(w, "route")
		return
	}
	claims := ClaimsFrom(r.Context())
	if claims == nil {
		writeUnauth(w)
		return
	}
	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		badRequest(w, "invalid sub")
		return
	}

	// List BEFORE revoking — we need the decrypted JWTs for upstream
	// logout calls. If we revoke first, the rows are filtered out
	// of the active-set query.
	active, err := s.authDeps.sessions.ListActiveForUserSystem(r.Context(), userID)
	if err != nil {
		internalError(w, "auth", fmt.Errorf("list active sessions for %s: %w", userID, err))
		return
	}

	// Best-effort upstream logout for each. We don't fail the request
	// on upstream errors — local revocation is the load-bearing step,
	// and the caller's expectation is "kill all my sessions" which is
	// satisfied either way. Sequential rather than parallel because
	// N is typically tiny (1-5) and gotrue's rate limits prefer it.
	for _, sess := range active {
		if uerr := s.authDeps.gotrueLogout(r.Context(), sess.JWT); uerr != nil {
			authLog.Warn("upstream logout-all session failed", "error", uerr)
		}
	}

	n, err := s.authDeps.sessions.RevokeAllForUserSystem(r.Context(), userID)
	if err != nil {
		internalError(w, "auth", fmt.Errorf("revoke all sessions for %s: %w", userID, err))
		return
	}
	authLog.Info("logout-all revoked sessions", "user", userID, "count", n)
	// Durable logout_all record (best-effort), carrying the revoked count, the
	// caller's active org, and the INITIATING session (the device that triggered
	// the panic logout) — all from the session context withSession set — so the
	// event is org-scopable and the triggering session is pinned for forensics.
	var activeOrg uuid.NullUUID
	var initiatingSID uuid.UUID
	if sess := SessionFrom(r.Context()); sess != nil {
		activeOrg = sess.ActiveOrgID
		initiatingSID = sess.ID
	}
	s.recordLogoutAll(r, userID, int(n), activeOrg, initiatingSID)

	// Every session is now revoked, so close ALL of this user's live
	// websocket connections (TFAC-75) — no sid/org filter. Each device's
	// client sees the session-revoked code and routes to /login. Logged
	// for the revocation audit trail (SOC2).
	kicked := s.ws.CloseUserConnections(userID.String(), "", "",
		websocket.CloseSessionRevoked, "session revoked")
	authLog.Info("kicked ws connections on logout-all",
		"user", userID, "code", int(websocket.CloseSessionRevoked), "n", kicked)
	// Cross-pod (TFAC-584): see handleLogout's identical comment above.
	if s.wsBackplane != nil {
		s.wsBackplane.PublishKick(r.Context(), userID.String(), "", "",
			int(websocket.CloseSessionRevoked), "session revoked")
	}

	// Clear the cookie on this response too — the caller's current
	// session is one of the ones we just revoked.
	http.SetCookie(w, &http.Cookie{
		Name: s.sidCookieName(), Value: "", Path: "/",
		MaxAge: -1, HttpOnly: true, Secure: s.cookieSecure(r), SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

// handleActiveOrgUpdate swaps the active org for the caller's session.
// Wrapped at mount time in withCSRFOriginCheck + withSession.
//
// POST /api/me/active-org  body: { "org_id": "<uuid>" }
//
// The active-org primitive. We deliberately store the choice
// on the session row (single source of truth across tabs) rather than
// in the URL path. The next request's withSession reads the new value
// into ctxKeyOrgID; tab B sees the switch on its next round trip.
//
// 404-on-non-member mirrors the org-membership gate's posture — don't disclose
// whether the org exists to a user who isn't in it.
func (s *Server) handleActiveOrgUpdate(w http.ResponseWriter, r *http.Request) {
	if s.authDeps == nil {
		// The whole hosted-auth surface is absent in local mode: a route that
		// doesn't exist in this deployment mode answers 404.
		notFound(w, "route")
		return
	}
	claims := ClaimsFrom(r.Context())
	if claims == nil || claims.Subject == runmode.LocalDefaultUserID {
		// Sentinel-claim caller is the local-mode shim; local mode has
		// nothing to do here (the shim hardcodes the sentinel org). 401
		// matches handleMe's sentinel gate.
		writeUnauth(w)
		return
	}
	sess := SessionFrom(r.Context())
	if sess == nil {
		// withSession is responsible for setting this; absence is a
		// route-wiring bug, not a caller fault.
		authLog.Error("active-org update: no session in context, route missing withsession")
		writeUnauth(w)
		return
	}

	var body struct {
		OrgID string `json:"org_id"`
	}
	// Strict, capped decoding like every other body: the raw decoder here
	// accepted trailing junk and unknown fields.
	if !httpx.DecodeJSONStrictLimit(w, r, &body, 1<<10) {
		return
	}
	orgUUID, err := uuid.Parse(body.OrgID)
	if err != nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidField, Message: "org_id must be a valid org id", Field: "org_id",
		})
		return
	}

	ok, err := s.az.UserHasOrgAccess(r.Context(), claims.Subject, orgUUID.String())
	if err != nil {
		internalError(w, "auth", fmt.Errorf("active-org membership check %s/%s: %w", claims.Subject, orgUUID, err))
		return
	}
	if !ok {
		// 404 not 403 — same posture RequireOrgMember takes: an org the caller
		// isn't a member of must not be confirmed to exist.
		notFound(w, "org")
		return
	}

	if err := s.authDeps.sessions.UpdateActiveOrgSystem(r.Context(), sess.ID, orgUUID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Session vanished between withSession's lookup and now
			// (revoked from another tab, reaper raced in). 401 — caller
			// re-logs in.
			writeUnauth(w)
			return
		}
		internalError(w, "auth", fmt.Errorf("update active org for session %s: %w", sessions.LogID(sess.ID), err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{}"))
}

// handleMe returns the authenticated user's identity plus their org and team
// memberships, each with the role the viewer holds.
// Wrapped in withSession at mount time. Response wire shape is
// mirrored in the frontend as the canonical `MeResponse` type — the
// sole identity endpoint in both modes.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFrom(r.Context())
	if claims == nil {
		writeUnauth(w)
		return
	}
	type orgRow struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Role string `json:"role"`
	}
	type teamRow struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		OrgID string `json:"org_id"`
		Role  string `json:"role"`
	}
	type response struct {
		ID              string   `json:"id"`
		Email           string   `json:"email"`
		DisplayName     string   `json:"display_name,omitempty"`
		AvatarURL       string   `json:"avatar_url,omitempty"`
		GitHubUsername  string   `json:"github_username,omitempty"`
		JiraAccountID   string   `json:"jira_account_id,omitempty"`
		JiraDisplayName string   `json:"jira_display_name,omitempty"`
		Orgs            []orgRow `json:"orgs"`
		// Teams is the viewer's own team-membership rows, across every org
		// they belong to — hence the org tag on each, since /me spans orgs
		// where the teams list is scoped to one. Membership rows only: a team
		// an org admin can see but never joined carries no role and is not
		// here, which is the teams list's business. Archived teams are
		// excluded, matching that list. It is what makes the viewer's team
		// grants reachable without a session cursor: POST /api/teams/list
		// resolves its org from the session's active org, so a caller holding
		// no session state could not enumerate its teams at all.
		//
		// Not omitempty — [] is an answer ("no teams"), same contract as Orgs.
		Teams       []teamRow `json:"teams"`
		ActiveOrgID string    `json:"active_org_id,omitempty"`
		// OrgCreationEnabled surfaces the instance's TF_PREVENT_ORG_CREATION
		// toggle (inverted) so the frontend onboarding entry can decide
		// whether to enable the "create your org" affordance or show the
		// invite-only "ask your admin" state, without a second round trip.
		// Not omitempty — false is a meaningful value the gate must see.
		OrgCreationEnabled bool `json:"org_creation_enabled"`
		// IsOperator reports whether this identity is a deployment operator
		// (TFAC-589) — the org-less flag the fleet-console nav gate reads
		// (together with the FeatureFleet entitlement). Deployment config, not
		// tenant data, so it lives on identity here rather than a per-org role.
		// Not omitempty — false is meaningful. Local mode: the single user is
		// implicitly the operator.
		IsOperator bool `json:"is_operator"`
	}

	// Local-mode shim path: withSession injects a synthetic claim
	// carrying LocalDefaultUserID when authDeps is nil. The multi-mode
	// body below reads public.users via tf.current_user_id() — both
	// Postgres-only and would 500 against local SQLite. Synthesize the
	// same response shape from sentinel constants + the local user
	// row's stored identity so the FE renders one signed-in path across
	// both modes (local equals multi at N=1). github_username + jira_*
	// are sourced from the same users-store calls /api/config used to
	// make pre-slim, so the predicate editor finds them on /api/me in
	// both modes.
	//
	// Gated on authDeps==nil (not just the Subject) so a multi-mode
	// caller whose real GoTrue user UUID happens to collide with the
	// sentinel (seeded fixtures, deterministic test users) falls through
	// to the normal DB-backed path instead of receiving a fabricated
	// Local org that doesn't match their real memberships.
	if s.authDeps == nil && claims.Subject == runmode.LocalDefaultUserID {
		resp := response{
			ID:          runmode.LocalDefaultUserID,
			DisplayName: "Local",
			Orgs: []orgRow{{
				ID:   runmode.LocalDefaultOrgID,
				Name: "Local",
				Role: "owner",
			}},
			ActiveOrgID: runmode.LocalDefaultOrgID,
			Teams:       []teamRow{},
			// Local mode is N=1 with a pre-provisioned org and never
			// renders the onboarding entry, so the value is moot; report
			// the permissive default for shape consistency.
			OrgCreationEnabled: true,
			// The single local user is implicitly the operator (there is no
			// one else); FeatureFleet is still unlicensed locally, so the
			// console stays dark — this only makes core operability legible.
			IsOperator: true,
		}
		// s.users is wired by New(). The middleware-shim unit test
		// constructs a bare &Server{} to exercise the sentinel path
		// without an auth stack — guard the lookups so that minimal
		// rig still works while production calls (always wired)
		// populate identity from the users row.
		if s.users != nil {
			// Identity is host-scoped (GitHub, Jira):
			// resolve the local org's GitHub + Jira hosts, then look up
			// each binding for (user, host). s.orgs is wired by New();
			// guard like s.users for the bare-rig test.
			var ghHost, jiraHost string
			if s.orgs != nil {
				if orgSet, err := s.orgs.GetSettings(r.Context(), runmode.LocalDefaultOrgID); err == nil {
					ghHost = orgSet.GitHubBaseURL
					jiraHost = orgSet.JiraBaseURL
				}
			}
			resp.GitHubUsername, _ = s.users.GetGitHubLogin(r.Context(), runmode.LocalDefaultUserID, ghHost)
			resp.JiraAccountID, resp.JiraDisplayName, _ = s.users.GetJiraIdentity(r.Context(), runmode.LocalDefaultUserID, jiraHost)
		}
		// The sole local team, which local mode reports the single user as
		// admin of — the same role the teams list gives the per-team gates, so
		// the two reads of that one truth agree. s.teams is wired by New();
		// guard it like s.users for the bare-rig test. A read error is fatal
		// rather than an empty list: the field carries the viewer's team
		// grants, and answering "no teams" for a failed read withdraws them
		// silently.
		if s.teams != nil {
			teamID, err := s.teams.GetDefaultForOrg(r.Context(), runmode.LocalDefaultOrgID)
			if err != nil {
				internalError(w, "auth", fmt.Errorf("local default team: %w", err))
				return
			}
			if teamID != "" {
				t, err := s.teams.Get(r.Context(), runmode.LocalDefaultOrgID, teamID)
				if err != nil {
					internalError(w, "auth", fmt.Errorf("local team %s: %w", teamID, err))
					return
				}
				if t != nil {
					resp.Teams = append(resp.Teams, teamRow{ID: t.ID, Name: t.Name, OrgID: t.OrgID, Role: t.Role})
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	var resp response
	resp.Orgs = []orgRow{}
	resp.Teams = []teamRow{}
	resp.Email = claims.Email
	resp.OrgCreationEnabled = runmode.OrgCreationEnabled()
	// Deployment-operator flag (TFAC-589): a plain lookup of the verified
	// session email against the org-less operators table. Admin-pool read
	// (operators has no RLS policy); best-effort — a lookup error leaves it
	// false (fail closed, the console 404s), never fails the identity read.
	if s.allStores.Operators != nil && claims.Email != "" {
		if ok, err := s.allStores.Operators.IsOperator(r.Context(), claims.Email); err != nil {
			serverLog.Warn("operator lookup failed on /api/me", "error", err)
		} else {
			resp.IsOperator = ok
		}
	}
	if sess := SessionFrom(r.Context()); sess != nil && sess.ActiveOrgID.Valid {
		resp.ActiveOrgID = sess.ActiveOrgID.UUID.String()
	}

	// Wrap both reads in a single transaction with request.jwt.claims
	// populated, so the queries source the identity from
	// tf.current_user_id() rather than a $1 parameter. Defense-in-
	// depth: if a future bug routes a request here with claim
	// context pointing at a different user, the SQL helpers return
	// that user's row — not "whatever ID the caller passes." Once
	// D9 introduces the app pool with RLS enforcement, the same
	// queries become RLS-defended end-to-end without further edits.
	// Pass the session's active org as the org claim too (not just sub) so
	// the host-scoped GitHub identity lookup below can prefer the row bound
	// to the active org's GitHub host. A stale/empty active org
	// is harmless: org_event_sources RLS filters it out and the lookup falls
	// back to the most-recently-verified identity row.
	err := tfdb.WithTx(r.Context(), s.db,
		tfdb.Claims{Sub: claims.Subject, OrgID: resp.ActiveOrgID},
		func(tx *sql.Tx) error {
			// Identity is host-scoped for both providers (GitHub,
			// Jira): prefer the row bound to the active org's host
			// (GitHub login via the correlated subquery; Jira account_id +
			// display_name via the LATERAL, which keeps the pair on one row),
			// else the most recently verified row. An absent row scans to ""
			// exactly as the old NULL columns did.
			if err := tx.QueryRowContext(r.Context(), `
				SELECT u.id::text,
				       COALESCE(u.display_name, ''),
				       COALESCE(u.avatar_url, ''),
				       COALESCE((
				           SELECT i.login
				             FROM user_github_identities i
				            WHERE i.user_id = u.id
					            ORDER BY (i.github_base_url = rtrim((
					                        SELECT es.base_url FROM org_event_sources es
					                         WHERE es.org_id = tf.current_org_id() AND es.kind = 'github'
					                      ), '/')) DESC NULLS LAST,
					                     i.verified_at DESC NULLS LAST
				            LIMIT 1
				       ), ''),
				       COALESCE(j.account_id, ''),
				       COALESCE(j.display_name, '')
				  FROM public.users u
			  LEFT JOIN LATERAL (
			           SELECT ji.account_id, ji.display_name
			             FROM user_jira_identities ji
			            WHERE ji.user_id = u.id
			            ORDER BY (ji.jira_base_url = rtrim(trim((
			                        SELECT es.base_url FROM org_event_sources es
			                         WHERE es.org_id = tf.current_org_id() AND es.kind = 'jira'
			                      )), '/')) DESC NULLS LAST,
			                     ji.verified_at DESC NULLS LAST
			            LIMIT 1
			       ) j ON true
				 WHERE u.id = tf.current_user_id()
			`).Scan(&resp.ID, &resp.DisplayName, &resp.AvatarURL, &resp.GitHubUsername, &resp.JiraAccountID, &resp.JiraDisplayName); err != nil {
				return fmt.Errorf("user lookup: %w", err)
			}

			// The two membership reads are separate funcs so each result set
			// is closed by its own defer before the next query issues: a
			// transaction is one connection, and a still-open result set on it
			// is a busy connection rather than a second cursor.
			readOrgs := func() error {
				rows, err := tx.QueryContext(r.Context(), `
					SELECT o.id::text, o.name, om.role
					  FROM org_memberships om
					  JOIN orgs o ON o.id = om.org_id
					 WHERE om.user_id = tf.current_user_id()
					 ORDER BY o.name
				`)
				if err != nil {
					return fmt.Errorf("org list: %w", err)
				}
				defer rows.Close()
				for rows.Next() {
					var o orgRow
					if err := rows.Scan(&o.ID, &o.Name, &o.Role); err != nil {
						return fmt.Errorf("org scan: %w", err)
					}
					resp.Orgs = append(resp.Orgs, o)
				}
				return rows.Err()
			}

			// Team memberships carry no org predicate, unlike the teams list:
			// /me spans every org the viewer belongs to, so each row is tagged
			// with the org whose team it is. Keyed on tf.current_user_id() like
			// the org query, so the memberships join is what narrows the rows
			// and there is no user id parameter to get wrong. deleted_at IS
			// NULL is the same request-facing filter that hides an archived
			// team from every selector. Ordered (org_id, name) with an id
			// tiebreak so the shape is stable across reads.
			readTeams := func() error {
				rows, err := tx.QueryContext(r.Context(), `
					SELECT t.id::text, t.name, t.org_id::text, m.role::text
					  FROM memberships m
					  JOIN teams t ON t.id = m.team_id
					 WHERE m.user_id = tf.current_user_id()
					   AND t.deleted_at IS NULL
					 ORDER BY t.org_id, t.name, t.id
				`)
				if err != nil {
					return fmt.Errorf("team list: %w", err)
				}
				defer rows.Close()
				for rows.Next() {
					var tr teamRow
					if err := rows.Scan(&tr.ID, &tr.Name, &tr.OrgID, &tr.Role); err != nil {
						return fmt.Errorf("team scan: %w", err)
					}
					resp.Teams = append(resp.Teams, tr)
				}
				return rows.Err()
			}

			if err := readOrgs(); err != nil {
				return err
			}
			return readTeams()
		},
	)
	if err != nil {
		internalError(w, "auth", fmt.Errorf("/api/me for %s: %w", claims.Subject, err))
		return
	}

	// Drop a stale ActiveOrgID. sessions.active_org_id's FK only points
	// at orgs(id), not org_memberships — when a user's membership is
	// revoked but the org still exists, the session retains the now-
	// orphaned reference. Surfacing it would let an FE honoring the
	// active_org_id contract route to an org the caller no longer
	// belongs to.
	if resp.ActiveOrgID != "" {
		stillMember := false
		for _, o := range resp.Orgs {
			if o.ID == resp.ActiveOrgID {
				stillMember = true
				break
			}
		}
		if !stillMember {
			resp.ActiveOrgID = ""
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleMeIdentities returns the login identities linked to the signed-in
// PRINCIPAL — one row per GoTrue login (a GitHub login + N SSO providers), all
// resolving to the same public.users row via verified-email linking at login.
// This is the first shared (non-system) read of public.user_identities, the
// "Login methods" view on the account surface.
//
// Personal-identity read (axis (iii) of the multi-mode read-scoping rule in
// CLAUDE.md): scoped on tf.current_user_id() through the session, never org-
// scoped. The opaque bridge columns — auth_user_id and provider_subject — are
// deliberately NOT exposed: they carry no user value and auth_user_id is an
// internal GoTrue key. The identity backing the active session is flagged
// `current` so the UI can mark "this is the login you're signed in with."
//
// Runs on s.db, the admin (BYPASSRLS) pool — same posture as handleMe.
// user_identities is deny-by-default for tf_app (REVOKEd, no app-role policy:
// resolution + linking only ever ran on the admin pool at login), so the
// superuser pool does the read while `WHERE user_id = tf.current_user_id()`
// does the principal scoping. tfdb.WithTx seeds request.jwt.claims so that
// helper resolves to the principal even though RLS isn't enforcing here.
//
// Local mode has no GoTrue and no user_identities table, so it returns a single
// synthetic row — keeping the wire contract uniform across modes (the component
// needs no mode branch). It's a degenerate view: local mode is N=1 with no
// login/link/verification concept.
func (s *Server) handleMeIdentities(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFrom(r.Context())
	if claims == nil {
		writeUnauth(w)
		return
	}

	// loginMethod is one linked identity. Wire shape mirrored in the frontend
	// as LoginMethod (types.ts). No auth_user_id / provider_subject — see the
	// handler doc.
	type loginMethod struct {
		Provider      string `json:"provider"`
		Email         string `json:"email,omitempty"`
		EmailVerified bool   `json:"email_verified"`
		LinkedAt      string `json:"linked_at,omitempty"`
		Current       bool   `json:"current"`
	}
	type response struct {
		Methods []loginMethod `json:"methods"`
	}

	// Local-mode shim: one synthetic "local" row. Gated on authDeps==nil (not
	// just the sentinel Subject) so a multi-mode caller whose principal UUID
	// collides with the sentinel still falls through to the DB path — same
	// rationale as handleMe.
	if s.authDeps == nil && claims.Subject == runmode.LocalDefaultUserID {
		writeJSON(w, http.StatusOK, response{Methods: []loginMethod{{
			Provider: "local",
			Current:  true,
		}}})
		return
	}

	// The login identity backing THIS session (JWT sub == auth_user_id),
	// preserved by withSession before it remapped Subject to the principal.
	// Empty would simply mark no row current; in multi mode it's always set.
	currentIdentity := authIdentityIDFrom(r.Context())

	var activeOrg string
	if sess := SessionFrom(r.Context()); sess != nil && sess.ActiveOrgID.Valid {
		activeOrg = sess.ActiveOrgID.UUID.String()
	}

	resp := response{Methods: []loginMethod{}}
	err := tfdb.WithTx(r.Context(), s.db,
		tfdb.Claims{Sub: claims.Subject, OrgID: activeOrg},
		func(tx *sql.Tx) error {
			// Oldest-first: the bootstrap GitHub (break-glass) identity leads,
			// then SSO additions — the order that tells the linking story. The
			// provider tiebreak keeps same-instant rows deterministic.
			rows, err := tx.QueryContext(r.Context(), `
				SELECT auth_user_id::text, provider, COALESCE(email, ''),
				       email_verified, created_at
				  FROM public.user_identities
				 WHERE user_id = tf.current_user_id()
				 ORDER BY created_at ASC, provider ASC
			`)
			if err != nil {
				return fmt.Errorf("identities query: %w", err)
			}
			defer rows.Close()
			for rows.Next() {
				var authUserID, provider, email string
				var verified bool
				var createdAt time.Time
				if err := rows.Scan(&authUserID, &provider, &email, &verified, &createdAt); err != nil {
					return fmt.Errorf("identity scan: %w", err)
				}
				resp.Methods = append(resp.Methods, loginMethod{
					Provider:      provider,
					Email:         email,
					EmailVerified: verified,
					LinkedAt:      createdAt.UTC().Format(time.RFC3339),
					// Fold-insensitive: authUserID is Postgres ::text (lowercase
					// canonical), currentIdentity is the raw JWT sub. Both are
					// lowercase in practice, but comparing case-insensitively keeps
					// the mark from silently missing on any UUID-format drift.
					Current: strings.EqualFold(authUserID, currentIdentity),
				})
			}
			return rows.Err()
		},
	)
	if err != nil {
		internalError(w, "auth", fmt.Errorf("/api/me/identities for %s: %w", claims.Subject, err))
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

// resolveOrCreatePrincipal maps a GoTrue login identity (authUserID = the JWT
// sub) to its TF principal — the public.users row that memberships, ownership,
// and the session all key on — and returns the principal id.
//
// One human can hold several login identities: a GitHub login plus N SSO
// providers, because GoTrue mints a distinct auth.users per SSO provider and
// refuses to link them. TF unifies them in public.user_identities:
//
//   - returning identity (auth_user_id already linked) → its principal;
//   - new identity whose VERIFIED email matches an existing principal → link to
//     that principal (this is how a later SSO login attaches to the same human
//     as an earlier GitHub login — no duplicate account, no migration);
//   - otherwise → a fresh principal.
//
// Linking is gated on a *verified* email only: an unverified or absent email
// never links (the safe default is a separate principal). A per-email advisory
// lock serializes concurrent first-logins for the same verified email so a race
// can't fork one human into two principals — the only way the "one principal
// per verified email" invariant could otherwise break.
//
// Runs on the admin pool (the actor has no membership/claims yet, so RLS can't
// express these reads/writes — same rationale as org provisioning).
//
// isSSO marks a SAML login (TF-derived from its own signed state, never the
// JWT). It controls only the GitHub-INTEGRATION write — a separate axis: which
// GitHub account the agent acts as. A GitHub login mirrors that as a
// convenience binding (source='login_claim'); a SAML login writes none (an SSO
// user binds GitHub later via PAT/Connect). An Entra assertion can carry a
// preferred_username that is a UPN/email, not a github.com handle, so mirroring
// it would mint a bogus identity — hence the !isSSO gate.
func resolveOrCreatePrincipal(ctx context.Context, db *sql.DB, authUserID uuid.UUID, claims *verify.Claims, isSSO bool) (uuid.UUID, error) {
	provider := "github"
	if isSSO {
		provider = "saml"
	}
	email := strings.ToLower(strings.TrimSpace(claims.Email))

	var displayName, avatarURL, ghUsername, providerSubject string
	if claims.UserMetadata != nil {
		displayName, _ = claims.UserMetadata["full_name"].(string)
		if displayName == "" {
			displayName, _ = claims.UserMetadata["name"].(string)
		}
		// A GitHub account with no profile "Name" set yields an empty
		// full_name/name — fall back to the login handle so the roster never
		// renders "Unnamed user" for someone we actually have an identity for
		// (TFAC-560). user_name specifically (not the broader ghUsername
		// fallback below, which also takes preferred_username) since a SAML
		// preferred_username can be a UPN/email rather than a handle.
		if displayName == "" {
			displayName, _ = claims.UserMetadata["user_name"].(string)
		}
		avatarURL, _ = claims.UserMetadata["avatar_url"].(string)
		ghUsername, _ = claims.UserMetadata["user_name"].(string)
		if ghUsername == "" {
			ghUsername, _ = claims.UserMetadata["preferred_username"].(string)
		}
		// Best-effort: the IdP's own stable subject (GitHub numeric id / SAML
		// NameID). Non-load-bearing insurance for a future re-link that doesn't
		// trust email; null when the provider doesn't surface one.
		providerSubject, _ = claims.UserMetadata["provider_id"].(string)
		if providerSubject == "" {
			providerSubject, _ = claims.UserMetadata["sub"].(string)
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return uuid.Nil, fmt.Errorf("begin principal tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit succeeds

	// Serialize concurrent logins for the SAME identity (browser double-submit /
	// retry) so two requests can't both miss the lookup, both mint a principal,
	// and collide on the user_identities PK (which would 500 the second). Taken
	// on every login, keyed on auth_user_id (salt 1).
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 1))`, authUserID.String()); err != nil {
		return uuid.Nil, fmt.Errorf("acquire identity lock: %w", err)
	}
	// Additionally serialize concurrent first-logins by DIFFERENT identities that
	// share a verified email (salt 0), so they link to one principal rather than
	// racing into two. A missing or unverified email skips this — those never
	// link. Consistent lock order (identity then email) avoids deadlock.
	if email != "" && claims.EmailVerified {
		if _, err := tx.ExecContext(ctx,
			`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, email); err != nil {
			return uuid.Nil, fmt.Errorf("acquire email lock: %w", err)
		}
	}

	var principalID uuid.UUID
	lookupErr := tx.QueryRowContext(ctx,
		`SELECT user_id FROM public.user_identities WHERE auth_user_id = $1`, authUserID,
	).Scan(&principalID)
	switch {
	case lookupErr == nil:
		// Returning identity — fall through to the profile/email refresh below.
	case errors.Is(lookupErr, sql.ErrNoRows):
		// New identity. Link to an existing principal by verified email, else mint.
		if email != "" && claims.EmailVerified {
			if lerr := tx.QueryRowContext(ctx, `
				SELECT user_id FROM public.user_identities
				 WHERE lower(email) = $1 AND email_verified
				 LIMIT 1`, email,
			).Scan(&principalID); lerr != nil && !errors.Is(lerr, sql.ErrNoRows) {
				return uuid.Nil, fmt.Errorf("verified-email link lookup: %w", lerr)
			}
		}
		if principalID == uuid.Nil {
			if err := tx.QueryRowContext(ctx, `
				INSERT INTO public.users (display_name, avatar_url, created_at, updated_at)
				VALUES (NULLIF($1, ''), NULLIF($2, ''), now(), now())
				RETURNING id`, displayName, avatarURL,
			).Scan(&principalID); err != nil {
				return uuid.Nil, fmt.Errorf("create principal: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO public.user_identities
				(auth_user_id, user_id, provider, provider_subject, email, email_verified, created_at, updated_at)
			VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), $6, now(), now())
		`, authUserID, principalID, provider, providerSubject, email, claims.EmailVerified); err != nil {
			return uuid.Nil, fmt.Errorf("link identity: %w", err)
		}
	default:
		return uuid.Nil, fmt.Errorf("identity lookup: %w", lookupErr)
	}

	// Refresh the principal's profile + this identity's email on every login
	// (provider responses drift; COALESCE keeps a field we already have when the
	// claim omits it).
	if _, err := tx.ExecContext(ctx, `
		UPDATE public.users
		   SET display_name = COALESCE(NULLIF($2, ''), display_name),
		       avatar_url   = COALESCE(NULLIF($3, ''), avatar_url),
		       updated_at   = now()
		 WHERE id = $1
	`, principalID, displayName, avatarURL); err != nil {
		return uuid.Nil, fmt.Errorf("refresh principal: %w", err)
	}
	// Refresh email + its verification from the claim, but only when the claim
	// actually carries an email: a later login that omits it must NOT NULL out a
	// known email or downgrade its verified flag (that would silently disable
	// future linking). When an email IS present we take the provider's current
	// truth for it — including a genuine un-verify — rather than forcing
	// monotonicity, since a stale verified=true on a changed/unverified email
	// would be the unsafe direction (it could let a later identity wrongly link).
	if _, err := tx.ExecContext(ctx, `
		UPDATE public.user_identities
		   SET email          = COALESCE(NULLIF($2, ''), email),
		       email_verified = CASE WHEN NULLIF($2, '') IS NOT NULL THEN $3 ELSE email_verified END,
		       updated_at     = now()
		 WHERE auth_user_id = $1
	`, authUserID, email, claims.EmailVerified); err != nil {
		return uuid.Nil, fmt.Errorf("refresh identity: %w", err)
	}

	// GitHub-INTEGRATION binding (which github account the agent acts as) — a
	// separate axis from the login identity. Only a real GitHub login mirrors it
	// (source='login_claim'); a SAML login writes none. Keyed on the principal.
	if !isSSO && ghUsername != "" {
		githubEmail := ""
		if claims.EmailVerified {
			githubEmail = email
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO public.user_github_identities
				(user_id, github_base_url, login, github_user_id, email, source, verified_at, created_at, updated_at)
			VALUES ($1, 'https://github.com', $2, NULLIF($3, ''), NULLIF($4, ''), 'login_claim', now(), now(), now())
			ON CONFLICT (user_id, github_base_url) DO UPDATE
			   SET login       = EXCLUDED.login,
			       github_user_id = COALESCE(EXCLUDED.github_user_id, user_github_identities.github_user_id),
			       email       = CASE
			                       WHEN user_github_identities.login IS DISTINCT FROM EXCLUDED.login THEN EXCLUDED.email
			                       ELSE COALESCE(EXCLUDED.email, user_github_identities.email) END,
			       source      = EXCLUDED.source,
			       verified_at = EXCLUDED.verified_at,
			       updated_at  = now()
		`, principalID, ghUsername, providerSubject, githubEmail); err != nil {
			return uuid.Nil, fmt.Errorf("upsert github login identity: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return uuid.Nil, fmt.Errorf("commit principal tx: %w", err)
	}
	return principalID, nil
}

// gotrueRefreshFunc — POST /token?grant_type=refresh_token.
//
// Body is JSON, matching the pkce grant — GoTrue's /token handler
// uses the same JSON-only request parser across grant types.
func (s *Server) gotrueRefreshFunc(cfg *authConfig) func(context.Context, string) (string, string, int64, error) {
	return func(ctx context.Context, refreshToken string) (string, string, int64, error) {
		payload, err := json.Marshal(map[string]string{
			"refresh_token": refreshToken,
		})
		if err != nil {
			return "", "", 0, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			cfg.gotrueURL+"/token?grant_type=refresh_token", bytes.NewReader(payload))
		if err != nil {
			return "", "", 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		return decodeTokenResponse(req, "refresh")
	}
}

// gotrueExchangeFunc — PKCE auth-code exchange.
// POST /token?grant_type=pkce  body: auth_code, code_verifier.
//
// gotrue verifies that sha256(code_verifier) matches the
// code_challenge it stored when the /authorize redirect happened.
// On mismatch (replay, MITM tamper) the call returns 400. On
// success, the response body carries access_token + refresh_token.
func (s *Server) gotrueExchangeFunc(cfg *authConfig) func(context.Context, string, string) (string, string, int64, error) {
	return func(ctx context.Context, authCode, codeVerifier string) (string, string, int64, error) {
		// GoTrue's /token?grant_type=pkce expects a JSON body, not
		// application/x-www-form-urlencoded — form bodies decode to
		// an empty struct and the handler errors out with bad_json.
		payload, err := json.Marshal(map[string]string{
			"auth_code":     authCode,
			"code_verifier": codeVerifier,
		})
		if err != nil {
			return "", "", 0, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			cfg.gotrueURL+"/token?grant_type=pkce", bytes.NewReader(payload))
		if err != nil {
			return "", "", 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		return decodeTokenResponse(req, "exchange")
	}
}

// gotrueSSOFunc — SP-initiated SAML start. POST /sso (a PUBLIC GoTrue endpoint:
// send NO Authorization header — the TF_GOTRUE_SERVICE_ROLE_TOKEN bearer is for
// the /admin/* API only, used by TFAC-424) with the PKCE code_challenge so the
// same /token?grant_type=pkce exchange the GitHub flow uses completes the
// callback (TFAC-423 spike: PKCE composes with SP-initiated SAML). GoTrue
// answers 303 with the IdP redirect in Location; this returns that Location for
// StartSSO to forward to the browser.
func (s *Server) gotrueSSOFunc(cfg *authConfig) func(context.Context, string, string, string) (string, error) {
	return func(ctx context.Context, providerID, redirectTo, codeChallenge string) (string, error) {
		// JSON body (GoTrue's /sso decodes SingleSignOnParams from JSON).
		// "S256" is RFC 7636's canonical value and matches the GitHub
		// /authorize entrypoint above — keep the two PKCE paths identical
		// rather than relying on GoTrue case-folding a lowercase variant.
		payload, err := json.Marshal(map[string]string{
			"provider_id":           providerID,
			"redirect_to":           redirectTo,
			"code_challenge":        codeChallenge,
			"code_challenge_method": "S256",
		})
		if err != nil {
			return "", err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			cfg.gotrueURL+"/sso", bytes.NewReader(payload))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := gotrueSSOClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("sso start http: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
			return "", fmt.Errorf("sso start http %d: %s", resp.StatusCode, bytes.TrimSpace(b))
		}
		loc := resp.Header.Get("Location")
		if loc == "" {
			return "", errors.New("sso start: 303 with no Location header")
		}
		return loc, nil
	}
}

// gotrueLogoutFunc — POST /logout with Authorization: Bearer.
// Invalidates the refresh-token family upstream so a leaked access
// token can't be silently refreshed indefinitely.
func (s *Server) gotrueLogoutFunc(cfg *authConfig) func(context.Context, string) error {
	return func(ctx context.Context, accessToken string) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			cfg.gotrueURL+"/logout", nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		resp, err := gotrueHTTPClient.Do(req)
		if err != nil {
			return fmt.Errorf("logout http: %w", err)
		}
		defer resp.Body.Close()
		// gotrue returns 204 on success. Treat 4xx as "session already
		// invalid upstream" — that's the desired end state, so not an
		// error from our perspective.
		if resp.StatusCode >= 500 {
			b, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("logout http %d: %s", resp.StatusCode, bytes.TrimSpace(b))
		}
		return nil
	}
}

// decodeTokenResponse handles the shared /token response shape used
// by both refresh and PKCE-exchange. label distinguishes errors. The
// request already carries a context via http.NewRequestWithContext, so
// cancellation propagates through gotrueHTTPClient.
func decodeTokenResponse(req *http.Request, label string) (string, string, int64, error) {
	resp, err := gotrueHTTPClient.Do(req)
	if err != nil {
		return "", "", 0, fmt.Errorf("%s http: %w", label, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", "", 0, fmt.Errorf("%s http %d: %s", label, resp.StatusCode, bytes.TrimSpace(b))
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", 0, fmt.Errorf("%s decode: %w", label, err)
	}
	if out.AccessToken == "" || out.RefreshToken == "" {
		return "", "", 0, fmt.Errorf("%s response missing tokens", label)
	}
	return out.AccessToken, out.RefreshToken,
		timeNow().Add(time.Duration(out.ExpiresIn) * time.Second).Unix(), nil
}

// ---- PKCE -----------------------------------------------------------------

// generatePKCEVerifier returns a base64url-encoded 32-byte random
// string. RFC 7636 allows 43-128 chars from the unreserved set;
// base64url(32) is 43 chars — minimum acceptable size, maximum entropy
// per byte.
func generatePKCEVerifier() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// pkceChallenge computes the S256 challenge:
//
//	challenge = base64url(sha256(verifier))
//
// gotrue stores this on /authorize and validates against the verifier
// supplied on /token exchange.
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ---- state cookie ----------------------------------------------------------

const stateCookieName = "tf_oauth_state"

type stateClaims struct {
	ReturnTo string `json:"return_to"`
	CSRF     string `json:"csrf"`
	// CodeVerifier carries the PKCE verifier from /authorize redirect
	// to /callback exchange. Lives only in the HMAC-signed state cookie
	// (HttpOnly, scoped to /api/auth/, 10-minute TTL). Never persisted
	// server-side and never leaves the cookie.
	CodeVerifier string `json:"cv"`
	// ProviderID is the GoTrue SSO provider_id when this is a SAML
	// SP-initiated flow (set by StartSSO), empty for the GitHub
	// OAuth flow. It rides in TF's OWN signed state — NOT read back from
	// the GoTrue JWT — so the callback's JIT step resolves the org binding
	// from a value TF chose, never from anything the assertion carries
	// (the GoTrue provider claims are version-fragile; see StartSSO).
	// Its non-empty presence at the callback is the authoritative "this
	// login is SSO" signal.
	ProviderID string `json:"pid,omitempty"`
	// Test marks a verify-before-enforce SSO test round-trip (TFAC-431), set
	// only by the admin-gated test-start endpoint (the login extension's handleTestStart).
	// When true the callback completes the code exchange + JWT verify — proving
	// the assertion is valid and readable — then SHORT-CIRCUITS before any
	// writes: no resolveOrCreatePrincipal, no JIT, no session. It renders a
	// pass/fail page in the admin's popup instead. Like ProviderID it rides in
	// TF's OWN signed state (never read back from the GoTrue JWT), so a forged
	// callback can't flip a real login into the no-write test path or vice versa.
	Test      bool  `json:"test,omitempty"`
	ExpiresAt int64 `json:"exp"`
}

func (sc stateClaims) sign(key [32]byte) (string, error) {
	payload, err := json.Marshal(sc)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key[:])
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) +
		"." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func parseStateCookie(raw string, key [32]byte) (*stateClaims, error) {
	parts := strings.SplitN(raw, ".", 2)
	if len(parts) != 2 {
		return nil, errors.New("malformed state cookie")
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
	var sc stateClaims
	if err := json.Unmarshal(payload, &sc); err != nil {
		return nil, fmt.Errorf("unmarshal payload: %w", err)
	}
	if timeNow().Unix() > sc.ExpiresAt {
		return nil, errors.New("expired")
	}
	return &sc, nil
}

// normalizeReturnTo enforces relative-path-only and a default of "/".
// Anything starting with "//" (protocol-relative URL) or containing a
// scheme/host is rewritten to "/" — open-redirect protection.
//
// Backslashes are also rejected (both literal `\` and percent-encoded
// `%5C`). WHATWG URL parsing treats `\` as `/` for special schemes, so
// `/\evil.com` resolves to `//evil.com` in modern browsers — the same
// open-redirect class the `//` guard already blocks. Closing the
// parallel gap.
func normalizeReturnTo(raw string) string {
	if raw == "" || raw == "/" {
		return "/"
	}
	if strings.Contains(raw, "\\") {
		return "/"
	}
	if dec, err := url.PathUnescape(raw); err == nil && strings.Contains(dec, "\\") {
		return "/"
	}
	if !strings.HasPrefix(raw, "/") {
		return "/"
	}
	if strings.HasPrefix(raw, "//") {
		return "/"
	}
	if u, err := url.Parse(raw); err == nil {
		if u.Scheme != "" || u.Host != "" {
			return "/"
		}
	}
	return raw
}

// isHTTPS detects whether the original request came in over TLS, even
// behind a reverse proxy that terminated TLS. Used to set the Secure
// flag on cookies — HTTPS-only in prod, but local-dev runs over HTTP
// and would otherwise refuse to accept the cookie at all.
//
// Unlike clientIP, the X-Forwarded-Proto read is deliberately NOT gated on
// the trusted-proxy allowlist (TFAC-488). A forged "X-Forwarded-Proto: https"
// only ever ADDS a Secure flag, which is self-defeating for the forger — the
// browser then refuses to send that cookie back over plain HTTP — so there's
// no attacker leverage to remove. Meanwhile the authoritative prod signal is
// deployCfg.secureCookies (from TF_PUBLIC_URL), which cookieSecure checks
// first; this header is only the fallback for an HTTP-publicURL deployment
// behind a TLS-terminating proxy, or local dev, where requiring
// TF_TRUSTED_PROXY_CIDR just to get Secure cookies would be a footgun. Gating
// here could only strip a legitimate Secure flag, never block a harmful one.
func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return r.Header.Get("X-Forwarded-Proto") == "https"
}

// maxXFFHops bounds how many X-Forwarded-For entries clientIP examines when
// walking a trusted proxy's chain right→left. In a proxied deployment the
// header is attacker-controlled, so without a bound a caller could pad it
// with commas to force proportional work on a pre-auth route. A real
// trusted-proxy chain (CDN → LB → app, plus health-check / internal hops) is
// a handful deep; 64 sits far above any sane topology while keeping the walk
// O(1) in the header's length. Past the bound, clientIP falls back to the
// trusted peer (degraded but safe).
const maxXFFHops = 64

// clientIP best-effort extracts the requesting client's IP for the three
// consumers that key on it — the session forensics row (sessions.ip_addr),
// the SOC2 auth audit log (auth_events.ip_address), and the pre-auth
// per-IP rate limiter. The return value is stored as Postgres `inet`, so it
// must be a valid `inet` literal (or "" → NULL); net.SplitHostPort unwraps
// bracketed IPv6 and drops the port so a value like `[2001:db8::1]` can't
// fail the `::inet` cast and 500 the OAuth callback.
//
// X-Forwarded-For is client-*appended* (each proxy adds a hop, none
// overwrites), so its leftmost entry is attacker-controlled. It is trusted
// ONLY when the direct peer is a configured trusted proxy. The policy
// (TFAC-488; configured via runmode / TF_TRUSTED_PROXY_CIDR +
// TF_CAPTURE_CLIENT_IP):
//
//   - Capture disabled → "" (the IP columns store NULL).
//   - No trusted-proxy allowlist, or the peer is not in it (direct
//     exposure) → RemoteAddr only; XFF ignored (forgeable). Secure
//     default: never returns an attacker-chosen value.
//   - Peer IS a trusted proxy → walk XFF right→left, skip trusted hops,
//     return the first untrusted entry (the real client). If every entry
//     is trusted (or XFF is absent), fall back to the peer — degraded but
//     safe.
func clientIP(r *http.Request) string {
	if !runmode.CaptureClientIP() {
		return ""
	}

	peer := remoteHost(r.RemoteAddr)

	trusted := runmode.TrustedProxies()
	if len(trusted) == 0 || !ipInCIDRs(peer, trusted) {
		// No allowlist, or the direct peer isn't a trusted proxy: its XFF
		// is forgeable, so ignore it and attribute the peer itself.
		return peer
	}

	// Peer is a trusted proxy. XFF is append-order (leftmost = original
	// caller, rightmost = nearest hop), so walking right→left past the
	// trusted hops lands on the first IP the chain didn't vouch for — the
	// real client. Anything the caller pre-seeded sits to the LEFT of the
	// address the first trusted proxy appended, so it's never reached.
	//
	// Scan with LastIndexByte rather than strings.Split: the header is
	// attacker-controlled here, and Split would allocate a slice
	// proportional to its comma count — a cheap pre-auth memory/CPU
	// amplifier. This slices one field at a time (substring headers into the
	// original, no copy) from the right, bounded by maxXFFHops, and returns
	// as soon as it finds the client — typically on the first iteration,
	// since the proxy appends the real (untrusted) peer at the far right.
	rest := r.Header.Get("X-Forwarded-For")
	for hops := 0; rest != "" && hops < maxXFFHops; hops++ {
		field := rest
		if i := strings.LastIndexByte(rest, ','); i >= 0 {
			field, rest = rest[i+1:], rest[:i]
		} else {
			rest = ""
		}
		entry := normalizeXFFEntry(field)
		if entry == "" {
			continue // empty / unparseable hop — skip
		}
		if ipInCIDRs(entry, trusted) {
			continue // another trusted proxy in the chain
		}
		return entry // first untrusted from the right → the client
	}

	// Every forwarded hop was trusted (or none present): the closest
	// attributable address is the trusted peer.
	return peer
}

// remoteHost extracts the host portion of a RemoteAddr, unwrapping a
// bracketed IPv6 and dropping the port. Preserves the historical fallback:
// when no port is present (degenerate — RemoteAddr almost always carries
// one in HTTP), the value is returned as-is so a downstream inet cast
// surfaces a clear error rather than a silent empty.
func remoteHost(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

// normalizeXFFEntry trims one X-Forwarded-For element to a canonical bare-IP
// string, dropping any port some proxies append and unwrapping a bracketed
// IPv6. Returns "" for an empty or non-IP token so the right→left walk skips
// it rather than treating garbage as a client.
func normalizeXFFEntry(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host // had a port (incl. bracketed "[v6]:port")
	} else {
		s = strings.TrimPrefix(strings.TrimSuffix(s, "]"), "[") // bare "[v6]"
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return ""
	}
	return ip.String()
}

// ipInCIDRs reports whether ipStr parses to an IP contained in any of nets.
// A non-IP string is never in the set.
//
// IPv4-mapped IPv6 (::ffff:a.b.c.d) is normalized to its 4-byte v4 form
// first. Dual-stack load balancers (nginx on Linux, AWS ALB, and many
// others) deliver connections to Go as the mapped address, so without this
// an operator who sets an IPv4 allowlist (TF_TRUSTED_PROXY_CIDR=10.0.0.0/8)
// for such an LB would silently fail the match — net.IPNet.Contains compares
// equal-length addresses, so a 16-byte mapped peer never matches a 4-byte v4
// net — and XFF would be quietly ignored, collapsing the rate limiter and
// recording the LB in the audit log with no signal why. Genuine cross-family
// pairs (a real v6 peer against a v4 CIDR) still don't match, which is right.
func ipInCIDRs(ipStr string, nets []*net.IPNet) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
