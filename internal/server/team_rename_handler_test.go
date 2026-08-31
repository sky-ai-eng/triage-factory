package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// teamRenameRig is the multi-mode fixture for PATCH /api/teams/{team_id}: a
// real Postgres-backed teamsHandler (so the widened teams_update RLS is live)
// plus an org with a founder (org owner + sole team admin), a plain team
// member, and an org admin who is NOT on the team — the team-admin, org-admin,
// and non-admin-member branches of the rename gate.
type teamRenameRig struct {
	h        *pgtest.Harness
	th       *teamsHandler
	orgID    string
	teamID   string
	admin    string // founder: org owner + team admin
	memb     string // org member + plain team member
	orgAdmin string // org admin, deliberately NOT on the team
}

func newTeamRenameRig(t *testing.T) *teamRenameRig {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeMulti)

	h := pgtest.Shared(t)
	h.Reset(t)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	s := New(h.AdminDB, stores)

	orgID, founder, teamID := pgtest.SeedOrgWithUser(t, h, "founder")
	memb := pgtest.SeedUser(t, h, "member")
	pgtest.AddOrgMember(t, h, memb, orgID, teamID, "member", "member")
	orgAdmin := pgtest.SeedUser(t, h, "orgadmin")
	pgtest.MustExec(t, h.AdminDB,
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'admin')`, orgAdmin, orgID)

	return &teamRenameRig{
		h:        h,
		th:       &teamsHandler{tx: s.tx, az: s.az, allStores: stores},
		orgID:    orgID,
		teamID:   teamID,
		admin:    founder,
		memb:     memb,
		orgAdmin: orgAdmin,
	}
}

// req builds a PATCH against the rename handler as callerID, the team_id path
// value set and the caller's claims + active org seeded — the state
// withSession/withOrg would normally supply. We invoke the handler directly so
// the test exercises authz + RLS without the cookie-session dance.
func (r *teamRenameRig) req(callerID, teamID string, body any) *http.Request {
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(http.MethodPatch, "/api/teams/"+teamID, rdr)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("team_id", teamID)
	ctx := httpx.WithClaims(req.Context(), &verify.Claims{Subject: callerID})
	ctx = httpx.WithOrgID(ctx, r.orgID)
	return req.WithContext(ctx)
}

func (r *teamRenameRig) nameSlugDescOf(t *testing.T, teamID string) (name, slug, desc string) {
	t.Helper()
	if err := r.h.AdminDB.QueryRow(
		`SELECT name, slug, COALESCE(description, '') FROM teams WHERE id = $1`, teamID,
	).Scan(&name, &slug, &desc); err != nil {
		t.Fatalf("read team %s: %v", teamID, err)
	}
	return name, slug, desc
}

// TestTeamRename_TeamAdminRenamesOwnTeam: the founder (team admin) renames their
// own team — 200, name + re-derived slug + description persist, and the response
// echoes the updated row. This is the slice's reason for existing: the widened
// teams_update RLS lets a team admin write without org-admin.
func TestTeamRename_TeamAdminRenamesOwnTeam(t *testing.T) {
	r := newTeamRenameRig(t)

	rec := httptest.NewRecorder()
	r.th.handleTeamUpdate(rec, r.req(r.admin, r.teamID, map[string]string{
		"name": "Platform Eng", "description": "Owns the build",
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp teamJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Name != "Platform Eng" || resp.Slug != "platform-eng" || resp.Description != "Owns the build" {
		t.Errorf("response = %+v; want name=Platform Eng slug=platform-eng desc=Owns the build", resp)
	}
	name, slug, desc := r.nameSlugDescOf(t, r.teamID)
	if name != "Platform Eng" || slug != "platform-eng" || desc != "Owns the build" {
		t.Errorf("persisted = (%q, %q, %q); want renamed", name, slug, desc)
	}
}

// TestTeamRename_NonAdminMemberIs403: a plain team member can't rename — the
// team-admin-or-org-admin gate answers 403 (not 404: they're inside the org and
// can see the team), and the row is untouched.
func TestTeamRename_NonAdminMemberIs403(t *testing.T) {
	r := newTeamRenameRig(t)

	rec := httptest.NewRecorder()
	r.th.handleTeamUpdate(rec, r.req(r.memb, r.teamID, map[string]string{"name": "Hijacked"}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (non-admin member); body=%s", rec.Code, rec.Body.String())
	}
	if name, _, _ := r.nameSlugDescOf(t, r.teamID); name == "Hijacked" {
		t.Errorf("team renamed by non-admin member, want unchanged")
	}
}

// TestTeamRename_OrgAdminRenamesAnyTeam: an org admin who isn't on the team can
// still rename it — the org-admin branch of the gate (and the org-admin half of
// the widened RLS) both permit it.
func TestTeamRename_OrgAdminRenamesAnyTeam(t *testing.T) {
	r := newTeamRenameRig(t)

	rec := httptest.NewRecorder()
	r.th.handleTeamUpdate(rec, r.req(r.orgAdmin, r.teamID, map[string]string{"name": "Renamed By Org Admin"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (org admin renames); body=%s", rec.Code, rec.Body.String())
	}
	if name, _, _ := r.nameSlugDescOf(t, r.teamID); name != "Renamed By Org Admin" {
		t.Errorf("name = %q after org-admin rename, want %q", name, "Renamed By Org Admin")
	}
}

// TestTeamRename_CrossOrgTeamIs404: a team_id in a DIFFERENT org 404s
// (VerifyTeamInOrg fires before the role gate), so a caller can't rename — or
// even probe the existence of — another org's team.
func TestTeamRename_CrossOrgTeamIs404(t *testing.T) {
	r := newTeamRenameRig(t)
	_, _, otherTeam := pgtest.SeedOrgWithUser(t, r.h, "rival")

	rec := httptest.NewRecorder()
	r.th.handleTeamUpdate(rec, r.req(r.admin, otherTeam, map[string]string{"name": "Cross Org"}))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (cross-org team); body=%s", rec.Code, rec.Body.String())
	}
}

// TestTeamRename_EmptyNameIs400: a present-but-empty name is rejected before any
// write — the row keeps its original name.
func TestTeamRename_EmptyNameIs400(t *testing.T) {
	r := newTeamRenameRig(t)

	rec := httptest.NewRecorder()
	r.th.handleTeamUpdate(rec, r.req(r.admin, r.teamID, map[string]string{"name": "   "}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (empty name); body=%s", rec.Code, rec.Body.String())
	}
	if name, _, _ := r.nameSlugDescOf(t, r.teamID); name == "" {
		t.Errorf("name cleared on rejected empty-name update, want original")
	}
}

// TestTeamRename_DescriptionOnly: a body with no name leaves name + slug
// untouched (the COALESCE(NULL, name) partial-PATCH path, all the way through
// the widened RLS) while the description is written. The team admin is the
// caller, so this also confirms the description-only write passes the policy.
func TestTeamRename_DescriptionOnly(t *testing.T) {
	r := newTeamRenameRig(t)

	rec := httptest.NewRecorder()
	r.th.handleTeamUpdate(rec, r.req(r.admin, r.teamID, map[string]string{"description": "owns the build"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (description-only); body=%s", rec.Code, rec.Body.String())
	}
	name, slug, desc := r.nameSlugDescOf(t, r.teamID)
	if name != "default" || slug != "default" {
		// SeedOrgWithUser seeds the default team with name = slug = "default".
		t.Errorf("name/slug = (%q, %q) after description-only update, want unchanged (default)", name, slug)
	}
	if desc != "owns the build" {
		t.Errorf("description = %q, want %q", desc, "owns the build")
	}
}

// TestTeamRename_EmptyBodyIs400: a body with neither field is rejected — there's
// nothing to update, and an empty PATCH shouldn't silently bump updated_at.
func TestTeamRename_EmptyBodyIs400(t *testing.T) {
	r := newTeamRenameRig(t)

	rec := httptest.NewRecorder()
	r.th.handleTeamUpdate(rec, r.req(r.admin, r.teamID, map[string]struct{}{}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (nothing to update); body=%s", rec.Code, rec.Body.String())
	}
}

// TestTeamRename_NameTooLongIs400: a name past the rune cap is rejected before
// any write.
func TestTeamRename_NameTooLongIs400(t *testing.T) {
	r := newTeamRenameRig(t)

	rec := httptest.NewRecorder()
	r.th.handleTeamUpdate(rec, r.req(r.admin, r.teamID, map[string]string{
		"name": strings.Repeat("a", maxTeamNameLen+1),
	}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (name too long); body=%s", rec.Code, rec.Body.String())
	}
}

// TestTeamRename_DescriptionTooLongIs400: a description past the rune cap is
// rejected before any write.
func TestTeamRename_DescriptionTooLongIs400(t *testing.T) {
	r := newTeamRenameRig(t)

	rec := httptest.NewRecorder()
	r.th.handleTeamUpdate(rec, r.req(r.admin, r.teamID, map[string]string{
		"description": strings.Repeat("a", maxTeamDescriptionLen+1),
	}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (description too long); body=%s", rec.Code, rec.Body.String())
	}
}

// TestTeamRename_SlugCollisionIs409: renaming to a name whose re-derived slug
// collides with a sibling team in the org trips the UNIQUE (org_id, slug)
// constraint, surfaced as a 409 (mirroring Create), not a 500.
func TestTeamRename_SlugCollisionIs409(t *testing.T) {
	r := newTeamRenameRig(t)
	// A second team in the same org owning the slug "taken".
	pgtest.MustExec(t, r.h.AdminDB,
		`INSERT INTO teams (org_id, slug, name) VALUES ($1, 'taken', 'Taken')`, r.orgID)

	rec := httptest.NewRecorder()
	r.th.handleTeamUpdate(rec, r.req(r.admin, r.teamID, map[string]string{"name": "Taken"}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (slug collision); body=%s", rec.Code, rec.Body.String())
	}
}

// TestTeamRename_NonUUIDTeamIs404: a malformed team_id in the path 404s before
// any DB hit, so a bad path can't surface as a 500 from the uuid cast.
func TestTeamRename_NonUUIDTeamIs404(t *testing.T) {
	r := newTeamRenameRig(t)

	rec := httptest.NewRecorder()
	r.th.handleTeamUpdate(rec, r.req(r.admin, "not-a-uuid", map[string]string{"name": "X"}))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (non-UUID team_id); body=%s", rec.Code, rec.Body.String())
	}
}

// TestTeamRename_LocalIs404: the rename surface is multi-only — PATCH 404s in
// local mode (N=1 has one team and hides team management).
func TestTeamRename_LocalIs404(t *testing.T) {
	r := newTeamRenameRig(t)
	runmode.SetForTest(t, runmode.ModeLocal)

	rec := httptest.NewRecorder()
	r.th.handleTeamUpdate(rec, r.req(r.admin, r.teamID, map[string]string{"name": "Local"}))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 in local mode; body=%s", rec.Code, rec.Body.String())
	}
}

// getReq builds a GET against the team single read as callerID, with the same
// path value + claims + active org the PATCH helper seeds.
func (r *teamRenameRig) getReq(callerID, teamID string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/teams/"+teamID, nil)
	req.SetPathValue("team_id", teamID)
	ctx := httpx.WithClaims(req.Context(), &verify.Claims{Subject: callerID})
	ctx = httpx.WithOrgID(ctx, r.orgID)
	return req.WithContext(ctx)
}

// TestTeamGet_ReadsBackTheDescription_Postgres is the regression the single
// read exists for: teams.description was WRITE-ONLY. PATCH accepted it, the
// store persisted it, and no read on the whole surface returned it — so the
// field a user typed was unreadable by the UI that typed it, and nothing
// failed to say so. Only a round-trip catches that class.
func TestTeamGet_ReadsBackTheDescription_Postgres(t *testing.T) {
	r := newTeamRenameRig(t)
	const blurb = "Owns the ingestion pipeline and its on-call rotation."

	rec := httptest.NewRecorder()
	r.th.handleTeamUpdate(rec, r.req(r.admin, r.teamID, map[string]string{"description": blurb}))
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH description = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	r.th.handleTeamGet(rec, r.getReq(r.admin, r.teamID))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got teamJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Description != blurb {
		t.Errorf("description = %q, want the value PATCH wrote (%q)", got.Description, blurb)
	}

	// Clearing round-trips too, so "" is a readable state rather than
	// indistinguishable from a field the read forgot to project.
	rec = httptest.NewRecorder()
	r.th.handleTeamUpdate(rec, r.req(r.admin, r.teamID, map[string]string{"description": ""}))
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH clear = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	r.th.handleTeamGet(rec, r.getReq(r.admin, r.teamID))
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode after clear: %v", err)
	}
	if got.Description != "" {
		t.Errorf("description after clear = %q, want empty", got.Description)
	}
}

// TestTeamGet_DisclosureSplit_Postgres pins the two-valued denial: a team in
// another org is 404 (it isn't in the caller's list either — disclose
// nothing), while a team in the caller's own org that they are simply not on
// is still readable, because team membership is not the read gate for a team's
// identity — org membership is. The 403 arm belongs to the writes.
func TestTeamGet_DisclosureSplit_Postgres(t *testing.T) {
	r := newTeamRenameRig(t)

	// The org admin is deliberately NOT on the team, and still reads it.
	rec := httptest.NewRecorder()
	r.th.handleTeamGet(rec, r.getReq(r.orgAdmin, r.teamID))
	if rec.Code != http.StatusOK {
		t.Errorf("org admin (not on the team) = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// A team in a different org: 404, not 403.
	otherOrg, otherFounder, otherTeam := pgtest.SeedOrgWithUser(t, r.h, "other-founder")
	_ = otherOrg
	_ = otherFounder
	rec = httptest.NewRecorder()
	r.th.handleTeamGet(rec, r.getReq(r.admin, otherTeam))
	if rec.Code != http.StatusNotFound {
		t.Errorf("cross-org team = %d, want 404 (non-disclosure); body=%s", rec.Code, rec.Body.String())
	}
}
