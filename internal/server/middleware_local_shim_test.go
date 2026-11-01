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

// TestHandleMe_LocalMode_SynthesizesSentinelResponse: in local mode withSession
// injects the sentinel claims and /api/me answers a synthesized signed-in shape
// built from the sentinel constants, without the Postgres-only queries.
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
		OrgCreationEnabled bool `json:"org_creation_enabled"`
		CanCreateOrg       bool `json:"can_create_org"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !body.OrgCreationEnabled || !body.CanCreateOrg {
		t.Errorf("org_creation_enabled = %v, can_create_org = %v; want both true in local mode",
			body.OrgCreationEnabled, body.CanCreateOrg)
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

// TestWithSession_MultiMode_NilAuthDeps_PassesThroughWithoutClaims: a multi-mode
// request arriving before SetAuthDeps gets no sentinel claims or org, so the
// handler answers 401 itself rather than serving the synthetic local user.
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

// TestHandleMe_LocalMode_CarriesSoleTeamAsAdmin: local /api/me reports the one
// local team at role admin, read from the teams store, which is the answer the
// per-team gates get.
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

// TestHandleMe_LocalMode_UnprovisionedInstall_AnswersEmptyTeams: a local install
// nobody has provisioned has no org or team, and /api/me still answers 200 with
// teams []. It migrates directly because newTestServer seeds the local tenant.
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

// TestHandleMe_LocalMode_VanishedDefaultTeam_Errors: a default team id that reads
// back as no row is corruption, so /api/me answers 500 rather than [], which
// would withdraw the viewer's team grants.
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
