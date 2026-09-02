package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// teamMembersRig is the multi-mode fixture for the team roster handler: a real
// Postgres-backed Server (so RLS + the guard_team_admins trigger are live) plus
// a fully-wired org with a founder (org owner + sole team admin), a plain team
// member, and an org admin who is NOT on the team — enough to exercise the
// team-admin, org-admin, self, and non-manager branches of every endpoint.
type teamMembersRig struct {
	h        *pgtest.Harness
	tmh      *teamMembersHandler
	orgID    string
	teamID   string
	admin    string // founder: org owner + team admin (the team's sole admin)
	memb     string // org member + team member
	orgAdmin string // org admin, deliberately NOT on the team
}

func newTeamMembersRig(t *testing.T) *teamMembersRig {
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
	// Org admin but NOT on the team (no memberships row) — exercises the
	// org-admin-can-manage branch of the team mutate gate.
	pgtest.MustExec(t, h.AdminDB,
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'admin')`, orgAdmin, orgID)

	return &teamMembersRig{
		h:        h,
		tmh:      &teamMembersHandler{tx: s.tx, az: s.az},
		orgID:    orgID,
		teamID:   teamID,
		admin:    founder,
		memb:     memb,
		orgAdmin: orgAdmin,
	}
}

// req builds a request to the team roster handler as callerID with the team_id
// (and optional user_id) path values set, the caller's claims injected, and the
// active org seeded in context — the state withSession/withOrg would normally
// supply. We invoke the handler methods directly (not through the mux) so the
// test exercises authz + RLS without the cookie-session dance.
func (r *teamMembersRig) req(method, callerID, targetID string, body any) *http.Request {
	return r.reqTeam(method, callerID, r.teamID, targetID, body)
}

// reqTeam is req with an explicit team_id — the cross-org test points it at a
// team in a different org while keeping the active org pinned to r.orgID.
func (r *teamMembersRig) reqTeam(method, callerID, teamID, targetID string, body any) *http.Request {
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	path := "/api/teams/" + teamID + "/members"
	if targetID != "" {
		path += "/" + targetID
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("team_id", teamID)
	if targetID != "" {
		req.SetPathValue("user_id", targetID)
	}
	ctx := httpx.WithClaims(req.Context(), &verify.Claims{Subject: callerID})
	ctx = httpx.WithOrgID(ctx, r.orgID)
	return req.WithContext(ctx)
}

func (r *teamMembersRig) teamRoleOf(t *testing.T, userID string) string {
	t.Helper()
	var role string
	if err := r.h.AdminDB.QueryRow(
		`SELECT role::text FROM memberships WHERE team_id = $1 AND user_id = $2`,
		r.teamID, userID,
	).Scan(&role); err != nil {
		t.Fatalf("read team role for %s: %v", userID, err)
	}
	return role
}

func (r *teamMembersRig) isTeamMember(t *testing.T, userID string) bool {
	t.Helper()
	var n int
	if err := r.h.AdminDB.QueryRow(
		`SELECT COUNT(*) FROM memberships WHERE team_id = $1 AND user_id = $2`,
		r.teamID, userID,
	).Scan(&n); err != nil {
		t.Fatalf("count team membership for %s: %v", userID, err)
	}
	return n > 0
}

// TestTeamRosterList_AnyMemberReads: a plain team member can GET the full
// roster, and each row carries its team role + host-scoped identity readiness —
// the admin's seeded GitHub login resolves; the member reads "Not connected".
func TestTeamRosterList_AnyMemberReads(t *testing.T) {
	r := newTeamMembersRig(t)

	// Give the admin a GitHub login under the default github.com host, leaving
	// the org's github_base_url UNSET — the roster must surface a peer's
	// readiness (admin-pool enrichment past the self-only identity RLS) and
	// resolve the unset host to the deployment default (EffectiveGitHubHost).
	pgtest.MustExec(t, r.h.AdminDB,
		`INSERT INTO user_github_identities (user_id, github_base_url, login, source, verified_at)
		 VALUES ($1, 'https://github.com', 'founder-gh', 'pat', now())`, r.admin)

	rec := httptest.NewRecorder()
	r.tmh.handleTeamRosterList(rec, r.req(http.MethodPost, r.memb, "", map[string]any{}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var resp teamRosterListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("items = %d, want 2 (admin, member)", len(resp.Items))
	}
	if resp.TotalCount != 2 {
		t.Errorf("total_count = %d, want 2 (the members, never the bot)", resp.TotalCount)
	}
	if resp.NextPageToken != "" {
		t.Errorf("next_page_token = %q, want empty (single page)", resp.NextPageToken)
	}
	byID := map[string]teamRosterRow{}
	for _, m := range resp.Items {
		byID[m.UserID] = m
	}
	if got := byID[r.admin].Role; got != "admin" {
		t.Errorf("admin role = %q, want admin", got)
	}
	if got := byID[r.memb].Role; got != "member" {
		t.Errorf("member role = %q, want member", got)
	}
	// is_current_user is stamped for the caller (the member) only.
	if !byID[r.memb].IsCurrentUser {
		t.Errorf("member row is_current_user = false, want true (caller)")
	}
	if byID[r.admin].IsCurrentUser {
		t.Errorf("admin row is_current_user = true, want false")
	}
	if byID[r.admin].GitHubUsername == nil || *byID[r.admin].GitHubUsername != "founder-gh" {
		t.Errorf("admin github_username = %v, want founder-gh", byID[r.admin].GitHubUsername)
	}
	if byID[r.memb].GitHubUsername != nil {
		t.Errorf("member github_username = %v, want nil (not connected)", *byID[r.memb].GitHubUsername)
	}
	// No agent bootstrapped in this fixture → null bot row.
	if resp.Bot != nil {
		t.Errorf("bot = %+v, want nil (no agent bootstrapped)", resp.Bot)
	}
}

// TestTeamRosterList_BotRow: with an agent + a per-team team_agents override
// seeded, the roster's bot row reflects the override (enabled + model).
func TestTeamRosterList_BotRow(t *testing.T) {
	r := newTeamMembersRig(t)

	var agentID string
	if err := r.h.AdminDB.QueryRow(`
		INSERT INTO agents (org_id, display_name, default_model)
		VALUES ($1, 'Test Bot', 'claude-default') RETURNING id::text
	`, r.orgID).Scan(&agentID); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	pgtest.MustExec(t, r.h.AdminDB, `
		INSERT INTO team_agents (team_id, agent_id, enabled, per_team_model)
		VALUES ($1, $2, TRUE, 'claude-team-override')
	`, r.teamID, agentID)

	rec := httptest.NewRecorder()
	r.tmh.handleTeamRosterList(rec, r.req(http.MethodPost, r.admin, "", map[string]any{}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp teamRosterListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, m := range resp.Items {
		if m.UserID == resp.Bot.AgentID {
			t.Errorf("the bot appears in items; it belongs beside the page")
		}
	}
	if resp.TotalCount != len(resp.Items) {
		t.Errorf("total_count = %d, want %d (the members, never the bot)", resp.TotalCount, len(resp.Items))
	}
	if resp.Bot == nil {
		t.Fatalf("bot = nil, want the seeded agent row")
	}
	if resp.Bot.AgentID != agentID {
		t.Errorf("bot agent_id = %q, want %q", resp.Bot.AgentID, agentID)
	}
	if !resp.Bot.Enabled {
		t.Errorf("bot enabled = false, want true")
	}
	if resp.Bot.Model != "claude-team-override" {
		t.Errorf("bot model = %q, want the per-team override", resp.Bot.Model)
	}
}

// TestTeamMemberRoleChange_NonManagerIsForbidden: a plain member can't change
// roles — the team-admin-or-org-admin gate answers 403 (the team is in their
// org and they can list it, so the denial names the missing role rather than
// hiding the team), and the row is untouched.
func TestTeamMemberRoleChange_NonManagerIsForbidden(t *testing.T) {
	r := newTeamMembersRig(t)

	rec := httptest.NewRecorder()
	r.tmh.handleTeamMemberRoleChange(rec, r.req(http.MethodPatch, r.memb, r.admin, map[string]string{"role": "member"}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (non-manager); body=%s", rec.Code, rec.Body.String())
	}
	if got := r.teamRoleOf(t, r.admin); got != "admin" {
		t.Errorf("admin role = %q after blocked change, want admin (unchanged)", got)
	}
}

// TestTeamMemberRoleChange_AdminPromotes: a team admin promotes the member to
// admin — 204 and the role flips.
func TestTeamMemberRoleChange_AdminPromotes(t *testing.T) {
	r := newTeamMembersRig(t)

	rec := httptest.NewRecorder()
	r.tmh.handleTeamMemberRoleChange(rec, r.req(http.MethodPatch, r.admin, r.memb, map[string]string{"role": "admin"}))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if got := r.teamRoleOf(t, r.memb); got != "admin" {
		t.Errorf("member role = %q after promote, want admin", got)
	}
}

// TestTeamMemberRoleChange_OrgAdminCanManage: an org admin who isn't on the team
// can still change a team member's role — the org-admin branch of the mutate
// gate (and the org-admin-via-team RLS) both permit it.
func TestTeamMemberRoleChange_OrgAdminCanManage(t *testing.T) {
	r := newTeamMembersRig(t)

	rec := httptest.NewRecorder()
	r.tmh.handleTeamMemberRoleChange(rec, r.req(http.MethodPatch, r.orgAdmin, r.memb, map[string]string{"role": "viewer"}))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (org admin manages); body=%s", rec.Code, rec.Body.String())
	}
	if got := r.teamRoleOf(t, r.memb); got != "viewer" {
		t.Errorf("member role = %q after org-admin change, want viewer", got)
	}
}

// TestTeamMemberRoleChange_AssignViewer: this slice lets you assign all three
// roles, including viewer (whose read-only enforcement is a later slice).
func TestTeamMemberRoleChange_AssignViewer(t *testing.T) {
	r := newTeamMembersRig(t)

	rec := httptest.NewRecorder()
	r.tmh.handleTeamMemberRoleChange(rec, r.req(http.MethodPatch, r.admin, r.memb, map[string]string{"role": "viewer"}))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if got := r.teamRoleOf(t, r.memb); got != "viewer" {
		t.Errorf("member role = %q after assign-viewer, want viewer", got)
	}
}

// TestTeamMemberRoleChange_InvalidRoleIs400: an unknown role value is rejected
// before it reaches the column.
func TestTeamMemberRoleChange_InvalidRoleIs400(t *testing.T) {
	r := newTeamMembersRig(t)

	rec := httptest.NewRecorder()
	r.tmh.handleTeamMemberRoleChange(rec, r.req(http.MethodPatch, r.admin, r.memb, map[string]string{"role": "superuser"}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (invalid role); body=%s", rec.Code, rec.Body.String())
	}
}

// TestTeamMemberRoleChange_LastAdminIs409: demoting the team's sole admin trips
// the guard_team_admins trigger; the handler surfaces a friendly 409, not a
// 500, and the admin keeps their role.
func TestTeamMemberRoleChange_LastAdminIs409(t *testing.T) {
	r := newTeamMembersRig(t)

	// The founder demotes themselves (the sole admin) → guard fires.
	rec := httptest.NewRecorder()
	r.tmh.handleTeamMemberRoleChange(rec, r.req(http.MethodPatch, r.admin, r.admin, map[string]string{"role": "member"}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (last admin); body=%s", rec.Code, rec.Body.String())
	}
	if got := r.teamRoleOf(t, r.admin); got != "admin" {
		t.Errorf("admin role = %q after blocked demote, want admin (unchanged)", got)
	}
}

// TestTeamMemberRemove_LastAdminIs409: removing the sole admin trips the same
// guard — 409, admin still present.
func TestTeamMemberRemove_LastAdminIs409(t *testing.T) {
	r := newTeamMembersRig(t)

	rec := httptest.NewRecorder()
	r.tmh.handleTeamMemberRemove(rec, r.req(http.MethodDelete, r.admin, r.admin, nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (last admin self-leave); body=%s", rec.Code, rec.Body.String())
	}
	if !r.isTeamMember(t, r.admin) {
		t.Errorf("admin removed despite last-admin guard, want still a member")
	}
}

// TestTeamMemberRemove_SelfLeaveNonAdmin: a non-admin may remove themselves
// without manage rights — 204, and they're gone.
func TestTeamMemberRemove_SelfLeaveNonAdmin(t *testing.T) {
	r := newTeamMembersRig(t)

	rec := httptest.NewRecorder()
	r.tmh.handleTeamMemberRemove(rec, r.req(http.MethodDelete, r.memb, r.memb, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (self-leave); body=%s", rec.Code, rec.Body.String())
	}
	if r.isTeamMember(t, r.memb) {
		t.Errorf("member still present after self-leave, want removed")
	}
}

// TestTeamMemberRemove_AdminRemovesMember: the happy path through the manage
// branch — a team admin removes a member (not themselves) — 204 and gone.
func TestTeamMemberRemove_AdminRemovesMember(t *testing.T) {
	r := newTeamMembersRig(t)

	rec := httptest.NewRecorder()
	r.tmh.handleTeamMemberRemove(rec, r.req(http.MethodDelete, r.admin, r.memb, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (admin removes member); body=%s", rec.Code, rec.Body.String())
	}
	if r.isTeamMember(t, r.memb) {
		t.Errorf("member still present after admin removal, want removed")
	}
}

// TestTeamMemberRemove_NonManagerCantRemoveOther: a plain member removing a
// different user is 403 (not self, not a manager) and the target stays.
func TestTeamMemberRemove_NonManagerCantRemoveOther(t *testing.T) {
	r := newTeamMembersRig(t)

	rec := httptest.NewRecorder()
	r.tmh.handleTeamMemberRemove(rec, r.req(http.MethodDelete, r.memb, r.admin, nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (non-manager removing other); body=%s", rec.Code, rec.Body.String())
	}
	if !r.isTeamMember(t, r.admin) {
		t.Errorf("admin removed by non-manager, want still a member")
	}
}

// TestTeamMemberAdd_AdminAddsOrgMember: a team admin adds an existing org member
// (not yet on the team) with a role — 201, and the membership round-trips.
func TestTeamMemberAdd_AdminAddsOrgMember(t *testing.T) {
	r := newTeamMembersRig(t)

	// An org member with no team membership — the picker's source set.
	newcomer := pgtest.SeedUser(t, r.h, "newcomer")
	pgtest.MustExec(t, r.h.AdminDB,
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`, newcomer, r.orgID)

	rec := httptest.NewRecorder()
	r.tmh.handleTeamMemberAdd(rec, r.req(http.MethodPost, r.admin, "", map[string]string{
		"user_id": newcomer, "role": "member",
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if got := r.teamRoleOf(t, newcomer); got != "member" {
		t.Errorf("newcomer role = %q after add, want member", got)
	}
}

// TestTeamMemberAdd_NonOrgMemberIs422: adding a user who isn't in the org is a
// 422 (the defensive backstop behind the org-members-only picker), and no
// membership is minted.
func TestTeamMemberAdd_NonOrgMemberIs422(t *testing.T) {
	r := newTeamMembersRig(t)

	stranger := pgtest.SeedUser(t, r.h, "stranger") // no org membership

	rec := httptest.NewRecorder()
	r.tmh.handleTeamMemberAdd(rec, r.req(http.MethodPost, r.admin, "", map[string]string{
		"user_id": stranger, "role": "member",
	}))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (non-org-member); body=%s", rec.Code, rec.Body.String())
	}
	if r.isTeamMember(t, stranger) {
		t.Errorf("stranger added despite not being an org member")
	}
}

// TestTeamMemberAdd_DuplicateIs409: adding someone already on the team is a 409.
func TestTeamMemberAdd_DuplicateIs409(t *testing.T) {
	r := newTeamMembersRig(t)

	rec := httptest.NewRecorder()
	r.tmh.handleTeamMemberAdd(rec, r.req(http.MethodPost, r.admin, "", map[string]string{
		"user_id": r.memb, "role": "member",
	}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (already a member); body=%s", rec.Code, rec.Body.String())
	}
}

// TestTeamMemberAdd_NonManagerIsForbidden: a plain member can't add anyone — 403.
func TestTeamMemberAdd_NonManagerIsForbidden(t *testing.T) {
	r := newTeamMembersRig(t)

	newcomer := pgtest.SeedUser(t, r.h, "newcomer")
	pgtest.MustExec(t, r.h.AdminDB,
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`, newcomer, r.orgID)

	rec := httptest.NewRecorder()
	r.tmh.handleTeamMemberAdd(rec, r.req(http.MethodPost, r.memb, "", map[string]string{
		"user_id": newcomer, "role": "member",
	}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (non-manager add); body=%s", rec.Code, rec.Body.String())
	}
	if r.isTeamMember(t, newcomer) {
		t.Errorf("newcomer added by non-manager, want not a member")
	}
}

// TestTeamMembers_CrossOrgTeamIs404: a team_id that belongs to a DIFFERENT org
// 404s (VerifyTeamInOrg), so a caller can't read or mutate another org's team.
func TestTeamMembers_CrossOrgTeamIs404(t *testing.T) {
	r := newTeamMembersRig(t)
	// A second, unrelated org + team. The caller's active org stays r.orgID.
	_, _, otherTeam := pgtest.SeedOrgWithUser(t, r.h, "rival")

	for _, tc := range []struct {
		name   string
		method string
		fn     func(http.ResponseWriter, *http.Request)
		target string
		body   any
	}{
		{"list", http.MethodPost, r.tmh.handleTeamRosterList, "", map[string]any{}},
		{"add", http.MethodPost, r.tmh.handleTeamMemberAdd, "", map[string]string{"user_id": r.memb, "role": "member"}},
		{"role", http.MethodPatch, r.tmh.handleTeamMemberRoleChange, r.memb, map[string]string{"role": "admin"}},
		{"remove", http.MethodDelete, r.tmh.handleTeamMemberRemove, r.memb, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.fn(rec, r.reqTeam(tc.method, r.admin, otherTeam, tc.target, tc.body))
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s status = %d, want 404 (cross-org team)", tc.name, rec.Code)
			}
		})
	}
}

// TestTeamMembers_LocalMutatorsAre404: the roster WRITES are multi-only —
// every mutator 404s in local mode regardless of identity, because N=1 has
// nobody to enrol, promote or remove. The list read is deliberately absent
// from this table: it is the one roster every consumer reads, in both modes,
// and TestTeamRosterList_LocalSingleMember covers it there.
func TestTeamMembers_LocalMutatorsAre404(t *testing.T) {
	r := newTeamMembersRig(t)
	runmode.SetForTest(t, runmode.ModeLocal)

	for _, tc := range []struct {
		name   string
		method string
		fn     func(http.ResponseWriter, *http.Request)
		target string
		body   any
	}{
		{"add", http.MethodPost, r.tmh.handleTeamMemberAdd, "", map[string]string{"user_id": r.memb, "role": "member"}},
		{"role", http.MethodPatch, r.tmh.handleTeamMemberRoleChange, r.memb, map[string]string{"role": "admin"}},
		{"remove", http.MethodDelete, r.tmh.handleTeamMemberRemove, r.memb, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.fn(rec, r.req(tc.method, r.admin, tc.target, tc.body))
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s status = %d, want 404 in local mode", tc.name, rec.Code)
			}
		})
	}
}
