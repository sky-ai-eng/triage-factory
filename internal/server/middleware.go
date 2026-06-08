package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/sessions"
)

// timeNow is package-var so middleware tests can stub the clock.
// Production callers use time.Now() via this seam.
var timeNow = time.Now

func unixToTime(unixSeconds int64) time.Time {
	return time.Unix(unixSeconds, 0).UTC()
}

// Request-context keys. Unexported type so callers must use the
// exported accessors below — prevents accidental shadowing.
type ctxKey int

const (
	ctxKeyClaims ctxKey = iota
	ctxKeySession
	ctxKeyOrgID
)

// ClaimsFrom returns the verified JWT claims set by SessionMiddleware,
// or nil if the request didn't pass through it. Handlers that depend
// on a claim should fail closed on nil; the middleware would have
// already rejected an unauthenticated request, so nil from this
// helper inside a protected handler indicates a registration bug.
func ClaimsFrom(ctx context.Context) *verify.Claims {
	v, _ := ctx.Value(ctxKeyClaims).(*verify.Claims)
	return v
}

// SessionFrom returns the resolved session row. Used by /api/auth/logout
// to know which sid to revoke without re-reading the cookie.
func SessionFrom(ctx context.Context) *sessions.Session {
	v, _ := ctx.Value(ctxKeySession).(*sessions.Session)
	return v
}

// OrgIDFrom returns the active org for the request. Sources, in order:
//
//   - URL-path {org_id} validated by withOrg against the caller's
//     memberships (used by org-scoped routes like /api/orgs/{org_id}/...);
//   - the session's active_org_id, populated by withSession in multi
//     mode from public.sessions.active_org_id (SKY-313);
//   - the hardcoded LocalDefaultOrgID set by the local-mode shim in
//     withSession.
//
// Empty string when the caller is multi-mode and has no active org
// (zero memberships) — handlers that require an org should 409 with a
// stable code so the SPA can prompt the user to pick/join one.
func OrgIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyOrgID).(string)
	return v
}

// authDeps groups the auth-stack dependencies a Server is wired with.
// Held by-pointer on the Server so a nil group cleanly signals
// "local mode, no auth surface" without scattering individual nil
// checks across every middleware/handler.
//
// The three gotrue* functions are abstracted as closures (not methods)
// so the test harness can stub each independently — the integration
// tests don't run a real gotrue, so the production HTTP calls become
// in-process stubs that return canned shapes.
type authDeps struct {
	verifier *verify.Verifier
	sessions *sessions.Store

	// gotrueRefresh performs the refresh-token dance when a JWT is
	// near expiry. Returns (newJWT, newRefresh, jwtExpiresAtUnix).
	gotrueRefresh func(ctx context.Context, refreshToken string) (newJWT string, newRefresh string, jwtExpiresAtUnix int64, err error)

	// gotrueExchange performs the PKCE auth-code exchange after the
	// provider dance. Returns (accessToken, refreshToken,
	// jwtExpiresAtUnix). Called from handleOAuthCallback.
	gotrueExchange func(ctx context.Context, authCode, codeVerifier string) (accessToken string, refreshToken string, jwtExpiresAtUnix int64, err error)

	// gotrueLogout asks gotrue to invalidate the refresh-token family
	// upstream. Called from handleLogout as best-effort — if it fails
	// we still revoke the row locally and clear the cookie.
	gotrueLogout func(ctx context.Context, accessToken string) error
}

// withSession wraps a handler in the session middleware. The check for
// authDeps==nil happens at REQUEST TIME (not at wrap time) because
// SetAuthDeps is called after routes() registers handlers — capturing
// nil at wrap time would leave the wrapper inert for the entire process
// lifetime even after deps land.
//
// Local-mode behavior: when authDeps stays nil AND the process booted
// in ModeLocal, the wrapper injects sentinel identity values into the
// request context — a synthetic *verify.Claims carrying
// runmode.LocalDefaultUserID as Subject, and ctxKeyOrgID =
// runmode.LocalDefaultOrgID. Handlers then read identity uniformly via
// ClaimsFrom + OrgIDFrom without branching on mode. The "local equals
// multi at N=1" framing: handler code structure stays identical in
// both modes; local mode just threads the sentinel rows everywhere a
// real org/user UUID would otherwise flow.
//
// Multi-mode with nil authDeps is treated as a transient boot state
// (SetAuthDeps lands after routes() registers handlers) and falls
// through to the prior pass-through behavior. Downstream handlers that
// require real claims (e.g. /api/me) detect the missing claim and 401;
// masquerading as a sentinel-authed multi-mode request would be
// strictly worse than 401.
func (s *Server) withSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authDeps == nil {
			if runmode.Current() == runmode.ModeLocal {
				ctx := context.WithValue(r.Context(), ctxKeyClaims, &verify.Claims{Subject: runmode.LocalDefaultUserID})
				ctx = context.WithValue(ctx, ctxKeyOrgID, runmode.LocalDefaultOrgID)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		cookie, err := r.Cookie(s.sidCookieName())
		if err != nil {
			writeUnauth(w)
			return
		}
		sid, err := uuid.Parse(cookie.Value)
		if err != nil {
			writeUnauth(w)
			return
		}

		sess, err := s.authDeps.sessions.LookupSystem(r.Context(), sid)
		if err != nil {
			log.Printf("[auth] session lookup: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if sess == nil {
			writeUnauth(w)
			return
		}

		// Refresh inline if the JWT is within the refresh window
		// (60s). Failing the refresh forces re-login — better to
		// surface the failure now than to verify against an
		// already-expired JWT and 401 anyway.
		if needsRefresh(sess) {
			if err := s.refreshSessionInline(r.Context(), sess); err != nil {
				log.Printf("[auth] refresh failed for sid=%s: %v", sessions.LogID(sid), err)
				writeUnauth(w)
				return
			}
		}

		claims, err := s.authDeps.verifier.Verify(sess.JWT)
		if err != nil {
			// Either the JWT decrypted cleanly but failed verification
			// (rotated signing key, replay across issuers) — in either
			// case the session is unrecoverable. 401.
			log.Printf("[auth] verify failed for sid=%s: %v", sessions.LogID(sid), err)
			writeUnauth(w)
			return
		}

		// Best-effort last-seen bump; intentionally backgrounded so
		// the slow DB doesn't lengthen the request critical path.
		// Errors are logged inside the goroutine.
		//
		// context.Background() is safe here: the underlying
		// sessions.Store is constructed with the admin pool (see the
		// type docstring) because session validation is the auth path
		// itself and can't depend on the very claims it's validating.
		// No JWT-claims context is needed for the UPDATE.
		go func(id uuid.UUID) {
			if err := s.authDeps.sessions.TouchLastSeenSystem(context.Background(), id); err != nil {
				log.Printf("[auth] touch last_seen for sid=%s: %v", sessions.LogID(id), err)
			}
		}(sid)

		ctx := context.WithValue(r.Context(), ctxKeyClaims, claims)
		ctx = context.WithValue(ctx, ctxKeySession, sess)
		// SKY-313: surface the session's active org so OrgIDFrom() works
		// uniformly in multi mode without per-handler plumbing. Sessions
		// whose user has zero memberships carry NULL here — we leave
		// ctxKeyOrgID unset and handlers that require an org return 409.
		if sess.ActiveOrgID.Valid {
			ctx = context.WithValue(ctx, ctxKeyOrgID, sess.ActiveOrgID.UUID.String())
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// withOrg wraps a handler in the org-membership check. Reads
// r.PathValue("org_id"), confirms the caller is a member, and 404s
// otherwise (404 not 403 — don't leak the org's existence).
//
// Must be composed *after* withSession; uses ClaimsFrom to read the
// caller's sub. Routes without {org_id} in the pattern pass through
// unchanged.
func (s *Server) withOrg(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		orgID := r.PathValue("org_id")
		if orgID == "" {
			next.ServeHTTP(w, r)
			return
		}
		// Cheap validation before the DB hit — malformed UUID in the
		// path is a 404 (same response as "no such org").
		if _, err := uuid.Parse(orgID); err != nil {
			http.NotFound(w, r)
			return
		}
		claims := ClaimsFrom(r.Context())
		if claims == nil {
			// Programmer error: withOrg mounted without withSession.
			// Don't reveal the misconfiguration to the caller.
			log.Printf("[auth] withOrg saw no claims — route missing withSession wrapper: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		ok, err := s.az.userHasOrgAccess(r.Context(), claims.Subject, orgID)
		if err != nil {
			log.Printf("[auth] membership check %s/%s: %v", claims.Subject, orgID, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyOrgID, orgID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// needsRefresh is true when the JWT will expire within the refresh
// window (60s). Keeps the threshold in one place; tests can shadow it.
func needsRefresh(sess *sessions.Session) bool {
	const refreshWindowSeconds = 60
	return sess.JWTExpiresAt.Unix()-nowUnix() < refreshWindowSeconds
}

// nowUnix is var-able so tests can shift the clock.
var nowUnix = func() int64 {
	return timeNow().Unix()
}

// refreshTokens is the shared result type for refreshSessionInline.
// singleflight returns one of these to every concurrent waiter so each
// goroutine can splice the new values into its own local *Session.
type refreshTokens struct {
	jwt, refresh string
	jwtExp       time.Time
}

// refreshSessionInline coordinates concurrent refresh attempts via
// singleflight: at most one goroutine per session ID executes the
// gotrue refresh dance; all concurrent waiters receive the same
// result and avoid a duplicate gotrue call (which would fail anyway —
// GoTrue rotates the refresh-token family on first use).
//
// Why singleflight over a per-session mutex:
//   - The Group clears its key once the in-flight call returns, so
//     there's no per-session entry accumulating over process lifetime
//     (the prior sync.Map[uuid]*Mutex grew monotonically).
//   - Concurrent waiters get the result directly without a second DB
//     re-fetch under the lock.
//
// The re-fetch inside fn is still load-bearing: a refresh from another
// path (different endpoint hitting middleware) might have landed
// between this request's initial Lookup in withSession and the Do
// entry here. If so, fn skips the gotrue call.
//
// The *sess passed in is mutated in place so subsequent middleware
// steps (verifier.Verify) see the fresh JWT.
//
// fn runs with a detached context, not the caller's. singleflight binds
// the function's execution to whichever goroutine wins the race; if
// THAT caller disconnects while N other waiters are still connected,
// using the winner's ctx would cancel the gotrue call and 401 every
// waiter — including ones whose own request is still live. The detach
// + 35s timeout (margin above gotrueHTTPClient.Timeout's 30s) keeps the
// refresh independent of any single caller's lifetime.
func (s *Server) refreshSessionInline(ctx context.Context, sess *sessions.Session) error {
	if s.authDeps == nil || s.authDeps.gotrueRefresh == nil {
		return errors.New("refresh not wired")
	}
	_ = ctx // caller's ctx intentionally not propagated into fn; see comment above.

	v, err, _ := s.refreshGroup.Do(sess.ID.String(), func() (any, error) {
		// Detached background ctx with a 35s hard cap so a stuck refresh
		// can't pin the singleflight group forever. The sessions.Lookup /
		// UpdateJWT calls inside route through the admin pool by Store
		// construction (see internal/sessions docstring) — no JWT
		// claims required for the refresh dance.
		fnCtx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cancel()

		fresh, err := s.authDeps.sessions.LookupSystem(fnCtx, sess.ID)
		if err != nil {
			return nil, fmt.Errorf("re-fetch session: %w", err)
		}
		if fresh == nil {
			return nil, errors.New("session revoked during refresh wait")
		}
		if !needsRefresh(fresh) {
			// Another path already refreshed this session. Hand the
			// fresh tokens back without calling gotrue.
			return refreshTokens{jwt: fresh.JWT, refresh: fresh.RefreshToken, jwtExp: fresh.JWTExpiresAt}, nil
		}

		newJWT, newRefresh, newExp, err := s.authDeps.gotrueRefresh(fnCtx, fresh.RefreshToken)
		if err != nil {
			return nil, err
		}
		newExpTime := unixToTime(newExp)
		if err := s.authDeps.sessions.UpdateJWTSystem(fnCtx, sess.ID, newJWT, newRefresh, newExpTime); err != nil {
			return nil, err
		}
		return refreshTokens{jwt: newJWT, refresh: newRefresh, jwtExp: newExpTime}, nil
	})
	if err != nil {
		return err
	}
	r := v.(refreshTokens)
	sess.JWT = r.jwt
	sess.RefreshToken = r.refresh
	sess.JWTExpiresAt = r.jwtExp
	return nil
}

// withCSRFOriginCheck rejects mutating requests (POST/PUT/PATCH/DELETE)
// whose Origin header doesn't match the configured publicURL. Browsers
// always send Origin on cross-origin requests, so this catches the
// gap that SameSite=Lax leaves (which permits top-level cross-site
// POSTs to the request URL).
//
// deployCfg nil: pass-through (shouldn't happen after boot, but safe).
//
// Same-origin requests that omit Origin (rare; some old browsers,
// fetch() in non-CORS modes) are allowed: a missing Origin can't
// indicate cross-site since cross-site mutating requests must set it.
func (s *Server) withCSRFOriginCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.deployCfg == nil {
			next.ServeHTTP(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}
		if origin != s.deployCfg.publicURL {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sidCookieName resolves at request time: __Host-sid for HTTPS
// deployments (browser-enforced: Secure flag required, Path=/, no
// Domain), plain sid otherwise. Local-dev / tests run over HTTP and
// would have the browser silently drop a __Host- cookie that doesn't
// also carry Secure.
func (s *Server) sidCookieName() string {
	if s.deployCfg != nil && s.deployCfg.secureCookies {
		return "__Host-sid"
	}
	return "sid"
}

// cookieSecure derives whether to mark a cookie Secure. True if the
// deployment is HTTPS (publicURL starts with https://) OR the
// individual request arrived over TLS. The latter covers reverse-
// proxy deployments where TLS termination happens upstream and the
// Go server sees plain HTTP — X-Forwarded-Proto = https.
func (s *Server) cookieSecure(r *http.Request) bool {
	if s.deployCfg != nil && s.deployCfg.secureCookies {
		return true
	}
	return isHTTPS(r)
}

func writeUnauth(w http.ResponseWriter) {
	http.Error(w, "unauthenticated", http.StatusUnauthorized)
}
