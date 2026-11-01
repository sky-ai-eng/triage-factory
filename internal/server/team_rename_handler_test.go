package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// teamRenameRig is a Postgres-backed teamsHandler with RLS live, and an org
// with a founder (org owner, team admin), a plain team member, and an org admin
// who is not on the team.
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

// req builds a PATCH as callerID, with the team_id path value, claims and active
// org that withSession would supply, for calling the handler directly.
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

// TestTeamRename_TeamAdminRenamesOwnTeam: a team admin who is not an org admin
// renames their team; the name, re-derived slug and description persist and
// the response echoes the row.
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

// TestTeamRename_OrgAdminRenamesAnyTeam: an org admin who is not on the team can
// rename it.
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

// TestTeamRename_Refusals: each refused PATCH answers its status and leaves the
// team as seeded (name = slug = "default", no description).
func TestTeamRename_Refusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, r *teamRenameRig) (caller, teamID string)
		body  any
		want  int
	}{
		{"non-admin member is 403", func(t *testing.T, r *teamRenameRig) (string, string) {
			// 403, not 404: the member is in the org and can see the team.
			return r.memb, r.teamID
		}, map[string]string{"name": "Hijacked"}, http.StatusForbidden},
		{"team in another org is 404", func(t *testing.T, r *teamRenameRig) (string, string) {
			// The org check runs before the role gate, so another org's team
			// cannot be renamed or probed.
			_, _, otherTeam := pgtest.SeedOrgWithUser(t, r.h, "rival")
			return r.admin, otherTeam
		}, map[string]string{"name": "Cross Org"}, http.StatusNotFound},
		{"non-UUID team is 404", func(t *testing.T, r *teamRenameRig) (string, string) {
			// Refused before any DB hit, so it cannot surface as a 500 from the
			// uuid cast.
			return r.admin, "not-a-uuid"
		}, map[string]string{"name": "X"}, http.StatusNotFound},
		{"empty body is 400", func(t *testing.T, r *teamRenameRig) (string, string) {
			return r.admin, r.teamID
		}, map[string]struct{}{}, http.StatusBadRequest},
		{"description too long is 400", func(t *testing.T, r *teamRenameRig) (string, string) {
			return r.admin, r.teamID
		}, map[string]string{"description": strings.Repeat("a", maxTeamDescriptionLen+1)}, http.StatusBadRequest},
		{"slug collision is 409", func(t *testing.T, r *teamRenameRig) (string, string) {
			// The UNIQUE (org_id, slug) violation surfaces as 409, as on create.
			pgtest.MustExec(t, r.h.AdminDB,
				`INSERT INTO teams (org_id, slug, name) VALUES ($1, 'taken', 'Taken')`, r.orgID)
			return r.admin, r.teamID
		}, map[string]string{"name": "Taken"}, http.StatusConflict},
		{"local mode is 404", func(t *testing.T, r *teamRenameRig) (string, string) {
			runmode.SetForTest(t, runmode.ModeLocal)
			return r.admin, r.teamID
		}, map[string]string{"name": "Local"}, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newTeamRenameRig(t)
			caller, teamID := tc.setup(t, r)
			// Another org's team is the PATCH target in the cross-org case, so
			// its row is checked against what it held before the request.
			otherTarget := teamID != r.teamID && uuid.Validate(teamID) == nil
			var before [3]string
			if otherTarget {
				before[0], before[1], before[2] = r.nameSlugDescOf(t, teamID)
			}

			rec := httptest.NewRecorder()
			r.th.handleTeamUpdate(rec, r.req(caller, teamID, tc.body))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
			if name, slug, desc := r.nameSlugDescOf(t, r.teamID); name != "default" || slug != "default" || desc != "" {
				t.Errorf("team = (%q, %q, %q) after a refused PATCH, want it as seeded", name, slug, desc)
			}
			if otherTarget {
				if name, slug, desc := r.nameSlugDescOf(t, teamID); [3]string{name, slug, desc} != before {
					t.Errorf("targeted team = (%q, %q, %q) after a refused PATCH, want %q", name, slug, desc, before)
				}
			}
		})
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

// TestTeamGet_ReadsBackTheDescription_Postgres: a description-only PATCH keeps
// the name and slug, and the single read returns the description it wrote,
// including a clear to "".
func TestTeamGet_ReadsBackTheDescription_Postgres(t *testing.T) {
	r := newTeamRenameRig(t)
	const blurb = "Owns the ingestion pipeline and its on-call rotation."

	rec := httptest.NewRecorder()
	r.th.handleTeamUpdate(rec, r.req(r.admin, r.teamID, map[string]string{"description": blurb}))
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH description = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// SeedOrgWithUser names the default team "default", slug "default".
	if name, slug, _ := r.nameSlugDescOf(t, r.teamID); name != "default" || slug != "default" {
		t.Errorf("name/slug = (%q, %q) after a description-only PATCH, want unchanged (default)", name, slug)
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

// TestTeamGet_DisclosureSplit_Postgres: org membership, not team membership, is
// the read gate for a team, so an org admin not on the team reads it, and a
// team in another org is a 404.
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
