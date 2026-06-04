package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestActiveOrg_OAuthCallback_DefaultsToEarliestMembership exercises
// the SKY-313 OAuth-callback path: after a successful PKCE handshake,
// the new session row's active_org_id is populated from the user's
// earliest org membership. The middleware then surfaces it as
// ctxKeyOrgID on the next request.
func TestActiveOrg_OAuthCallback_DefaultsToEarliestMembership(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()

	// Two memberships, deliberately created in order so we can pin
	// "earliest by created_at" semantics. The DB uses now() so we add
	// a small sleep between to keep the ORDER BY deterministic.
	orgA, _ := r.seedOrg(userID, "earlier-org")
	time.Sleep(50 * time.Millisecond)
	orgB, _ := r.seedOrg(userID, "later-org")
	_ = orgB

	resp, _ := r.driveCallback(userID)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status=%d, want 302", resp.StatusCode)
	}
	sid := r.sidFromResp(resp)

	// Inspect the session row directly: active_org_id should be orgA.
	sidUUID := uuid.MustParse(sid)
	var activeOrg uuid.NullUUID
	if err := r.h.AdminDB.QueryRow(
		`SELECT active_org_id FROM public.sessions WHERE id = $1`, sidUUID,
	).Scan(&activeOrg); err != nil {
		t.Fatalf("select active_org_id: %v", err)
	}
	if !activeOrg.Valid {
		t.Fatal("active_org_id NULL after callback with memberships")
	}
	if activeOrg.UUID != orgA {
		t.Errorf("active_org_id = %s, want %s (earliest membership)", activeOrg.UUID, orgA)
	}
}

// TestActiveOrg_OAuthCallback_NoMembershipsStoresNull covers the
// first-time-login-pre-invite case: user has no org_memberships rows
// at callback time, so the session is created with active_org_id =
// NULL. Handlers that require an org will 409 until the user is added
// to one.
func TestActiveOrg_OAuthCallback_NoMembershipsStoresNull(t *testing.T) {
	r := newAuthRig(t)
	// Signup provisions nothing, so a fresh user with no seedOrg always
	// lands with zero memberships — no join-policy pinning needed.
	userID := r.seedUser()
	// Deliberately no seedOrg.

	resp, _ := r.driveCallback(userID)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status=%d, want 302", resp.StatusCode)
	}
	sid := r.sidFromResp(resp)

	sidUUID := uuid.MustParse(sid)
	var activeOrg uuid.NullUUID
	if err := r.h.AdminDB.QueryRow(
		`SELECT active_org_id FROM public.sessions WHERE id = $1`, sidUUID,
	).Scan(&activeOrg); err != nil {
		t.Fatalf("select active_org_id: %v", err)
	}
	if activeOrg.Valid {
		t.Errorf("active_org_id = %s, want NULL (no memberships)", activeOrg.UUID)
	}
}

// TestActiveOrg_Middleware_PopulatesOrgIDFromSession is the
// integration check for withSession's multi-mode population: after a
// successful auth flow, OrgIDFrom(ctx) returns the session's active
// org. Without this the D9-core handler sweep is dead-on-arrival in
// multi mode — every handler would see an empty orgID.
func TestActiveOrg_Middleware_PopulatesOrgIDFromSession(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "ctx-org")

	resp, _ := r.driveCallback(userID)
	sid := r.sidFromResp(resp)

	// Mount a probe handler that returns whatever OrgIDFrom sees.
	r.srv.mux.Handle("GET /api/test/orgid-probe",
		r.srv.withSession(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			_, _ = w.Write([]byte(OrgIDFrom(req.Context())))
		})))

	got := r.requestWithSid("GET", "/api/test/orgid-probe", sid)
	if got.StatusCode != http.StatusOK {
		t.Fatalf("probe status=%d", got.StatusCode)
	}
	body := make([]byte, 64)
	n, _ := got.Body.Read(body)
	if string(body[:n]) != orgID.String() {
		t.Errorf("OrgIDFrom = %q, want %q", string(body[:n]), orgID)
	}
}

// TestActiveOrg_Middleware_NullActiveOrgLeavesCtxKeyEmpty pins the
// other half of the multi-mode population: when the session row has
// active_org_id NULL (user joined no org yet), withSession leaves
// ctxKeyOrgID unset and OrgIDFrom returns "". Handlers that require
// an org are expected to 409; that policy is a D9-core concern.
func TestActiveOrg_Middleware_NullActiveOrgLeavesCtxKeyEmpty(t *testing.T) {
	r := newAuthRig(t)
	// Signup provisions nothing, so a fresh user with no seedOrg lands
	// with active_org_id NULL — the zero-membership scenario this exercises.
	userID := r.seedUser()
	// No seedOrg — session will land with active_org_id NULL.

	resp, _ := r.driveCallback(userID)
	sid := r.sidFromResp(resp)

	var sawOrgID, sawClaims string
	r.srv.mux.Handle("GET /api/test/null-orgid-probe",
		r.srv.withSession(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			sawOrgID = OrgIDFrom(req.Context())
			if c := ClaimsFrom(req.Context()); c != nil {
				sawClaims = c.Subject
			}
			w.WriteHeader(http.StatusOK)
		})))

	got := r.requestWithSid("GET", "/api/test/null-orgid-probe", sid)
	if got.StatusCode != http.StatusOK {
		t.Fatalf("probe status=%d", got.StatusCode)
	}
	if sawOrgID != "" {
		t.Errorf("OrgIDFrom = %q, want empty (NULL active_org_id)", sawOrgID)
	}
	if sawClaims != userID.String() {
		t.Errorf("ClaimsFrom().Subject = %q, want %q (claims still populated)", sawClaims, userID)
	}
}

// TestActiveOrg_LocalShim_StillSetsSentinel pins that the local-mode
// shim's hardcoded LocalDefaultOrgID is untouched by the new
// active-org plumbing. The shim doesn't read sessions; it just sets
// the sentinel directly.
func TestActiveOrg_LocalShim_StillSetsSentinel(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := &Server{} // authDeps nil → shim path

	var gotOrgID string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOrgID = OrgIDFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	s.withSession(inner).ServeHTTP(rec, httptest.NewRequest("GET", "/api/anything", nil))

	if gotOrgID != runmode.LocalDefaultOrgID {
		t.Errorf("local-mode OrgIDFrom = %q, want %q (shim must hardcode sentinel regardless of session state)",
			gotOrgID, runmode.LocalDefaultOrgID)
	}
}

// postActiveOrg helper hits POST /api/me/active-org with the given sid
// and org_id body. Sets the same-origin Origin header so the CSRF
// guard passes.
func (r *authRig) postActiveOrg(t *testing.T, sid string, body any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest("POST", "/api/me/active-org", bytes.NewReader(raw))
	if sid != "" {
		req.AddCookie(&http.Cookie{Name: r.srv.sidCookieName(), Value: sid})
	}
	req.Header.Set("Origin", r.srv.deployCfg.publicURL)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, req)
	return rec.Result()
}

// TestActiveOrg_Update_SwitchToMemberOrg_Succeeds pins the happy path:
// user belongs to two orgs, defaults to orgA, switches to orgB, the
// next request's middleware surfaces orgB.
func TestActiveOrg_Update_SwitchToMemberOrg_Succeeds(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgA, _ := r.seedOrg(userID, "active-a")
	time.Sleep(50 * time.Millisecond)
	orgB, _ := r.seedOrg(userID, "active-b")
	_ = orgA

	resp, _ := r.driveCallback(userID)
	sid := r.sidFromResp(resp)

	// Switch to orgB.
	got := r.postActiveOrg(t, sid, map[string]string{"org_id": orgB.String()})
	if got.StatusCode != http.StatusOK {
		t.Fatalf("active-org status=%d, want 200", got.StatusCode)
	}

	// Next request surfaces orgB via OrgIDFrom.
	r.srv.mux.Handle("GET /api/test/switch-probe",
		r.srv.withSession(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			_, _ = w.Write([]byte(OrgIDFrom(req.Context())))
		})))

	probe := r.requestWithSid("GET", "/api/test/switch-probe", sid)
	body := make([]byte, 64)
	n, _ := probe.Body.Read(body)
	if string(body[:n]) != orgB.String() {
		t.Errorf("post-switch OrgIDFrom = %q, want %q", string(body[:n]), orgB)
	}
}

// TestActiveOrg_Update_NonMemberOrg_Returns404 pins the cross-tenant
// defense: switching to an org the caller is NOT a member of must
// 404, not 403 — same posture as withOrg. Don't leak existence.
func TestActiveOrg_Update_NonMemberOrg_Returns404(t *testing.T) {
	r := newAuthRig(t)
	userA := r.seedUser()
	r.seedOrg(userA, "alice-org")
	userB := r.seedUser()
	orgB, _ := r.seedOrg(userB, "bob-org")

	resp, _ := r.driveCallback(userA)
	sid := r.sidFromResp(resp)

	got := r.postActiveOrg(t, sid, map[string]string{"org_id": orgB.String()})
	if got.StatusCode != http.StatusNotFound {
		t.Errorf("non-member switch status=%d, want 404 (don't leak existence)", got.StatusCode)
	}

	// Active org on the session was NOT swapped.
	sidUUID := uuid.MustParse(sid)
	var active uuid.NullUUID
	if err := r.h.AdminDB.QueryRow(
		`SELECT active_org_id FROM public.sessions WHERE id = $1`, sidUUID,
	).Scan(&active); err != nil {
		t.Fatalf("post-attempt select: %v", err)
	}
	if !active.Valid || active.UUID == orgB {
		t.Errorf("session active_org_id mutated by failed switch: %+v", active)
	}
}

// TestActiveOrg_Update_MalformedOrgID_Returns400 covers the obvious
// validation: a non-UUID body is a 400, distinct from the 404 the
// "valid UUID but not a member" path returns.
func TestActiveOrg_Update_MalformedOrgID_Returns400(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	r.seedOrg(userID, "alice-org")

	resp, _ := r.driveCallback(userID)
	sid := r.sidFromResp(resp)

	got := r.postActiveOrg(t, sid, map[string]string{"org_id": "not-a-uuid"})
	if got.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed org_id status=%d, want 400", got.StatusCode)
	}
}

// TestActiveOrg_Update_NoSession_Returns401 covers the unauthenticated
// case: a request without a sid cookie hits withSession's 401 before
// the active-org handler runs.
func TestActiveOrg_Update_NoSession_Returns401(t *testing.T) {
	r := newAuthRig(t)
	got := r.postActiveOrg(t, "", map[string]string{"org_id": uuid.New().String()})
	if got.StatusCode != http.StatusUnauthorized {
		t.Errorf("no sid status=%d, want 401", got.StatusCode)
	}
}

// TestActiveOrg_Update_SentinelClaim_Returns401 mirrors handleMe's
// gate: a sentinel-claim caller (local-mode shim) has nothing to do
// here and is bounced. The active-org primitive only makes sense
// against a real GoTrue user with real memberships.
func TestActiveOrg_Update_SentinelClaim_Returns401(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "alice-org")

	// Force-inject the sentinel claim into the request bypassing
	// withSession's real auth. Easiest way: invoke handleActiveOrgUpdate
	// directly with a synthetic context.
	body := bytes.NewBufferString(`{"org_id":"` + orgID.String() + `"}`)
	req := httptest.NewRequest("POST", "/api/me/active-org", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", r.srv.deployCfg.publicURL)
	ctx := context.WithValue(req.Context(), ctxKeyClaims, &verify.Claims{Subject: runmode.LocalDefaultUserID})
	rec := httptest.NewRecorder()
	r.srv.handleActiveOrgUpdate(rec, req.WithContext(ctx))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("sentinel claim status=%d, want 401", rec.Code)
	}
}

// TestActiveOrg_Update_CrossOriginRejected pins the CSRF guard: a
// forged cross-origin POST (with sid cookie) is blocked by
// withCSRFOriginCheck regardless of membership. The mutating endpoint
// must not be drivable from an attacker-controlled origin.
func TestActiveOrg_Update_CrossOriginRejected(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "alice-org")

	resp, _ := r.driveCallback(userID)
	sid := r.sidFromResp(resp)

	body := bytes.NewBufferString(`{"org_id":"` + orgID.String() + `"}`)
	req := httptest.NewRequest("POST", "/api/me/active-org", body)
	req.AddCookie(&http.Cookie{Name: r.srv.sidCookieName(), Value: sid})
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin status=%d, want 403", rec.Code)
	}
}

// TestActiveOrg_Update_RevokedSession_Returns401 covers the race where
// a session was revoked between withSession's lookup and the UPDATE.
// In practice withSession's lookup would already have 401'd, but the
// store API returns sql.ErrNoRows; the handler must surface that as
// 401 rather than 500.
//
// This case is exercised by calling the store method directly with a
// revoked sid, since plumbing a real race through middleware would be
// flaky.
func TestActiveOrg_Update_RevokedSession_StoreReturnsNoRows(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "alice-org")

	resp, _ := r.driveCallback(userID)
	sid := r.sidFromResp(resp)
	sidUUID := uuid.MustParse(sid)

	// Revoke the session.
	if err := r.srv.authDeps.sessions.RevokeSystem(context.Background(), sidUUID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	err := r.srv.authDeps.sessions.UpdateActiveOrgSystem(context.Background(), sidUUID, orgID)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("UpdateActiveOrgSystem on revoked: %v, want sql.ErrNoRows", err)
	}
}
