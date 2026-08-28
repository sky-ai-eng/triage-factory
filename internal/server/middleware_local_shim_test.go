package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
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

// TestHandleMe_LocalMode_UnprovisionedInstall_AnswersEmptyTeams pins the
// fresh-install shape. Nothing creates tenant rows at boot or in a migration —
// provisioning is the explicit "Start your factory" action — so a local DB has
// no org and no team until the user takes it, and /api/me is answerable that
// whole time. The teams field is [] there, not an error: an unprovisioned
// install is a state, not a fault, and 500ing the identity read would break
// first run. (The teams store's own doc calls a TEAMLESS ORG a bootstrap bug,
// which this is not — there is no org here either.)
//
// Migrates directly rather than going through newTestServer, whose fixture
// seeds the synthetic local tenant.
func TestHandleMe_LocalMode_UnprovisionedInstall_AnswersEmptyTeams(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)

	database, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = database.Close() })
	if err := db.Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// The fixture is only honest if it really is tenantless.
	var teams int
	if err := database.QueryRow(`SELECT count(*) FROM teams`).Scan(&teams); err != nil {
		t.Fatalf("count teams: %v", err)
	}
	if teams != 0 {
		t.Fatalf("fixture seeded %d teams — this must be the tenantless shape", teams)
	}

	s := New(database, sqlitestore.New(database))

	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/me", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — an unprovisioned local install must still get its identity", rec.Code)
	}
	var body struct {
		Teams []struct {
			ID string `json:"id"`
		} `json:"teams"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Teams) != 0 {
		t.Errorf("teams = %+v, want [] before provisioning", body.Teams)
	}
}

// teamGetMiss is the real teams store with Get forced to answer "no row". It
// stages the one state the local arm treats as corruption: the default-team
// lookup names an id, and reading that id back finds nothing.
type teamGetMiss struct{ db.TeamsStore }

func (teamGetMiss) Get(context.Context, string, string) (*domain.Team, error) { return nil, nil }

// TestHandleMe_LocalMode_VanishedDefaultTeam_Errors is the other side of the
// test above. An empty default team is a state; a default team that names a row
// which is not there is not — the id came from the teams table one statement
// earlier, and this read is the same table by (id, org_id) with no further
// filter. Answering [] would withdraw the viewer's team grants and report
// success for a read that found nothing, so it fails instead.
func TestHandleMe_LocalMode_VanishedDefaultTeam_Errors(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)

	s := newTestServer(t)
	s.teams = teamGetMiss{s.teams}

	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/me", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 — a default team resolving to no row is corruption, not an empty list (body=%s)",
			rec.Code, rec.Body.String())
	}
}
