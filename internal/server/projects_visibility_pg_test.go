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
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// projectVisibilityRig is the multi-mode fixture for TFAC-562: a real
// Postgres-backed Server (RLS live) with an org admin/founder, a plain
// write-member, a viewer, and a teamless org member (a member of the org
// but of no team) — the exact "what would a teamless user's project be?"
// scenario the ticket reported as an opaque 500.
type projectVisibilityRig struct {
	h      *pgtest.Harness
	s      *Server
	orgID  string
	teamID string
	// admin is the org founder: org role "owner" (counts as org-admin)
	// and team role "admin".
	admin string
	// member has team role "member" — can write the team, is not an org
	// admin.
	member string
	// viewer has team role "viewer" — a team member who cannot write.
	viewer string
	// teamless is an org member (org_memberships row) with no memberships
	// row at all — TFAC-562's reported case.
	teamless string
}

func newProjectVisibilityRig(t *testing.T) *projectVisibilityRig {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeMulti)

	h := pgtest.Shared(t)
	h.Reset(t)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	s := New(h.AdminDB, stores)

	orgID, founder, teamID := pgtest.SeedOrgWithUser(t, h, "founder")
	member := pgtest.SeedUser(t, h, "member")
	pgtest.AddOrgMember(t, h, member, orgID, teamID, "member", "member")
	viewer := pgtest.SeedUser(t, h, "viewer")
	pgtest.AddOrgMember(t, h, viewer, orgID, teamID, "member", "viewer")
	teamless := pgtest.SeedUser(t, h, "teamless")
	pgtest.MustExec(t, h.AdminDB,
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`, teamless, orgID)

	return &projectVisibilityRig{
		h: h, s: s, orgID: orgID, teamID: teamID,
		admin: founder, member: member, viewer: viewer, teamless: teamless,
	}
}

// req builds a request to path as callerID with claims + active org
// injected, mirroring viewerRig.req.
func (r *projectVisibilityRig) req(method, path, callerID string, body any) *http.Request {
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	ctx := httpx.WithClaims(req.Context(), &verify.Claims{Subject: callerID})
	ctx = httpx.WithOrgID(ctx, r.orgID)
	return req.WithContext(ctx)
}

func (r *projectVisibilityRig) create(t *testing.T, callerID string, body map[string]any) (*httptest.ResponseRecorder, domain.Project) {
	t.Helper()
	rec := httptest.NewRecorder()
	r.s.handleProjectCreate(rec, r.req(http.MethodPost, "/api/projects", callerID, body))
	var p domain.Project
	if rec.Code == http.StatusCreated {
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatalf("decode created project: %v", err)
		}
	}
	return rec, p
}

func (r *projectVisibilityRig) patch(t *testing.T, callerID, projectID string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := r.req(http.MethodPatch, "/api/projects/"+projectID, callerID, body)
	req.SetPathValue("id", projectID)
	r.s.handleProjectUpdate(rec, req)
	return rec
}

// TestProjectCreate_Teamless_TeamVisibility_CleanBadRequest pins the core
// TFAC-562 fix: a teamless org member requesting (explicitly or via the
// "team" default) team-visibility no longer 500s via the
// teamscope.ResolveActing org-default-team fallback tripping
// projects_insert RLS — it's a clean 400 telling them to join a team or
// pick a different visibility.
func TestProjectCreate_Teamless_TeamVisibility_CleanBadRequest(t *testing.T) {
	r := newProjectVisibilityRig(t)

	for _, body := range []map[string]any{
		{"name": "no visibility field"},
		{"name": "explicit team", "visibility": "team"},
	} {
		rec, _ := r.create(t, r.teamless, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body=%v: status = %d, want 400; resp=%s", body, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "join a team") {
			t.Errorf("body=%v: resp = %s, want a \"join a team\" hint", body, rec.Body.String())
		}
	}
}

// TestProjectCreate_Teamless_PrivateVisibility_Succeeds is the ticket's
// headline fix: a teamless user's project is private (creator-only, no
// team), not an attempt at team-scoping that RLS then rejects.
func TestProjectCreate_Teamless_PrivateVisibility_Succeeds(t *testing.T) {
	r := newProjectVisibilityRig(t)
	rec, got := r.create(t, r.teamless, map[string]any{
		"name":       "My private project",
		"visibility": "private",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if got.Visibility != domain.ProjectVisibilityPrivate {
		t.Errorf("visibility = %q, want %q", got.Visibility, domain.ProjectVisibilityPrivate)
	}
	if got.TeamID != "" {
		t.Errorf("team_id = %q, want empty (private projects are teamless)", got.TeamID)
	}
}

// TestProjectCreate_OrgVisibility_RequiresOrgAdmin exercises the
// permission model already baked into the projects_insert RLS policy:
// only an org admin may create an org-visible project. A non-admin
// (whether teamless or a plain team member) gets a 403, not a 500.
func TestProjectCreate_OrgVisibility_RequiresOrgAdmin(t *testing.T) {
	r := newProjectVisibilityRig(t)

	for _, caller := range []string{r.teamless, r.member} {
		rec, _ := r.create(t, caller, map[string]any{
			"name":       "org project attempt",
			"visibility": "org",
		})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("caller=%s: status = %d, want 403; body=%s", caller, rec.Code, rec.Body.String())
		}
	}

	rec, got := r.create(t, r.admin, map[string]any{
		"name":       "org project",
		"visibility": "org",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("org admin create: status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if got.Visibility != domain.ProjectVisibilityOrg {
		t.Errorf("visibility = %q, want %q", got.Visibility, domain.ProjectVisibilityOrg)
	}
}

// TestProjectCreate_Viewer_TeamVisibility_Forbidden confirms a bonus fix
// alongside TFAC-562's teamless case: a team VIEWER (a real member, just
// read-only) hit the exact same opaque 500 creating a team-visibility
// project, because projects_insert's WITH CHECK requires
// user_can_write_team — the same translateVisibilityErr path now maps
// that denial to a clean 403 too.
func TestProjectCreate_Viewer_TeamVisibility_Forbidden(t *testing.T) {
	r := newProjectVisibilityRig(t)

	rec, _ := r.create(t, r.viewer, map[string]any{"name": "viewer's project"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer create: status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}

	rec, got := r.create(t, r.member, map[string]any{"name": "member's project"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("member create: status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if got.TeamID != r.teamID {
		t.Errorf("team_id = %q, want %q", got.TeamID, r.teamID)
	}
}

// TestProjectCreate_NonTeamVisibility_RejectsPinnedRepos pins the v1 scope
// cut: private/org projects have no team to validate pinned repos or
// tracker keys against, so supplying either 400s instead of silently
// dropping them or crashing on a nil team lookup.
func TestProjectCreate_NonTeamVisibility_RejectsPinnedRepos(t *testing.T) {
	r := newProjectVisibilityRig(t)
	rec, _ := r.create(t, r.teamless, map[string]any{
		"name":                  "private with repos",
		"visibility":            "private",
		"pinned_repository_ids": []string{"some-repository-id"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "team visibility") {
		t.Errorf("body = %s, want a team-visibility hint", rec.Body.String())
	}
}

// TestProjectCreate_InvalidVisibility_BadRequest guards the enum: garbage
// input 400s instead of tripping the projects_visibility_check CHECK as a
// raw constraint-violation 500.
func TestProjectCreate_InvalidVisibility_BadRequest(t *testing.T) {
	r := newProjectVisibilityRig(t)
	rec, _ := r.create(t, r.member, map[string]any{"name": "P", "visibility": "public"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestProjectPatch_VisibilityDowngradeToPrivate_RequiresCreator answers
// the open question in the ticket: only the project's own creator may set
// visibility to "private", whether that's a downgrade (from team/org) or
// not — the projects_update RLS WITH CHECK's private branch requires
// creator_user_id = current_user regardless of direction. A team
// write-member who didn't create the project is blocked even though they
// can otherwise write the team's projects.
func TestProjectPatch_VisibilityDowngradeToPrivate_RequiresCreator(t *testing.T) {
	r := newProjectVisibilityRig(t)
	_, created := r.create(t, r.admin, map[string]any{"name": "team project"})

	rec := r.patch(t, r.member, created.ID, map[string]any{"visibility": "private"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-creator member downgrade: status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}

	rec = r.patch(t, r.admin, created.ID, map[string]any{"visibility": "private"})
	if rec.Code != http.StatusOK {
		t.Fatalf("creator downgrade: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got domain.Project
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Visibility != domain.ProjectVisibilityPrivate {
		t.Errorf("visibility = %q, want %q", got.Visibility, domain.ProjectVisibilityPrivate)
	}
}

// TestProjectPatch_VisibilityToOrg_RequiresOrgAdmin answers the ticket's
// other open question — the org-visibility gate is symmetric for
// upgrades and downgrades alike, since the RLS WITH CHECK only inspects
// the resulting row's visibility, not the direction of the change.
func TestProjectPatch_VisibilityToOrg_RequiresOrgAdmin(t *testing.T) {
	r := newProjectVisibilityRig(t)
	_, created := r.create(t, r.admin, map[string]any{"name": "team project"})

	rec := r.patch(t, r.member, created.ID, map[string]any{"visibility": "org"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin member -> org: status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}

	rec = r.patch(t, r.admin, created.ID, map[string]any{"visibility": "org"})
	if rec.Code != http.StatusOK {
		t.Fatalf("org admin -> org: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestProjectPatch_TeamlessProject_CannotUpgradeToTeam pins the v1 scope
// cut on the update path: a private/org project created without a team
// has no team_id to satisfy the projects_team_visibility_requires_team
// CHECK, and PATCH has no field to attach one — so switching it to
// "team" 400s with an actionable message instead of tripping the CHECK.
func TestProjectPatch_TeamlessProject_CannotUpgradeToTeam(t *testing.T) {
	r := newProjectVisibilityRig(t)
	_, created := r.create(t, r.teamless, map[string]any{
		"name":       "teamless project",
		"visibility": "private",
	})

	rec := r.patch(t, r.teamless, created.ID, map[string]any{"visibility": "team"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no team") {
		t.Errorf("body = %s, want a \"no team\" hint", rec.Body.String())
	}
}

// TestProjectPatch_TeamlessProject_TeamScopedFieldsCleanBadRequest pins a
// review-caught gap: handleProjectUpdate's pinned_repos / jira_project_key
// / spec_authorship_blueprint_id branches each guard on existing.TeamID ==
// "" and, before this test, answered internalError (500) — a "should never
// happen" guard written before TFAC-562 made a teamless project a normal,
// reachable state. Each must now 400 with an actionable message instead.
func TestProjectPatch_TeamlessProject_TeamScopedFieldsCleanBadRequest(t *testing.T) {
	r := newProjectVisibilityRig(t)
	_, created := r.create(t, r.teamless, map[string]any{
		"name":       "teamless project",
		"visibility": "private",
	})

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"pinned_repository_ids", map[string]any{"pinned_repository_ids": []string{"some-repository-id"}}},
		{"jira_project_key", map[string]any{"jira_project_key": "SKY"}},
		{"spec_authorship_blueprint_id", map[string]any{"spec_authorship_blueprint_id": "some-blueprint-id"}},
	} {
		rec := r.patch(t, r.teamless, created.ID, tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400; body=%s", tc.name, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "no team") {
			t.Errorf("%s: body = %s, want a \"no team\" hint", tc.name, rec.Body.String())
		}
	}
}
