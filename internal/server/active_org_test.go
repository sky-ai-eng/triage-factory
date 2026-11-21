package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// TestActiveOrg_OAuthCallback_DefaultsToEarliestMembership: the session row the
// OAuth callback mints points at the user's earliest org membership.
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

// TestActiveOrg_OAuthCallback_NoMembershipsStoresNull: a user with no org
// membership gets a session with active_org_id NULL, and withSession then
// carries their claims with no org rather than substituting one.
func TestActiveOrg_OAuthCallback_NoMembershipsStoresNull(t *testing.T) {
	r := newAuthRig(t)
	// Signup provisions nothing, so a fresh user with no seedOrg has zero
	// memberships.
	userID := r.seedUser()

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

	var sawOrgID, sawClaims string
	r.srv.mux.Handle("GET /api/test/null-orgid-probe",
		r.srv.withSession(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			sawOrgID = OrgIDFrom(req.Context())
			if c := ClaimsFrom(req.Context()); c != nil {
				sawClaims = c.Subject
			}
			w.WriteHeader(http.StatusOK)
		})))
	if got := r.requestWithSid("GET", "/api/test/null-orgid-probe", sid); got.StatusCode != http.StatusOK {
		t.Fatalf("probe status=%d", got.StatusCode)
	}
	if sawOrgID != "" {
		t.Errorf("OrgIDFrom = %q, want empty (NULL active_org_id)", sawOrgID)
	}
	if sawClaims != userID.String() {
		t.Errorf("ClaimsFrom().Subject = %q, want %q (claims still populated)", sawClaims, userID)
	}
}

// patchMe sends body as PATCH /api/me with the given sid and a same-origin
// Origin header, so the CSRF guard passes.
func (r *authRig) patchMe(t *testing.T, sid string, body any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest("PATCH", "/api/me", bytes.NewReader(raw))
	if sid != "" {
		req.AddCookie(&http.Cookie{Name: r.srv.sidCookieName(), Value: sid})
	}
	req.Header.Set("Origin", r.srv.deployCfg.publicURL)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, req)
	return rec.Result()
}

// TestActiveOrg_Update_SwitchToMemberOrg_Succeeds: a user in two orgs switches
// from the default to the other, and withSession surfaces it on the next request.
func TestActiveOrg_Update_SwitchToMemberOrg_Succeeds(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgA, _ := r.seedOrg(userID, "active-a")
	time.Sleep(50 * time.Millisecond)
	orgB, _ := r.seedOrg(userID, "active-b")
	_ = orgA

	resp, _ := r.driveCallback(userID)
	sid := r.sidFromResp(resp)

	// Switch to orgB. The answer is the me resource with the org just set.
	got := r.patchMe(t, sid, map[string]string{"active_org_id": orgB.String()})
	if got.StatusCode != http.StatusOK {
		t.Fatalf("PATCH /api/me status=%d, want 200", got.StatusCode)
	}
	var me struct {
		ActiveOrgID string `json:"active_org_id"`
	}
	if err := json.NewDecoder(got.Body).Decode(&me); err != nil {
		t.Fatalf("decode PATCH /api/me: %v", err)
	}
	if me.ActiveOrgID != orgB.String() {
		t.Errorf("PATCH /api/me active_org_id = %q, want %q", me.ActiveOrgID, orgB)
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

// TestActiveOrg_Update_NonMemberOrg_Returns404: switching to an org the caller
// is not a member of is a 404, not a 403, so the org's existence is not leaked.
func TestActiveOrg_Update_NonMemberOrg_Returns404(t *testing.T) {
	r := newAuthRig(t)
	userA := r.seedUser()
	r.seedOrg(userA, "alice-org")
	userB := r.seedUser()
	orgB, _ := r.seedOrg(userB, "bob-org")

	resp, _ := r.driveCallback(userA)
	sid := r.sidFromResp(resp)

	got := r.patchMe(t, sid, map[string]string{"active_org_id": orgB.String()})
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

// TestActiveOrg_Update_BadBodies_Return400: a non-UUID active_org_id is a 400,
// distinct from the 404 for a well-formed id the caller is not a member of; so
// is a body that names no field, and a null, which asks to clear the session's
// org.
func TestActiveOrg_Update_BadBodies_Return400(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	r.seedOrg(userID, "alice-org")

	resp, _ := r.driveCallback(userID)
	sid := r.sidFromResp(resp)

	for _, tc := range []struct {
		what   string
		body   map[string]any
		reason string
	}{
		{"malformed", map[string]any{"active_org_id": "not-a-uuid"}, httpx.ReasonInvalidField},
		{"absent", map[string]any{}, httpx.ReasonMissingField},
		{"null", map[string]any{"active_org_id": nil}, httpx.ReasonInvalidField},
	} {
		t.Run(tc.what, func(t *testing.T) {
			got := r.patchMe(t, sid, tc.body)
			if got.StatusCode != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400", got.StatusCode)
			}
			var env struct {
				Errors []httpx.ErrorItem `json:"errors"`
			}
			if err := json.NewDecoder(got.Body).Decode(&env); err != nil || len(env.Errors) == 0 {
				t.Fatalf("decode errors: %v", err)
			}
			if e := env.Errors[0]; e.Reason != tc.reason || e.Field != "active_org_id" {
				t.Errorf("error = %+v, want %s on active_org_id", e, tc.reason)
			}
		})
	}
}

// TestActiveOrg_Update_NoSession_Returns401: a request without a sid cookie is
// refused by withSession before the handler runs.
func TestActiveOrg_Update_NoSession_Returns401(t *testing.T) {
	r := newAuthRig(t)
	got := r.patchMe(t, "", map[string]string{"active_org_id": uuid.New().String()})
	if got.StatusCode != http.StatusUnauthorized {
		t.Errorf("no sid status=%d, want 401", got.StatusCode)
	}
}

// TestActiveOrg_Update_SentinelClaim_Returns401: the local sentinel claim is
// refused, since switching orgs needs a real user with real memberships.
func TestActiveOrg_Update_SentinelClaim_Returns401(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "alice-org")

	// Call the handler directly with the sentinel claim, bypassing withSession.
	body := bytes.NewBufferString(`{"active_org_id":"` + orgID.String() + `"}`)
	req := httptest.NewRequest("PATCH", "/api/me", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", r.srv.deployCfg.publicURL)
	ctx := httpx.WithClaims(req.Context(), &verify.Claims{Subject: runmode.LocalDefaultUserID})
	rec := httptest.NewRecorder()
	r.srv.handleMeUpdate(rec, req.WithContext(ctx))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("sentinel claim status=%d, want 401", rec.Code)
	}
}

// TestActiveOrg_Update_CrossOriginRejected pins the CSRF guard: a cross-origin
// PATCH carrying a valid sid cookie is refused with 403.
func TestActiveOrg_Update_CrossOriginRejected(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "alice-org")

	resp, _ := r.driveCallback(userID)
	sid := r.sidFromResp(resp)

	body := bytes.NewBufferString(`{"active_org_id":"` + orgID.String() + `"}`)
	req := httptest.NewRequest("PATCH", "/api/me", body)
	req.AddCookie(&http.Cookie{Name: r.srv.sidCookieName(), Value: sid})
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin status=%d, want 403", rec.Code)
	}
}
