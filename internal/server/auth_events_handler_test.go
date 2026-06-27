package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
)

// These tests prove the core auth write-sites emit the right auth_events row, and
// the headline best-effort contract: a failing audit store never breaks the auth
// flow (login still completes, logout still 204s) and logs an ERROR instead.
// They reuse the white-box authRig in auth_handlers_test.go (pgtest-backed), so
// they skip cleanly when Docker is unavailable. TFAC-76.

// capturedAuthEvent is one auth_events row read back for assertion (detail parsed
// from jsonb, whose text form is normalized by Postgres).
type capturedAuthEvent struct {
	userID, orgID, sessionID string
	detail                   map[string]any
}

// authEventsByType reads every auth_events row of the given type, oldest-first.
func (r *authRig) authEventsByType(eventType string) []capturedAuthEvent {
	r.t.Helper()
	rows, err := r.h.AdminDB.Query(`
		SELECT COALESCE(user_id::text, ''), COALESCE(org_id::text, ''),
		       COALESCE(session_id::text, ''), COALESCE(detail_json::text, '')
		  FROM auth_events WHERE event_type = $1 ORDER BY created_at`, eventType)
	if err != nil {
		r.t.Fatalf("query auth_events(%s): %v", eventType, err)
	}
	defer rows.Close()
	var out []capturedAuthEvent
	for rows.Next() {
		var ce capturedAuthEvent
		var detail string
		if err := rows.Scan(&ce.userID, &ce.orgID, &ce.sessionID, &detail); err != nil {
			r.t.Fatalf("scan auth_event: %v", err)
		}
		if detail != "" {
			if err := json.Unmarshal([]byte(detail), &ce.detail); err != nil {
				r.t.Fatalf("auth_event detail not valid json: %q (%v)", detail, err)
			}
		}
		out = append(out, ce)
	}
	return out
}

func (r *authRig) oneAuthEvent(eventType string) capturedAuthEvent {
	r.t.Helper()
	got := r.authEventsByType(eventType)
	if len(got) != 1 {
		r.t.Fatalf("auth_events(%s) = %d rows, want exactly 1", eventType, len(got))
	}
	return got[0]
}

// failingAuthEventStore is an AuthEventStore whose RecordSystem always fails —
// injected to prove the best-effort contract.
type failingAuthEventStore struct{}

func (failingAuthEventStore) RecordSystem(context.Context, domain.AuthEvent) error {
	return errSimulated
}
func (failingAuthEventStore) ListByOrgSystem(context.Context, string, domain.AuthEventListOpts) ([]domain.AuthEvent, error) {
	return nil, nil
}
func (failingAuthEventStore) ListByUserSystem(context.Context, string, domain.AuthEventListOpts) ([]domain.AuthEvent, error) {
	return nil, nil
}

// A successful login records login_success with the principal, the defaulted org,
// the new sid, and {method:github, sso:false}.
func TestAuthEvents_LoginSuccess_Recorded(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "alice-org")

	resp, _ := r.driveCallback(userID)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login status=%d, want 302", resp.StatusCode)
	}
	sid := r.sidFromResp(resp)

	ev := r.oneAuthEvent(domain.AuthEventLoginSuccess)
	if ev.userID != userID.String() {
		t.Errorf("login_success user=%q, want %q", ev.userID, userID)
	}
	if ev.orgID != orgID.String() {
		t.Errorf("login_success org=%q, want %q", ev.orgID, orgID)
	}
	if ev.sessionID != sid {
		t.Errorf("login_success session=%q, want %q", ev.sessionID, sid)
	}
	if ev.detail["method"] != "github" || ev.detail["sso"] != false {
		t.Errorf("login_success detail=%v, want method=github sso=false", ev.detail)
	}
}

// A state-mismatch callback records ONE login_failure carrying the rejecting
// branch + method (state parsed, so method is known), and mints no session.
func TestAuthEvents_LoginFailure_StateMismatch_Recorded(t *testing.T) {
	r := newAuthRig(t)

	stateCookie := r.signStateCookie("/dashboard", "the-real-csrf", "verifier")
	q := url.Values{}
	q.Set("state", "different-csrf")
	q.Set("code", "any-code")
	req := httptest.NewRequest("GET", "/api/auth/callback?"+q.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: stateCookie})
	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("state mismatch status=%d, want 400", rec.Code)
	}

	ev := r.oneAuthEvent(domain.AuthEventLoginFailure)
	if ev.detail["reason"] != "state_mismatch" {
		t.Errorf("login_failure reason=%v, want state_mismatch", ev.detail["reason"])
	}
	if ev.detail["method"] != "github" {
		t.Errorf("login_failure method=%v, want github", ev.detail["method"])
	}
	if ev.userID != "" {
		t.Errorf("login_failure carried a user=%q, want none (pre-identity)", ev.userID)
	}
}

// Logout records a logout event tied to the revoked session + its active org.
func TestAuthEvents_Logout_Recorded(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "alice-org")

	resp, _ := r.driveCallback(userID)
	sid := r.sidFromResp(resp)

	if got := r.requestWithSid("POST", "/api/auth/logout", sid).StatusCode; got != http.StatusNoContent {
		t.Fatalf("logout status=%d, want 204", got)
	}

	ev := r.oneAuthEvent(domain.AuthEventLogout)
	if ev.userID != userID.String() {
		t.Errorf("logout user=%q, want %q", ev.userID, userID)
	}
	if ev.sessionID != sid {
		t.Errorf("logout session=%q, want %q", ev.sessionID, sid)
	}
	if ev.orgID != orgID.String() {
		t.Errorf("logout org=%q, want %q (the revoked session's active org)", ev.orgID, orgID)
	}
}

// Logout-all records a single logout_all carrying the revoked count + the
// caller's active org.
func TestAuthEvents_LogoutAll_Recorded(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "alice-org")

	enc := r.srv.authDeps.sessions
	// s1 (the cookie used for the request) carries a real active org so the
	// recorded event is org-scoped.
	s1, err := enc.CreateSystem(context.Background(), userID, r.signKey.mintJWT(t, validClaimsFor(userID)),
		"refresh-1", time.Now().Add(time.Hour), time.Now().Add(30*24*time.Hour), "d1", "1.1.1.1",
		uuid.NullUUID{UUID: orgID, Valid: true})
	if err != nil {
		t.Fatalf("create s1: %v", err)
	}
	if _, err := enc.CreateSystem(context.Background(), userID, r.signKey.mintJWT(t, validClaimsFor(userID)),
		"refresh-2", time.Now().Add(time.Hour), time.Now().Add(30*24*time.Hour), "d2", "2.2.2.2", uuid.NullUUID{}); err != nil {
		t.Fatalf("create s2: %v", err)
	}

	if got := r.requestWithSid("POST", "/api/auth/logout/all", s1.ID.String()).StatusCode; got != http.StatusNoContent {
		t.Fatalf("logout-all status=%d, want 204", got)
	}

	ev := r.oneAuthEvent(domain.AuthEventLogoutAll)
	if ev.userID != userID.String() {
		t.Errorf("logout_all user=%q, want %q", ev.userID, userID)
	}
	if ev.orgID != orgID.String() {
		t.Errorf("logout_all org=%q, want %q (the caller's active org)", ev.orgID, orgID)
	}
	if ev.sessionID != s1.ID.String() {
		t.Errorf("logout_all session=%q, want %q (the initiating session pinned for forensics)", ev.sessionID, s1.ID)
	}
	if ev.detail["count"] != float64(2) {
		t.Errorf("logout_all count=%v, want 2", ev.detail["count"])
	}
}

// A failed inline JWT refresh records refresh_failure (and 401s the request).
func TestAuthEvents_RefreshFailure_Recorded(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	r.seedOrg(userID, "alice-org")

	enc := r.srv.authDeps.sessions
	sess, err := enc.CreateSystem(context.Background(), userID, r.signKey.mintJWT(t, validClaimsFor(userID)),
		"refresh-orig",
		time.Now().Add(30*time.Second).UTC(), // near-expiry JWT → needsRefresh
		time.Now().Add(30*24*time.Hour).UTC(),
		"ua", "127.0.0.1", uuid.NullUUID{})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	r.srv.authDeps.gotrueRefresh = func(context.Context, string) (string, string, int64, error) {
		return "", "", 0, errSimulated
	}

	if got := r.requestWithSid("GET", "/api/me", sess.ID.String()).StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("/api/me with failed refresh = %d, want 401", got)
	}

	ev := r.oneAuthEvent(domain.AuthEventRefreshFailure)
	if ev.userID != userID.String() || ev.sessionID != sess.ID.String() {
		t.Errorf("refresh_failure user=%q session=%q, want %q / %q", ev.userID, ev.sessionID, userID, sess.ID)
	}
	if _, ok := ev.detail["reason"]; !ok {
		t.Errorf("refresh_failure detail missing reason: %v", ev.detail)
	}
}

// A session whose JWT fails verification (signed by a key absent from the JWKS)
// records jwt_verify_failure — the verify-rejection write-site only.
func TestAuthEvents_JWTVerifyFailure_Recorded(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	r.seedOrg(userID, "alice-org")

	// A JWT signed by a key the rig's JWKS never serves → verify rejects it.
	badKey := newTestKey(t)
	badJWT := badKey.mintJWT(t, validClaimsFor(userID))
	enc := r.srv.authDeps.sessions
	sess, err := enc.CreateSystem(context.Background(), userID, badJWT, "refresh-x",
		time.Now().Add(time.Hour).UTC(), // far from expiry → no refresh, verify runs
		time.Now().Add(30*24*time.Hour).UTC(),
		"ua", "127.0.0.1", uuid.NullUUID{})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	if got := r.requestWithSid("GET", "/api/me", sess.ID.String()).StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("/api/me with unverifiable JWT = %d, want 401", got)
	}

	ev := r.oneAuthEvent(domain.AuthEventJWTVerifyFailure)
	if ev.userID != userID.String() || ev.sessionID != sess.ID.String() {
		t.Errorf("jwt_verify_failure user=%q session=%q, want %q / %q", ev.userID, ev.sessionID, userID, sess.ID)
	}
}

// Headline best-effort contract: a failing AuthEventStore must NOT break the auth
// flow — login still 302s, logout still 204s — and the failure surfaces as an
// ERROR log line (the accepted "known gap" guarantee in lieu of gaplessness).
func TestAuthEvents_BestEffort_FailingStoreStillSucceeds(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	r.seedOrg(userID, "alice-org")

	r.srv.authEvents = failingAuthEventStore{}

	var logbuf bytes.Buffer
	restore := logging.SetOutput(&logbuf)
	defer restore()

	resp, _ := r.driveCallback(userID)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login status=%d, want 302 (a failing audit store must not break login)", resp.StatusCode)
	}
	sid := r.sidFromResp(resp)

	if got := r.requestWithSid("POST", "/api/auth/logout", sid).StatusCode; got != http.StatusNoContent {
		t.Fatalf("logout status=%d, want 204 (a failing audit store must not break logout)", got)
	}

	if !strings.Contains(logbuf.String(), "auth event record failed") {
		t.Errorf("expected an ERROR 'auth event record failed' log on the audit write failure; got:\n%s", logbuf.String())
	}
}

// capturingAuthEventStore records the last event handed to RecordSystem, so the
// seam's request-source enrichment can be asserted without a DB / the pg rig.
type capturingAuthEventStore struct{ last *domain.AuthEvent }

func (c *capturingAuthEventStore) RecordSystem(_ context.Context, e domain.AuthEvent) error {
	c.last = &e
	return nil
}
func (c *capturingAuthEventStore) ListByOrgSystem(context.Context, string, domain.AuthEventListOpts) ([]domain.AuthEvent, error) {
	return nil, nil
}
func (c *capturingAuthEventStore) ListByUserSystem(context.Context, string, domain.AuthEventListOpts) ([]domain.AuthEvent, error) {
	return nil, nil
}

// The RecordAuthEvent extension seam (ee/sso's only door to the audit log) fills
// the request-source fields — ip_address via the canonical clientIP parsing (port
// stripped), user_agent — so EE auth events carry them without duplicating
// clientIP. A caller-set value is preserved; a nil request is a no-op. Runs
// without the pg rig. TFAC-76.
func TestRecordAuthEventSeam_EnrichesRequestSource(t *testing.T) {
	recorder := &capturingAuthEventStore{}
	api := serverExtensionAPI{&Server{authEvents: recorder}}

	req := httptest.NewRequest("GET", "/api/auth/callback", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	req.Header.Set("User-Agent", "EE-Browser/2.0")

	api.RecordAuthEvent(req.Context(), req, domain.AuthEvent{
		EventType: domain.AuthEventBreakGlassLogin,
		UserID:    "11111111-1111-1111-1111-111111111111",
		OrgID:     "22222222-2222-2222-2222-222222222222",
	})
	if recorder.last == nil {
		t.Fatal("seam did not record an event")
	}
	if recorder.last.IPAddress != "203.0.113.9" {
		t.Errorf("ip_address=%q, want 203.0.113.9 (canonical clientIP parsing strips the port)", recorder.last.IPAddress)
	}
	if recorder.last.UserAgent != "EE-Browser/2.0" {
		t.Errorf("user_agent=%q, want EE-Browser/2.0", recorder.last.UserAgent)
	}

	// A caller-set source value is preserved (defensive — not overwritten).
	recorder.last = nil
	api.RecordAuthEvent(req.Context(), req, domain.AuthEvent{
		EventType: domain.AuthEventBreakGlassLogin, IPAddress: "198.51.100.1",
	})
	if recorder.last.IPAddress != "198.51.100.1" {
		t.Errorf("pre-set ip_address overwritten: got %q, want 198.51.100.1", recorder.last.IPAddress)
	}

	// A nil request is a no-op, not a panic (degenerate call).
	recorder.last = nil
	api.RecordAuthEvent(context.Background(), nil, domain.AuthEvent{EventType: domain.AuthEventBreakGlassLogin})
	if recorder.last == nil || recorder.last.IPAddress != "" || recorder.last.UserAgent != "" {
		t.Errorf("nil request should leave source fields empty: %+v", recorder.last)
	}
}

// fireCallbackRaw drives /api/auth/callback with a freshly-signed (valid) state
// cookie + matching csrf and the given code, returning the response. Unlike
// driveCallback it does NOT stub gotrueExchange — the caller shapes the failure
// branch under test by setting authDeps.gotrueExchange first.
func (r *authRig) fireCallbackRaw(code string) *http.Response {
	r.t.Helper()
	csrf := "csrf-" + uuid.NewString()
	stateVal := r.signStateCookie("/dashboard", csrf, "verifier")
	q := url.Values{}
	q.Set("state", csrf)
	if code != "" {
		q.Set("code", code)
	}
	req := httptest.NewRequest("GET", "/api/auth/callback?"+q.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: stateVal})
	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, req)
	return rec.Result()
}

// The remaining login_failure branches in handleOAuthCallback (beyond the
// state-mismatch case above) each emit ONE login_failure with the rejecting
// branch in detail. These are the most likely production failure paths. TFAC-76.
func TestAuthEvents_LoginFailure_Branches(t *testing.T) {
	// No code on the callback (state OK) — the mechanically-trivial branch.
	t.Run("missing_code", func(t *testing.T) {
		r := newAuthRig(t)
		if got := r.fireCallbackRaw("").StatusCode; got != http.StatusBadRequest {
			t.Fatalf("missing-code callback status=%d, want 400", got)
		}
		ev := r.oneAuthEvent(domain.AuthEventLoginFailure)
		if ev.detail["reason"] != "missing_code" || ev.detail["method"] != "github" {
			t.Errorf("detail=%v, want reason=missing_code method=github", ev.detail)
		}
	})

	// A failed token exchange.
	t.Run("exchange_failed", func(t *testing.T) {
		r := newAuthRig(t)
		r.srv.authDeps.gotrueExchange = func(context.Context, string, string) (string, string, int64, error) {
			return "", "", 0, errSimulated
		}
		if got := r.fireCallbackRaw("some-code").StatusCode; got != http.StatusBadRequest {
			t.Fatalf("exchange-fail callback status=%d, want 400", got)
		}
		ev := r.oneAuthEvent(domain.AuthEventLoginFailure)
		if ev.detail["reason"] != "exchange_failed" || ev.detail["method"] != "github" {
			t.Errorf("detail=%v, want reason=exchange_failed method=github", ev.detail)
		}
	})

	// A token whose signing key is absent from the JWKS → verify rejects it.
	t.Run("verify_failed", func(t *testing.T) {
		r := newAuthRig(t)
		badKey := newTestKey(t)
		badTok := badKey.mintJWT(t, validClaimsFor(uuid.New()))
		r.srv.authDeps.gotrueExchange = func(context.Context, string, string) (string, string, int64, error) {
			return badTok, "refresh", time.Now().Add(time.Hour).Unix(), nil
		}
		if got := r.fireCallbackRaw("code").StatusCode; got != http.StatusUnauthorized {
			t.Fatalf("verify-fail callback status=%d, want 401", got)
		}
		ev := r.oneAuthEvent(domain.AuthEventLoginFailure)
		if ev.detail["reason"] != "verify_failed" {
			t.Errorf("detail=%v, want reason=verify_failed", ev.detail)
		}
	})

	// A valid token (signature/iss/aud/exp) whose sub is not a UUID → parse fails
	// after verify, so the email is known and lands in detail.
	t.Run("bad_sub", func(t *testing.T) {
		r := newAuthRig(t)
		claims := jwt.MapClaims{
			"sub": "not-a-uuid", "email": "bad@corp.example", "email_verified": true,
			"iss": testIssuer, "aud": testAudience,
			"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		}
		tok := r.signKey.mintJWT(t, claims)
		r.srv.authDeps.gotrueExchange = func(context.Context, string, string) (string, string, int64, error) {
			return tok, "refresh", time.Now().Add(time.Hour).Unix(), nil
		}
		if got := r.fireCallbackRaw("code").StatusCode; got != http.StatusBadRequest {
			t.Fatalf("bad-sub callback status=%d, want 400", got)
		}
		ev := r.oneAuthEvent(domain.AuthEventLoginFailure)
		if ev.detail["reason"] != "bad_sub" || ev.detail["email"] != "bad@corp.example" {
			t.Errorf("detail=%v, want reason=bad_sub email=bad@corp.example", ev.detail)
		}
	})

	// The highest-value error path: principal resolution fails at the DB. A sub
	// that's a valid UUID but absent from auth.users makes the user_identities FK
	// insert inside resolveOrCreatePrincipal fail → principal_resolve_failed (and a
	// 500). The email is known from the verified claim.
	t.Run("principal_resolve_failed", func(t *testing.T) {
		r := newAuthRig(t)
		orphan := uuid.New() // never seeded into auth.users → FK insert fails
		tok := r.signKey.mintJWT(t, validClaimsFor(orphan))
		r.srv.authDeps.gotrueExchange = func(context.Context, string, string) (string, string, int64, error) {
			return tok, "refresh", time.Now().Add(time.Hour).Unix(), nil
		}
		if got := r.fireCallbackRaw("code").StatusCode; got != http.StatusInternalServerError {
			t.Fatalf("principal-resolve-fail callback status=%d, want 500", got)
		}
		ev := r.oneAuthEvent(domain.AuthEventLoginFailure)
		if ev.detail["reason"] != "principal_resolve_failed" {
			t.Errorf("detail=%v, want reason=principal_resolve_failed", ev.detail)
		}
		if ev.detail["email"] != orphan.String()+"@test" {
			t.Errorf("detail email=%v, want %q (known from the verified claim)", ev.detail["email"], orphan.String()+"@test")
		}
	})
}
