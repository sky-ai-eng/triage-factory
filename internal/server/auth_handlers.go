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
		gotrueLogout:   s.gotrueLogoutFunc(cfg),
	}

	// Spawn the session reaper. Cadence + retention follow the arch
	// doc (10-minute tick, 30-day retention window); the goroutine
	// exits when ctx is cancelled, so server shutdown / test cleanup
	// drains it without further wiring.
	go sessionStore.RunReaper(ctx, 10*time.Minute, 30*24*time.Hour)

	return nil
}

// handleOAuthStart redirects the browser to gotrue's /authorize with
// the PKCE parameters set. The state cookie carries the CSRF token,
// the PKCE code_verifier, and the return_to path through the OAuth
// roundtrip so the callback handler can complete the dance with no
// per-flow database row.
//
// PKCE (RFC 7636) means tokens never traverse the URL bar — gotrue
// hands back an opaque `code`, which the callback exchanges via a
// server-to-server POST. Tokens stay off referer headers, server
// access logs, and browser history.
//
// GET /api/auth/oauth/{provider}?return_to=/some/path
func (s *Server) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	if s.authDeps == nil {
		http.NotFound(w, r)
		return
	}
	provider := r.PathValue("provider")
	if provider != "github" {
		http.Error(w, "unsupported provider", http.StatusNotFound)
		return
	}

	returnTo := normalizeReturnTo(r.URL.Query().Get("return_to"))

	csrfRaw := make([]byte, 16)
	if _, err := rand.Read(csrfRaw); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	codeVerifier, err := generatePKCEVerifier()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	state := stateClaims{
		ReturnTo:     returnTo,
		CSRF:         hex.EncodeToString(csrfRaw),
		CodeVerifier: codeVerifier,
		ExpiresAt:    timeNow().Add(10 * time.Minute).Unix(),
	}
	signed, err := state.sign(s.deployCfg.hmacKey)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
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

	q := url.Values{}
	q.Set("provider", provider)
	// PKCE wiring: gotrue accepts code_challenge + code_challenge_method
	// and flow_type=pkce on /authorize. After the provider dance, gotrue
	// redirects to redirect_to with ?code=<authcode>, which the callback
	// trades for tokens via /token?grant_type=pkce.
	q.Set("code_challenge", pkceChallenge(codeVerifier))
	q.Set("code_challenge_method", "S256")
	q.Set("flow_type", "pkce")
	q.Set("redirect_to", s.deployCfg.publicURL+"/api/auth/callback?state="+state.CSRF)
	target := s.deployCfg.publicURL + "/auth/v1/authorize?" + q.Encode()

	http.Redirect(w, r, target, http.StatusFound)
}

// handleOAuthCallback completes the PKCE dance: validates state,
// exchanges the auth code via server-side POST to gotrue's /token,
// verifies the returned JWT, upserts public.users, creates an
// encrypted session, sets the sid cookie, and redirects to return_to.
//
// GET /api/auth/callback?state=<csrf>&code=<auth_code>
func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if s.authDeps == nil {
		http.NotFound(w, r)
		return
	}

	stateCookie, err := r.Cookie(stateCookieName)
	if err != nil {
		http.Error(w, "missing state cookie", http.StatusBadRequest)
		return
	}
	state, err := parseStateCookie(stateCookie.Value, s.deployCfg.hmacKey)
	if err != nil {
		authLog.Warn("state cookie parse failed", "error", err)
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}
	if r.URL.Query().Get("state") != state.CSRF {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}

	// State done — clear the cookie. (Cookie's secure-flag derivation
	// must match the original SetCookie or the browser may keep both;
	// see s.cookieSecure for the per-request resolution.)
	http.SetCookie(w, &http.Cookie{
		Name: stateCookieName, Value: "", Path: "/api/auth/",
		MaxAge: -1, HttpOnly: true, Secure: s.cookieSecure(r), SameSite: http.SameSiteLaxMode,
	})

	authCode := r.URL.Query().Get("code")
	if authCode == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
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
		http.Error(w, "token exchange failed", http.StatusBadRequest)
		return
	}

	claims, err := s.authDeps.verifier.Verify(accessToken)
	if err != nil {
		authLog.Warn("verify callback jwt failed", "error", err)
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	userUUID, err := uuid.Parse(claims.Subject)
	if err != nil {
		http.Error(w, "invalid sub", http.StatusBadRequest)
		return
	}

	if err := upsertUserFromClaims(r.Context(), s.db, userUUID, claims); err != nil {
		authLog.Error("upsert user failed", "user", userUUID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Signup provisions nothing. Org creation is a deliberate user
	// action (the onboarding "Start your Factory" CTA → the create-org
	// flow), so the callback never mints a tenant. Resolve which org the
	// session should default to — the user's earliest existing
	// membership, or NULL for a first-time user, who lands at the
	// zero-membership onboarding entry. Whether that screen's create
	// affordance is enabled is governed separately by
	// runmode.OrgCreationEnabled() (TF_PREVENT_ORG_CREATION).
	defaultOrg, err := s.lookupEarliestMembership(r.Context(), s.db, userUUID)
	if err != nil {
		authLog.Error("resolve active org failed", "user", userUUID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
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
		authLog.Error("create session failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

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
		http.NotFound(w, r)
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
		http.NotFound(w, r)
		return
	}
	claims := ClaimsFrom(r.Context())
	if claims == nil {
		writeUnauth(w)
		return
	}
	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		http.Error(w, "invalid sub", http.StatusBadRequest)
		return
	}

	// List BEFORE revoking — we need the decrypted JWTs for upstream
	// logout calls. If we revoke first, the rows are filtered out
	// of the active-set query.
	active, err := s.authDeps.sessions.ListActiveForUserSystem(r.Context(), userID)
	if err != nil {
		authLog.Error("logout-all list failed", "user", userID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
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
		authLog.Error("revoke-all failed", "user", userID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	authLog.Info("logout-all revoked sessions", "user", userID, "count", n)

	// Every session is now revoked, so close ALL of this user's live
	// websocket connections (TFAC-75) — no sid/org filter. Each device's
	// client sees the session-revoked code and routes to /login. Logged
	// for the revocation audit trail (SOC2).
	kicked := s.ws.CloseUserConnections(userID.String(), "", "",
		websocket.CloseSessionRevoked, "session revoked")
	authLog.Info("kicked ws connections on logout-all",
		"user", userID, "code", int(websocket.CloseSessionRevoked), "n", kicked)

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
// SKY-313: the active-org primitive. We deliberately store the choice
// on the session row (single source of truth across tabs) rather than
// in the URL path. The next request's withSession reads the new value
// into ctxKeyOrgID; tab B sees the switch on its next round trip.
//
// 404-on-non-member mirrors withOrg's posture — don't disclose whether
// the org exists to a user who isn't in it.
func (s *Server) handleActiveOrgUpdate(w http.ResponseWriter, r *http.Request) {
	if s.authDeps == nil {
		http.NotFound(w, r)
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
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	orgUUID, err := uuid.Parse(body.OrgID)
	if err != nil {
		http.Error(w, "invalid org_id", http.StatusBadRequest)
		return
	}

	ok, err := s.az.UserHasOrgAccess(r.Context(), claims.Subject, orgUUID.String())
	if err != nil {
		authLog.Error("active-org membership check failed", "user", claims.Subject, "org", orgUUID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !ok {
		// 404 not 403 — same posture as withOrg.
		http.NotFound(w, r)
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
		authLog.Error("update active-org failed", "sid", sessions.LogID(sess.ID), "org", orgUUID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{}"))
}

// handleMe returns the authenticated user's identity + org list.
// Wrapped in SessionMiddleware at mount time. Response wire shape is
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
	type response struct {
		ID              string   `json:"id"`
		Email           string   `json:"email"`
		DisplayName     string   `json:"display_name,omitempty"`
		AvatarURL       string   `json:"avatar_url,omitempty"`
		GitHubUsername  string   `json:"github_username,omitempty"`
		JiraAccountID   string   `json:"jira_account_id,omitempty"`
		JiraDisplayName string   `json:"jira_display_name,omitempty"`
		Orgs            []orgRow `json:"orgs"`
		ActiveOrgID     string   `json:"active_org_id,omitempty"`
		// OrgCreationEnabled surfaces the instance's TF_PREVENT_ORG_CREATION
		// toggle (inverted) so the frontend onboarding entry can decide
		// whether to enable the "create your org" affordance or show the
		// invite-only "ask your admin" state, without a second round trip.
		// Not omitempty — false is a meaningful value the gate must see.
		OrgCreationEnabled bool `json:"org_creation_enabled"`
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
			// Local mode is N=1 with a pre-provisioned org and never
			// renders the onboarding entry, so the value is moot; report
			// the permissive default for shape consistency.
			OrgCreationEnabled: true,
		}
		// s.users is wired by New(). The middleware-shim unit test
		// constructs a bare &Server{} to exercise the sentinel path
		// without an auth stack — guard the lookups so that minimal
		// rig still works while production calls (always wired)
		// populate identity from the users row.
		if s.users != nil {
			// Identity is host-scoped (GitHub SKY-396, Jira SKY-397):
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
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	var resp response
	resp.Orgs = []orgRow{}
	resp.Email = claims.Email
	resp.OrgCreationEnabled = runmode.OrgCreationEnabled()
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
	// to the active org's GitHub host (SKY-396). A stale/empty active org
	// is harmless: org_settings RLS filters it out and the lookup falls
	// back to the most-recently-verified identity row.
	err := tfdb.WithTx(r.Context(), s.db,
		tfdb.Claims{Sub: claims.Subject, OrgID: resp.ActiveOrgID},
		func(tx *sql.Tx) error {
			// Identity is host-scoped for both providers (GitHub SKY-396,
			// Jira SKY-397): prefer the row bound to the active org's host
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
					                        SELECT os.github_base_url FROM org_settings os
					                         WHERE os.org_id = tf.current_org_id()
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
			                        SELECT os.jira_base_url FROM org_settings os
			                         WHERE os.org_id = tf.current_org_id()
			                      )), '/')) DESC NULLS LAST,
			                     ji.verified_at DESC NULLS LAST
			            LIMIT 1
			       ) j ON true
				 WHERE u.id = tf.current_user_id()
			`).Scan(&resp.ID, &resp.DisplayName, &resp.AvatarURL, &resp.GitHubUsername, &resp.JiraAccountID, &resp.JiraDisplayName); err != nil {
				return fmt.Errorf("user lookup: %w", err)
			}

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
		},
	)
	if err != nil {
		authLog.Error("/api/me failed", "user", claims.Subject, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
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

// upsertUserFromClaims mirrors the user's identity from JWT claims into
// public.users + public.user_github_identities. COALESCE preserves any field
// the claims happen to be missing — provider responses are inconsistent.
//
// Runs on the admin pool (main.go routes the raw *sql.DB to adminDB): the
// auth-callback PROVISIONING layer that *creates* the users row this login
// maps to (FK users.id → auth.users.id). That sits below the RLS/app-pool/
// store model on purpose — it mints the identity tf.current_user_id() later
// keys on. It can't route through UsersStore.UpsertGitHubIdentity even in
// principle: that runs under a claims tx requiring the users row to already
// exist (FK target; the runner rejects a userID with no row), and this is the
// function creating it. Both writes target the *verified-JWT* user's own rows
// (userID = claims.Subject), so the RLS bypass carries no untrusted target —
// the same trust model the retired github_username column write already used.
func upsertUserFromClaims(ctx context.Context, db *sql.DB, userID uuid.UUID, claims *verify.Claims) error {
	var displayName, avatarURL, ghUsername string
	if claims.UserMetadata != nil {
		displayName, _ = claims.UserMetadata["full_name"].(string)
		if displayName == "" {
			displayName, _ = claims.UserMetadata["name"].(string)
		}
		avatarURL, _ = claims.UserMetadata["avatar_url"].(string)
		ghUsername, _ = claims.UserMetadata["user_name"].(string)
		if ghUsername == "" {
			ghUsername, _ = claims.UserMetadata["preferred_username"].(string)
		}
	}

	// One admin-pool tx for both inserts so a users row never lands without
	// its identity row (or vice-versa); a mid-way failure rolls back and the
	// next login re-runs the idempotent upsert.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin user provisioning tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit succeeds

	// Mirror display_name + avatar_url onto the users row. github_username
	// moved out to user_github_identities (SKY-396); see the identity upsert
	// below.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO public.users (id, display_name, avatar_url, created_at, updated_at)
		VALUES ($1, NULLIF($2, ''), NULLIF($3, ''), now(), now())
		ON CONFLICT (id) DO UPDATE
		   SET display_name = COALESCE(EXCLUDED.display_name, public.users.display_name),
		       avatar_url   = COALESCE(EXCLUDED.avatar_url,   public.users.avatar_url),
		       updated_at   = now()
	`, userID, displayName, avatarURL); err != nil {
		return fmt.Errorf("upsert user: %w", err)
	}

	// GitHub-login claim → host-scoped identity row. The GoTrue
	// GitHub social provider is hardwired to github.com, so this binding is
	// always against github.com with source='login_claim' — the landmine the
	// Connect capture flow demotes (it silently NULLs the day login becomes
	// Entra SAML, but Connect/PAT fill the row when this can't). Skip when the
	// claim carries no username (non-GitHub login provider): the row stays
	// absent, preserving any previously-captured identity and honoring the
	// NULL-degrades contract.
	// The hardcoded host is already in NormalizeGitHubHost form; mirror
	// UsersStore.UpsertGitHubIdentity (verified_at = now(), upsert on the host
	// key) if that store method ever grows logic beyond the SQL.
	if ghUsername != "" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO public.user_github_identities
				(user_id, github_base_url, login, source, verified_at, created_at, updated_at)
			VALUES ($1, 'https://github.com', $2, 'login_claim', now(), now(), now())
			ON CONFLICT (user_id, github_base_url) DO UPDATE
			   SET login       = EXCLUDED.login,
			       source      = EXCLUDED.source,
			       verified_at = EXCLUDED.verified_at,
			       updated_at  = now()
		`, userID, ghUsername); err != nil {
			return fmt.Errorf("upsert github identity from login claim: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit user provisioning tx: %w", err)
	}
	return nil
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
	ExpiresAt    int64  `json:"exp"`
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
func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return r.Header.Get("X-Forwarded-Proto") == "https"
}

// clientIP best-effort extracts the requesting IP. Stored on the session
// row as `inet`, so the return value must be a valid Postgres `inet`
// literal — bracketed IPv6 (`[2001:db8::1]`) gets rejected.
//
// IPv6 `RemoteAddr` is the `[addr]:port` form; naive last-colon stripping
// returns `[addr]` with brackets intact, which then fails Postgres'
// `::inet` cast and 500s the OAuth callback. net.SplitHostPort handles
// both v4 + bracketed v6 correctly and unwraps the brackets.
func clientIP(r *http.Request) string {
	if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
		if i := strings.Index(xf, ","); i >= 0 {
			return strings.TrimSpace(xf[:i])
		}
		return strings.TrimSpace(xf)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	// No port present (rare — RemoteAddr almost always carries one in
	// HTTP). Return as-is and let the inet cast either accept it or
	// fail downstream with a clear error.
	return r.RemoteAddr
}
