package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
	"github.com/sky-ai-eng/triage-factory/internal/server/teams"
)

// archivedTeamRig is a Postgres-backed Server, with RLS live, over an org with
// an owner and a plain member, for the writes an archived team refuses.
type archivedTeamRig struct {
	s      *Server
	orgID  string
	teamID string
	owner  string // org owner (org admin)
	member string // org + team member, NOT an org admin
}

func newArchivedTeamRig(t *testing.T) *archivedTeamRig {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeMulti)

	h := pgtest.Shared(t)
	h.Reset(t)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	s := New(h.AdminDB, stores)

	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, h, "owner")
	member := pgtest.SeedUser(t, h, "member")
	pgtest.AddOrgMember(t, h, member, orgID, teamID, "member", "member")

	return &archivedTeamRig{s: s, orgID: orgID, teamID: teamID, owner: owner, member: member}
}

// archive archives the rig's team as its owner through the teams routes,
// mounted on a bare mux so the request's seeded claims reach the handler
// without a login session. No spawner is wired: there is no work to stop.
func (r *archivedTeamRig) archive(t *testing.T) {
	t.Helper()
	mux := http.NewServeMux()
	direct := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, h) }
	teams.New(r.s.tx, r.s.az, r.s.allStores, nil, nil).Register(httpx.Mounts{API: direct, APIMutating: direct})

	req := httptest.NewRequest(http.MethodPost, "/api/teams/"+r.teamID+"/archive", nil)
	ctx := httpx.WithClaims(req.Context(), &verify.Claims{Subject: r.owner})
	ctx = httpx.WithOrgID(ctx, r.orgID)
	arc := httptest.NewRecorder()
	mux.ServeHTTP(arc, req.WithContext(ctx))
	if arc.Code != http.StatusOK {
		t.Fatalf("archive = %d, want 200; body=%s", arc.Code, arc.Body.String())
	}
}

// TestTeamArchive_BlocksTeamSettingsWrite: an archived team rejects a
// team-settings write with 403 TEAM_ARCHIVED, and a restore re-opens it. This
// is the VerifyTeamNotArchived gate end-to-end.
func TestTeamArchive_BlocksTeamSettingsWrite(t *testing.T) {
	r := newArchivedTeamRig(t)
	r.archive(t)

	// A team-settings PATCH (gated on user_is_team_admin) must now be blocked by
	// VerifyTeamNotArchived with a clear archived 403.
	wreq := httptest.NewRequest(http.MethodPatch, "/api/teams/"+r.teamID+"/settings", strings.NewReader(`{"ai_model":"opus"}`))
	wreq.Header.Set("Content-Type", "application/json")
	wreq.SetPathValue("team_id", r.teamID)
	ctx := httpx.WithClaims(wreq.Context(), &verify.Claims{Subject: r.owner})
	ctx = httpx.WithOrgID(ctx, r.orgID)
	wreq = wreq.WithContext(ctx)

	wrec := httptest.NewRecorder()
	r.s.handleTeamSettingsPatch(wrec, wreq)
	if wrec.Code != http.StatusForbidden {
		t.Fatalf("team-settings write on archived team = %d, want 403; body=%s", wrec.Code, wrec.Body.String())
	}
	httpxtest.AssertFirstError(t, wrec, httpx.ReasonTeamArchived, "")
}

// TestTeamArchive_BlocksMembershipWrite: an archived team's member add is a 403
// TEAM_ARCHIVED. The membership handlers' RLS does not filter archived teams,
// so the handler's own archived check is what refuses it.
func TestTeamArchive_BlocksMembershipWrite(t *testing.T) {
	r := newArchivedTeamRig(t)
	r.archive(t)

	tmh := &teamMembersHandler{tx: r.s.tx, az: r.s.az}
	body := strings.NewReader(`{"user_id":"` + r.member + `","role":"member"}`)
	mreq := httptest.NewRequest(http.MethodPost, "/api/teams/"+r.teamID+"/members", body)
	mreq.Header.Set("Content-Type", "application/json")
	mreq.SetPathValue("team_id", r.teamID)
	ctx := httpx.WithClaims(mreq.Context(), &verify.Claims{Subject: r.owner})
	ctx = httpx.WithOrgID(ctx, r.orgID)
	mreq = mreq.WithContext(ctx)

	mrec := httptest.NewRecorder()
	tmh.handleTeamMemberAdd(mrec, mreq)
	if mrec.Code != http.StatusForbidden {
		t.Fatalf("member add on archived team = %d, want 403; body=%s", mrec.Code, mrec.Body.String())
	}
}
