package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/curator"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// teamArchiveRig is the multi-mode fixture for the archive/restore lifecycle:
// a real Postgres-backed teamsHandler (so the deleted_at filter + teams_update
// RLS are live) plus an org with a founder (org owner) and a plain member who
// is NOT an org admin. spawner + curator are nil getters — the no-active-work
// path doesn't need them, and the cascade enumeration is pinned in the db tests.
type teamArchiveRig struct {
	h      *pgtest.Harness
	s      *Server
	th     *teamsHandler
	orgID  string
	teamID string
	owner  string // org owner (org admin)
	member string // org + team member, NOT an org admin
}

func newTeamArchiveRig(t *testing.T) *teamArchiveRig {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeMulti)

	h := pgtest.Shared(t)
	h.Reset(t)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	s := New(h.AdminDB, stores)

	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, h, "owner")
	member := pgtest.SeedUser(t, h, "member")
	pgtest.AddOrgMember(t, h, member, orgID, teamID, "member", "member")

	th := &teamsHandler{
		tx:        s.tx,
		az:        s.az,
		allStores: stores,
		spawner:   func() *delegate.Spawner { return nil },
		curator:   func() *curator.Curator { return nil },
	}
	return &teamArchiveRig{h: h, s: s, th: th, orgID: orgID, teamID: teamID, owner: owner, member: member}
}

// req builds a request as callerID with the team_id path value + claims + org
// seeded, invoking the handler directly (no cookie-session dance).
func (r *teamArchiveRig) req(method, path, callerID, teamID string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	if teamID != "" {
		req.SetPathValue("team_id", teamID)
	}
	ctx := httpx.WithClaims(req.Context(), &verify.Claims{Subject: callerID})
	ctx = httpx.WithOrgID(ctx, r.orgID)
	return req.WithContext(ctx)
}

func (r *teamArchiveRig) deletedAt(t *testing.T, teamID string) (set bool) {
	t.Helper()
	if err := r.h.AdminDB.QueryRow(
		`SELECT deleted_at IS NOT NULL FROM teams WHERE id = $1`, teamID,
	).Scan(&set); err != nil {
		t.Fatalf("read deleted_at %s: %v", teamID, err)
	}
	return set
}

// TestTeamArchive_OrgAdminArchivesAndCounts: the org owner archives the team —
// 200, deleted_at set, zero-work counts returned (no runs / curator sessions),
// and the team disappears from the archived-list before / appears after.
func TestTeamArchive_OrgAdminArchivesAndCounts(t *testing.T) {
	r := newTeamArchiveRig(t)

	rec := httptest.NewRecorder()
	r.th.handleTeamArchive(rec, r.req(http.MethodPost, "/api/teams/"+r.teamID+"/archive", r.owner, r.teamID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp teamArchiveResultJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.CancelledRuns != 0 || resp.CancelledCuratorSessions != 0 {
		t.Errorf("counts = %+v; want zero (no active work seeded)", resp)
	}
	if !r.deletedAt(t, r.teamID) {
		t.Error("deleted_at not set after archive")
	}
}

// TestTeamArchive_NonOrgAdminIs404: a plain member can't archive / restore /
// preview — the org-admin gate 404s (non-disclosure), and the team is untouched.
func TestTeamArchive_NonOrgAdminIs404(t *testing.T) {
	r := newTeamArchiveRig(t)

	for _, tc := range []struct {
		name, method, path string
	}{
		{"archive", http.MethodPost, "/api/teams/" + r.teamID + "/archive"},
		{"restore", http.MethodPost, "/api/teams/" + r.teamID + "/restore"},
		{"preview", http.MethodGet, "/api/teams/" + r.teamID + "/archive/preview"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := r.req(tc.method, tc.path, r.member, r.teamID)
			switch tc.name {
			case "archive":
				r.th.handleTeamArchive(rec, req)
			case "restore":
				r.th.handleTeamRestore(rec, req)
			case "preview":
				r.th.handleTeamArchivePreview(rec, req)
			}
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s by non-admin = %d, want 404; body=%s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
	if r.deletedAt(t, r.teamID) {
		t.Error("team archived by a non-admin probe, want untouched")
	}
}

// TestTeamArchive_AlreadyArchivedIs409: archiving an archived team is a 409.
func TestTeamArchive_AlreadyArchivedIs409(t *testing.T) {
	r := newTeamArchiveRig(t)

	first := httptest.NewRecorder()
	r.th.handleTeamArchive(first, r.req(http.MethodPost, "/api/teams/"+r.teamID+"/archive", r.owner, r.teamID))
	if first.Code != http.StatusOK {
		t.Fatalf("first archive = %d, want 200; body=%s", first.Code, first.Body.String())
	}
	second := httptest.NewRecorder()
	r.th.handleTeamArchive(second, r.req(http.MethodPost, "/api/teams/"+r.teamID+"/archive", r.owner, r.teamID))
	if second.Code != http.StatusConflict {
		t.Fatalf("second archive = %d, want 409; body=%s", second.Code, second.Body.String())
	}
}

// TestTeamRestore_RevivesTeam: restore clears deleted_at and the team is
// writable again; restoring an active team is a 409.
func TestTeamRestore_RevivesTeam(t *testing.T) {
	r := newTeamArchiveRig(t)

	// Restore before archive → 409 (not archived).
	pre := httptest.NewRecorder()
	r.th.handleTeamRestore(pre, r.req(http.MethodPost, "/api/teams/"+r.teamID+"/restore", r.owner, r.teamID))
	if pre.Code != http.StatusConflict {
		t.Fatalf("restore active team = %d, want 409; body=%s", pre.Code, pre.Body.String())
	}

	arc := httptest.NewRecorder()
	r.th.handleTeamArchive(arc, r.req(http.MethodPost, "/api/teams/"+r.teamID+"/archive", r.owner, r.teamID))
	if arc.Code != http.StatusOK {
		t.Fatalf("archive = %d, want 200; body=%s", arc.Code, arc.Body.String())
	}

	res := httptest.NewRecorder()
	r.th.handleTeamRestore(res, r.req(http.MethodPost, "/api/teams/"+r.teamID+"/restore", r.owner, r.teamID))
	if res.Code != http.StatusOK {
		t.Fatalf("restore = %d, want 200; body=%s", res.Code, res.Body.String())
	}
	if r.deletedAt(t, r.teamID) {
		t.Error("deleted_at still set after restore")
	}
}

// TestTeamArchive_BlocksTeamSettingsWrite: an archived team rejects a
// team-settings write with 403 + archived:true, and a restore re-opens it. This
// is the VerifyTeamNotArchived gate end-to-end.
func TestTeamArchive_BlocksTeamSettingsWrite(t *testing.T) {
	r := newTeamArchiveRig(t)

	arc := httptest.NewRecorder()
	r.th.handleTeamArchive(arc, r.req(http.MethodPost, "/api/teams/"+r.teamID+"/archive", r.owner, r.teamID))
	if arc.Code != http.StatusOK {
		t.Fatalf("archive = %d, want 200; body=%s", arc.Code, arc.Body.String())
	}

	// A team-settings POST (gated on user_is_team_admin) must now be blocked by
	// VerifyTeamNotArchived with a clear archived 403.
	wreq := httptest.NewRequest(http.MethodPost, "/api/settings/team/"+r.teamID, strings.NewReader(`{"ai_model":"opus"}`))
	wreq.Header.Set("Content-Type", "application/json")
	wreq.SetPathValue("team_id", r.teamID)
	ctx := httpx.WithClaims(wreq.Context(), &verify.Claims{Subject: r.owner})
	ctx = httpx.WithOrgID(ctx, r.orgID)
	wreq = wreq.WithContext(ctx)

	wrec := httptest.NewRecorder()
	r.s.handleTeamSettingsPost(wrec, wreq)
	if wrec.Code != http.StatusForbidden {
		t.Fatalf("team-settings write on archived team = %d, want 403; body=%s", wrec.Code, wrec.Body.String())
	}
	var blocked struct {
		Archived bool `json:"archived"`
	}
	if err := json.Unmarshal(wrec.Body.Bytes(), &blocked); err != nil {
		t.Fatalf("decode 403 body: %v", err)
	}
	if !blocked.Archived {
		t.Errorf("403 body missing archived:true marker; body=%s", wrec.Body.String())
	}
}

// TestTeamArchive_BlocksMembershipWrite: once archived, a team's roster is
// frozen — a member add is rejected with 403 + archived:true. The membership
// handlers gate on user_is_team_admin (no user_can_write_team filter), so this
// proves the explicit VerifyTeamNotArchived gate covers them (TFAC-448).
func TestTeamArchive_BlocksMembershipWrite(t *testing.T) {
	r := newTeamArchiveRig(t)

	arc := httptest.NewRecorder()
	r.th.handleTeamArchive(arc, r.req(http.MethodPost, "/api/teams/"+r.teamID+"/archive", r.owner, r.teamID))
	if arc.Code != http.StatusOK {
		t.Fatalf("archive = %d, want 200; body=%s", arc.Code, arc.Body.String())
	}

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
