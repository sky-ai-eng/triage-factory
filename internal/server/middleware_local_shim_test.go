package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestWithSession_LocalShim_InjectsSentinels pins the "handlers read
// identity uniformly via ClaimsFrom/OrgIDFrom in both modes" contract.
// In local mode (TF_MODE=local, authDeps nil) the wrapper must inject
// a synthetic Claims with Subject = LocalDefaultUserID and ctxKeyOrgID
// = LocalDefaultOrgID before delegating. A regression that drops the
// injection would put every handler back into "branch on mode" land —
// every per-handler sweep PR depends on this.
func TestWithSession_LocalShim_InjectsSentinels(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)

	s := &Server{} // authDeps deliberately nil — local-mode boot

	var gotSubject, gotOrgID string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c := ClaimsFrom(r.Context()); c != nil {
			gotSubject = c.Subject
		}
		gotOrgID = OrgIDFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	s.withSession(inner).ServeHTTP(rec, httptest.NewRequest("GET", "/api/anything", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (local-mode shim should pass through)", rec.Code)
	}
	if gotSubject != runmode.LocalDefaultUserID {
		t.Errorf("ClaimsFrom().Subject = %q, want %q", gotSubject, runmode.LocalDefaultUserID)
	}
	if gotOrgID != runmode.LocalDefaultOrgID {
		t.Errorf("OrgIDFrom() = %q, want %q", gotOrgID, runmode.LocalDefaultOrgID)
	}
}

// TestHandleMe_LocalMode_SynthesizesSentinelResponse pins the
// local-equals-multi-at-N=1 contract: the withSession shim injects a
// synthetic claim with Subject = LocalDefaultUserID, and handleMe
// detects that and returns a synthesized response built from sentinel
// constants instead of hitting Postgres-only queries (public.users,
// tf.current_user_id()) that would 500 against local SQLite. The FE
// gets one signed-in shape across both modes.
func TestHandleMe_LocalMode_SynthesizesSentinelResponse(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)

	s := &Server{} // authDeps nil → shim injects sentinel claims

	rec := httptest.NewRecorder()
	s.withSession(http.HandlerFunc(s.handleMe)).ServeHTTP(rec, httptest.NewRequest("GET", "/api/me", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (local-mode /api/me must synthesize a signed-in response)", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body struct {
		ID          string `json:"id"`
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
		ActiveOrgID string `json:"active_org_id"`
		Orgs        []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Role string `json:"role"`
		} `json:"orgs"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.ID != runmode.LocalDefaultUserID {
		t.Errorf("id = %q, want %q", body.ID, runmode.LocalDefaultUserID)
	}
	if body.Email != "" {
		t.Errorf("email = %q, want empty (local mode has no email)", body.Email)
	}
	if body.DisplayName != "Local" {
		t.Errorf("display_name = %q, want %q", body.DisplayName, "Local")
	}
	if body.ActiveOrgID != runmode.LocalDefaultOrgID {
		t.Errorf("active_org_id = %q, want %q", body.ActiveOrgID, runmode.LocalDefaultOrgID)
	}
	if len(body.Orgs) != 1 {
		t.Fatalf("orgs len = %d, want 1", len(body.Orgs))
	}
	if body.Orgs[0].ID != runmode.LocalDefaultOrgID {
		t.Errorf("orgs[0].id = %q, want %q", body.Orgs[0].ID, runmode.LocalDefaultOrgID)
	}
	if body.Orgs[0].Name != "Local" {
		t.Errorf("orgs[0].name = %q, want %q", body.Orgs[0].Name, "Local")
	}
	if body.Orgs[0].Role != "owner" {
		t.Errorf("orgs[0].role = %q, want owner", body.Orgs[0].Role)
	}
}

// TestWithSession_MultiMode_NilAuthDeps_PassesThroughWithoutClaims
// pins the boot-race safety. SetAuthDeps lands after routes() in
// multi mode — a request that races in during that window must NOT
// receive the local-mode sentinel (that would let an unauthenticated
// caller masquerade as the synthetic local user once authDeps lands
// for a different identity model). The correct posture is the prior
// pass-through: handlers see nil claims and write 401 themselves.
func TestWithSession_MultiMode_NilAuthDeps_PassesThroughWithoutClaims(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)

	s := &Server{}

	var sawClaims, sawOrgID bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawClaims = ClaimsFrom(r.Context()) != nil
		sawOrgID = OrgIDFrom(r.Context()) != ""
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	s.withSession(inner).ServeHTTP(rec, httptest.NewRequest("GET", "/api/anything", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (multi-mode pre-deps boot race should pass through)", rec.Code)
	}
	if sawClaims {
		t.Error("ClaimsFrom() returned non-nil in multi mode with nil authDeps; sentinel must NOT bleed across modes")
	}
	if sawOrgID {
		t.Error("OrgIDFrom() returned non-empty in multi mode with nil authDeps; sentinel must NOT bleed across modes")
	}
}

// TestHandleMe_LocalMode_CarriesSoleTeamAsAdmin pins the local half of the
// teams field. Local mode is N=1 — one org, one team, and the single user owns
// it — so the synthesized response reports that team at role admin, which is
// the same answer the teams list gives the per-team gates. Runs against the
// real SQLite stores rather than the bare rig above, because the row it reports
// comes from the teams store rather than a sentinel constant.
func TestHandleMe_LocalMode_CarriesSoleTeamAsAdmin(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)

	s := newTestServer(t)

	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/me", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Teams []struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			OrgID string `json:"org_id"`
			Role  string `json:"role"`
		} `json:"teams"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Teams) != 1 {
		t.Fatalf("teams = %+v, want the sole local team", body.Teams)
	}
	got := body.Teams[0]
	if got.ID != runmode.LocalDefaultTeamID {
		t.Errorf("teams[0].id = %q, want %q", got.ID, runmode.LocalDefaultTeamID)
	}
	if got.OrgID != runmode.LocalDefaultOrgID {
		t.Errorf("teams[0].org_id = %q, want %q", got.OrgID, runmode.LocalDefaultOrgID)
	}
	if got.Role != "admin" {
		t.Errorf("teams[0].role = %q, want admin (local's sole user owns its sole team)", got.Role)
	}
	if got.Name == "" {
		t.Error("teams[0].name is empty — the rail renders it")
	}
}
