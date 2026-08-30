package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/sky-ai-eng/triage-factory/internal/apitokens"
	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/sessions"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
)

// timeNow is package-var so middleware tests can stub the clock.
// Production callers use time.Now() via this seam.
var timeNow = time.Now

func unixToTime(unixSeconds int64) time.Time {
	return time.Unix(unixSeconds, 0).UTC()
}

// Request-context key for the resolved session row. The verified claims and
// the active org now live in internal/server/httpx (read via httpx.ClaimsFrom
// / httpx.OrgIDFrom, forwarded under their short names in http_helpers.go);
// the session stays here because only the auth and orgs handlers read it.
// Unexported type so callers must use SessionFrom.
type ctxKey int

const ctxKeySession ctxKey = iota

// SessionFrom returns the resolved session row. Used by /api/auth/logout
// to know which sid to revoke without re-reading the cookie.
func SessionFrom(ctx context.Context) *sessions.Session {
	v, _ := ctx.Value(ctxKeySession).(*sessions.Session)
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

	// apiTokens is the Bearer half of the same identity story: a token is a
	// session cursor with its org fixed at mint, so it resolves the same
	// (user, active org) pair the cookie path resolves and lands beside it
	// rather than in a parallel dependency of its own.
	apiTokens *apitokens.Store

	// gotrueRefresh performs the refresh-token dance when a JWT is
	// near expiry. Returns (newJWT, newRefresh, jwtExpiresAtUnix).
	gotrueRefresh func(ctx context.Context, refreshToken string) (newJWT string, newRefresh string, jwtExpiresAtUnix int64, err error)

	// gotrueExchange performs the PKCE auth-code exchange after the
	// provider dance. Returns (accessToken, refreshToken,
	// jwtExpiresAtUnix). Called from handleOAuthCallback.
	gotrueExchange func(ctx context.Context, authCode, codeVerifier string) (accessToken string, refreshToken string, jwtExpiresAtUnix int64, err error)

	// gotrueSSO drives the SP-initiated SAML start: POST gotrue's public
	// /sso and return the 303 Location (the IdP redirect) to forward to
	// the browser. Called from StartSSO. SAML-only; the GitHub flow
	// uses a plain /authorize browser redirect, not this.
	gotrueSSO func(ctx context.Context, providerID, redirectTo, codeChallenge string) (location string, err error)

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
// runmode.LocalDefaultUserID as Subject, and the active org =
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
				ctx := httpx.WithClaims(r.Context(), &verify.Claims{Subject: runmode.LocalDefaultUserID})
				ctx = httpx.WithOrgID(ctx, runmode.LocalDefaultOrgID)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		// The Bearer path is the cookie's peer, not its fallback: ANY
		// Authorization header sends the request here and it is decided here,
		// however it fails. Falling back to the cookie on a bad token would
		// mean a request that named a credential got served under a different,
		// ambient one — the same result-set-widening bug class as a filter that
		// opens up when it can't parse its input.
		if header := r.Header.Get("Authorization"); header != "" {
			s.serveTokenAuth(w, r, next, header)
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

		// The dominant per-request auth cost: every authenticated request
		// pays this admin-pool read before any handler work begins, and
		// otelsql's statement span alone wouldn't say who asked.
		lookupCtx, lookupSpan := tracer.Start(r.Context(), "session.lookup")
		sess, err := s.authDeps.sessions.LookupSystem(lookupCtx, sid)
		switch {
		case err != nil:
			lookupSpan.SetStatus(codes.Error, "lookup failed")
		case sess == nil:
			// Not an error: an expired or revoked cookie is how a
			// session normally ends.
			lookupSpan.SetAttributes(telemetry.Outcome("not_found"))
		default:
			lookupSpan.SetAttributes(telemetry.Outcome("found"))
		}
		lookupSpan.End()
		if err != nil {
			httpx.InternalError(w, "auth", fmt.Errorf("session lookup: %w", err))
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
				authLog.Warn("refresh failed", "sid", sessions.LogID(sid), "error", err)
				s.recordRefreshFailure(r, sess.UserID, sid, "refresh_failed")
				writeUnauth(w)
				return
			}
		}

		// CPU-bound signature checking against a cached JWKS — except on
		// a cache miss, when it fetches. That case is the whole reason
		// this gets a span rather than being absorbed into the request's.
		_, verifySpan := tracer.Start(r.Context(), "jwt.verify")
		claims, err := s.authDeps.verifier.Verify(sess.JWT)
		if err != nil {
			verifySpan.SetStatus(codes.Error, "verify failed")
		}
		verifySpan.End()
		if err != nil {
			// Either the JWT decrypted cleanly but failed verification
			// (rotated signing key, replay across issuers) — in either
			// case the session is unrecoverable. 401. This is the ONLY
			// jwt_verify_failure write-site — the anonymous missing/invalid-
			// cookie 401s above are poke noise and stay uninstrumented.
			authLog.Warn("verify failed", "sid", sessions.LogID(sid), "error", err)
			s.recordJWTVerifyFailure(r, sess.UserID, sid, "verify_failed")
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
				authLog.Warn("touch last_seen failed", "sid", sessions.LogID(id), "error", err)
			}
		}(sid)

		// The verified JWT's sub is the GoTrue login IDENTITY; TF authorizes on
		// the PRINCIPAL. The session carries the principal (resolved at the OAuth
		// callback), so key RLS + every handler on it. claims.Email stays the
		// identity's email, for display.
		//
		// Stash the login-identity id (the JWT sub == user_identities.auth_user_id)
		// before the remap, so a personal read like GET /api/me/identities can mark
		// which of the principal's N linked identities backs THIS session — the
		// remapped principal can't say which, and linked identities often share an
		// email, so email is not a reliable discriminator.
		authIdentityID := claims.Subject
		claims.Subject = sess.UserID.String()

		ctx := httpx.WithClaims(r.Context(), claims)
		ctx = httpx.WithAuthIdentityID(ctx, authIdentityID)
		ctx = context.WithValue(ctx, ctxKeySession, sess)
		// Surface the session's active org so OrgIDFrom() works
		// uniformly in multi mode without per-handler plumbing. Sessions
		// whose user has zero memberships carry NULL here — we leave
		// the active org unset and handlers that require an org return 409.
		if sess.ActiveOrgID.Valid {
			ctx = httpx.WithOrgID(ctx, sess.ActiveOrgID.UUID.String())
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// bearerScheme is the one Authorization scheme this server accepts, matched
// case-insensitively.
const bearerScheme = "Bearer"

// bearerCredential returns the credential from an Authorization header value
// when it is a well-formed Bearer, and ok=false for anything else — another
// scheme, no separating space, or a scheme with nothing after it. Callers that
// only need to know whether a request carries a Bearer at all read the second
// return value and drop the first.
//
// The separator is one OR MORE spaces, which is the grammar both RFC 7235
// (`auth-scheme 1*SP token68`) and RFC 6750 write. A client that doubles the
// space has sent a valid request with an unambiguous credential in it, so
// folding the extra into the secret — where it could only ever fail to match a
// hash — would be us answering 401 to something we are required to accept.
// Only spaces count: a tab is not SP, and a header split on one is malformed.
func bearerCredential(header string) (string, bool) {
	if len(header) <= len(bearerScheme) || !strings.EqualFold(header[:len(bearerScheme)], bearerScheme) {
		return "", false
	}
	rest := header[len(bearerScheme):]
	credential := strings.TrimLeft(rest, " ")
	if len(credential) == len(rest) || credential == "" {
		// Nothing separated the scheme from what follows (so this is some
		// longer scheme name, not Bearer), or nothing followed it at all.
		return "", false
	}
	return credential, true
}

// apiTokenCredential returns the API-token credential an Authorization header
// carries, and ok=false when it carries none: another scheme, a malformed
// Bearer, or a Bearer holding something that was never one of this server's
// tokens. The "tf_" prefix is part of the question rather than a validation
// step after it — a string without it cannot match the hash of a token, so
// nothing may treat it as one.
//
// This is the single definition of "presents an API token", shared by the path
// that resolves one and the guards that refuse one, so the two cannot drift
// into disagreeing about what a token even looks like.
func apiTokenCredential(header string) (string, bool) {
	raw, ok := bearerCredential(header)
	if !ok || !strings.HasPrefix(raw, apitokens.Prefix) {
		return "", false
	}
	return raw, true
}

// refuseTokenAuth answers 403 and returns true when the request is driven by an
// API token, for a verb whose subject IS the session: moving the active-org
// cursor, ending a session, accepting an invite into a different org. Those are
// not permissions a token happens to lack — a token has no cursor to move and
// no session to end, so there is nothing for the verb to act on.
//
// 403 rather than 401, because the credential authenticated fine: the caller is
// who they say they are, and this is simply not a thing their kind of
// credential does. A 401 would send an automation off to re-authenticate a
// token that is working perfectly. The message names the session requirement so
// the answer is actionable rather than a bare refusal.
//
// Two signals, because the guarded routes do not all resolve their credential
// the same way. TokenAuthFrom is the authoritative one, set by the middleware
// that actually resolved the token; on a route behind withSession it is the
// only one that can fire, since a Bearer that failed there never reached a
// handler. POST /api/auth/logout is the exception it exists for: that route
// deliberately sits outside withSession — it reads the sid cookie directly so a
// stale session can still be logged out — so nothing has resolved anything by
// the time it runs, and the header is all there is to read.
//
// That fallback asks for OUR token's shape, not for a Bearer. A credential this
// server does not accept is not an API token, and answering "you need a
// session" to one would both be untrue and change what a caller who never used
// a token gets from a route that has always ignored the header.
func refuseTokenAuth(w http.ResponseWriter, r *http.Request, requirement string) bool {
	if httpx.TokenAuthFrom(r.Context()) == nil {
		if _, presentsToken := apiTokenCredential(r.Header.Get("Authorization")); !presentsToken {
			return false
		}
	}
	forbidden(w, requirement)
	return true
}

// serveTokenAuth is withSession's Bearer branch: it resolves an API token to
// the (user, org) pair a session cookie would have resolved to, and seeds the
// same request context so every handler downstream is unaware of which
// credential arrived. Multi-mode only — local mode never reaches here, because
// withSession's synthetic-identity branch returns before the header is read.
//
// Every way this can fail — unknown scheme, no "tf_" prefix, no matching row,
// revoked, past its stored expiry, past the org's current max-age cap, or an
// address outside the token's allowlist — answers the same 401. The refusals
// are deliberately indistinguishable: telling a caller which one applied tells
// them how close they got.
//
// What this does NOT do, deliberately: re-check the principal's org membership,
// their seat, or whether the org has since turned on SSO. That is not an
// omission, it is parity — the cookie path checks the same things exactly once,
// at mint, and leaves per-request enforcement to the RLS membership predicates
// every read runs under, the authz probes each gate re-runs, and revocation at
// the moment access is taken away (org removal revokes the org's tokens beside
// its sessions). A per-request membership read here would buy a few seconds of
// latency on a removal the revoke already handles, at the cost of a database
// round-trip on every headless call.
func (s *Server) serveTokenAuth(w http.ResponseWriter, r *http.Request, next http.Handler, header string) {
	if s.authDeps.apiTokens == nil {
		// A wiring gap, not a caller fault: refuse rather than fall through to
		// a route that would then run with no identity at all. Nothing is
		// charged to the caller's failure budget for our own missing store.
		authLog.Warn("bearer auth attempted with no api-token store wired")
		writeUnauth(w)
		return
	}

	// The failure budget is consulted BEFORE the lookup and spent only after
	// one fails, so a valid token is never rate-limited however fast it calls.
	// Same client-IP resolution as the limiter's own keying and as the CIDR
	// check below — one answer to "who is calling" for all three.
	ip := clientIP(r)
	limiter := s.tokenAuthFailureLimiter
	if limiter != nil {
		if ok, retryAfter := limiter.peek(ip); !ok {
			writeRateLimited(w, retryAfter)
			return
		}
	}
	refuse := func() {
		if limiter != nil {
			limiter.charge(ip)
		}
		writeUnauth(w)
	}

	raw, ok := apiTokenCredential(header)
	if !ok {
		// Shape-check before the round-trip: a string that was never a token
		// cannot match a hash of one, so there is nothing to ask the database.
		refuse()
		return
	}

	// The Bearer path's equivalent of session.lookup, and the same reason for
	// the span: it is the whole per-request auth cost, and the statement span
	// underneath it wouldn't say what the read was for.
	lookupCtx, lookupSpan := tracer.Start(r.Context(), "apitoken.lookup")
	identity, err := s.authDeps.apiTokens.LookupSystem(lookupCtx, raw)
	switch {
	case err != nil:
		lookupSpan.SetStatus(codes.Error, "lookup failed")
	case identity == nil:
		// Not an error: an expired or revoked token is how a credential
		// normally ends.
		lookupSpan.SetAttributes(telemetry.Outcome("not_found"))
	default:
		lookupSpan.SetAttributes(telemetry.Outcome("found"))
	}
	lookupSpan.End()
	if err != nil {
		// A database failure is not "no such token": answering 401 here would
		// tell a caller holding a good credential that it had been revoked.
		httpx.InternalError(w, "auth", fmt.Errorf("api token lookup: %w", err))
		return
	}
	if identity == nil {
		refuse()
		return
	}
	if !addrInCIDRs(ip, identity.AllowedCIDRs) {
		refuse()
		return
	}

	// Best-effort last-used bump, backgrounded for the same reason the
	// session path backgrounds its last-seen write: the store is admin-pool
	// by construction and the request must not wait on bookkeeping.
	go func(tokenID string) {
		if err := s.authDeps.apiTokens.TouchLastUsedSystem(context.Background(), tokenID); err != nil {
			authLog.Warn("touch last_used failed", "token_id", tokenID, "error", err)
		}
	}(identity.TokenID)

	// The claims a token synthesizes carry only what a token knows: the
	// principal it acts as and that principal's email, for display. There is no
	// GoTrue login behind this request, so no AuthIdentityID is injected — a
	// handler that needs one is session-only and says so at its own door.
	ctx := httpx.WithClaims(r.Context(), &verify.Claims{
		Subject: identity.UserID,
		Email:   identity.Email,
	})
	// The token's org is the request's tenant, injected exactly where the
	// session path injects the session cursor's active org. That is what makes
	// the ~130 cursor-scoped routes work Bearer-only with no re-plumbing.
	ctx = httpx.WithOrgID(ctx, identity.OrgID)
	ctx = httpx.WithTokenAuth(ctx, &httpx.TokenAuth{
		TokenID: identity.TokenID,
		OrgID:   identity.OrgID,
	})
	next.ServeHTTP(w, r.WithContext(ctx))
}

// addrInCIDRs reports whether ip falls inside a token's allowlist. An empty
// allowlist is no restriction, so it admits everything; a non-empty one that
// cannot be evaluated — an unparseable address, or none at all because
// TF_CAPTURE_CLIENT_IP is off — admits nothing. Failing closed is the only
// honest reading of "only from these addresses" when the address is unknown.
//
// Addresses are unmapped before matching: a dual-stack listener hands Go
// ::ffff:10.0.0.1 for a v4 client, and netip.Prefix.Contains refuses a
// cross-family comparison, so without this an operator's 10.0.0.0/8 would
// silently match nothing. An entry that fails to parse is skipped rather than
// treated as a match — the column stores canonical ranges, so this is
// unreachable in practice, and skipping keeps a corrupt row from widening the
// allowlist.
func addrInCIDRs(ip string, cidrs []string) bool {
	if len(cidrs) == 0 {
		return true
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			continue
		}
		if p.Contains(addr) {
			return true
		}
	}
	return false
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
	// The caller's ctx still must not reach fn (see above), but a span
	// context is a value, not a cancellation, so capturing it costs that
	// detachment nothing. The refresh span LINKS to it rather than
	// parenting under it, for the same reason the ctx is dropped: the
	// refresh outlives whichever request won the race, and the N-1
	// waiters are just as much its callers as the winner.
	requester := trace.SpanContextFromContext(ctx)

	v, err, _ := s.refreshGroup.Do(sess.ID.String(), func() (any, error) {
		// Detached background ctx with a 35s hard cap so a stuck refresh
		// can't pin the singleflight group forever. The sessions.Lookup /
		// UpdateJWT calls inside route through the admin pool by Store
		// construction (see internal/sessions docstring) — no JWT
		// claims required for the refresh dance.
		fnCtx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cancel()

		var startOpts []trace.SpanStartOption
		if requester.IsValid() {
			startOpts = append(startOpts, trace.WithLinks(trace.Link{SpanContext: requester}))
		}
		fnCtx, span := tracer.Start(fnCtx, "session.refresh", startOpts...)
		defer span.End()

		fresh, err := s.authDeps.sessions.LookupSystem(fnCtx, sess.ID)
		if err != nil {
			span.SetStatus(codes.Error, "re-fetch failed")
			return nil, fmt.Errorf("re-fetch session: %w", err)
		}
		if fresh == nil {
			span.SetStatus(codes.Error, "session revoked")
			return nil, errors.New("session revoked during refresh wait")
		}
		if !needsRefresh(fresh) {
			// Another path already refreshed this session. Hand the
			// fresh tokens back without calling gotrue.
			span.SetAttributes(telemetry.Outcome("already_fresh"))
			return refreshTokens{jwt: fresh.JWT, refresh: fresh.RefreshToken, jwtExp: fresh.JWTExpiresAt}, nil
		}

		newJWT, newRefresh, newExp, err := s.authDeps.gotrueRefresh(fnCtx, fresh.RefreshToken)
		if err != nil {
			span.SetStatus(codes.Error, "gotrue refresh failed")
			return nil, err
		}
		newExpTime := unixToTime(newExp)
		if err := s.authDeps.sessions.UpdateJWTSystem(fnCtx, sess.ID, newJWT, newRefresh, newExpTime); err != nil {
			span.SetStatus(codes.Error, "persist failed")
			return nil, err
		}
		span.SetAttributes(telemetry.Outcome("refreshed"))
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

// devFrontendOrigin is the Vite dev server's origin (frontend/vite.config.ts
// pins server.port 5173 with strictPort so this stays valid). Trusted by
// withCSRFOriginCheck only when deployCfg.trustDevFrontendOrigin is set
// (local mode only, via AllowDevFrontendOrigin) — never in multi mode.
//
// This is a deliberate, narrow trust expansion for local-dev convenience,
// not a strict equivalent of publicURL: a remote website can't forge this
// Origin (the browser stamps it from the page's real origin), so a
// cross-site attacker is still rejected — but ANY local process that binds
// :5173 and serves a page, not just Vite, would also be trusted here.
// That's accepted only because local mode already assumes a single
// trusted user on a single trusted machine; it is not a generally-safe
// pattern and must not be widened beyond this one dev-only port.
const devFrontendOrigin = "http://localhost:5173"

// withCSRFOriginCheck rejects mutating requests (POST/PUT/PATCH/DELETE)
// whose Origin header doesn't match the configured publicURL (or, in local
// mode with the dev frontend running, devFrontendOrigin — see above).
// Browsers always send Origin on cross-origin requests, so this catches the
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
		// A Bearer request needs no origin check. This wrapper runs OUTSIDE
		// withSession, so it sees the header and not whether the token behind
		// it is any good — and that is sufficient, because what CSRF defends
		// against is a browser attaching an AMBIENT credential to a request
		// the user's page did not intend, and a cross-site browser context
		// cannot attach an Authorization header at all without a CORS
		// preflight this server never approves. So the header's presence
		// already says the request was made deliberately by its sender.
		// Whether the credential is good is withSession's decision, one layer
		// in, and a bad one is refused there rather than here.
		if _, ok := bearerCredential(r.Header.Get("Authorization")); ok {
			next.ServeHTTP(w, r)
			return
		}
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}
		if origin != s.deployCfg.publicURL {
			if !s.deployCfg.trustDevFrontendOrigin || origin != devFrontendOrigin {
				httpx.WriteErrors(w, http.StatusForbidden, httpx.ErrorItem{
					Reason: httpx.ReasonForbidden, Message: "cross-origin request rejected",
				})
				return
			}
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
