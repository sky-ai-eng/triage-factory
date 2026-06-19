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

// orgMembersRig is the multi-mode fixture for the org People roster handler:
// a real Postgres-backed Server (so RLS + the guard_org_owners trigger are
// live) plus a fully-wired org with three roles to act as/against — owner O,
// admin A, member M, all on one default team.
type orgMembersRig struct {
	h     *pgtest.Harness
	omh   *orgMembersHandler
	orgID string
	owner string
	admin string
	memb  string
}

func newOrgMembersRig(t *testing.T) *orgMembersRig {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeMulti)

	h := pgtest.Shared(t)
	h.Reset(t)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	s := New(h.AdminDB, stores, 3000)

	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, h, "founder")
	admin := pgtest.SeedUser(t, h, "admin")
	pgtest.AddOrgMember(t, h, admin, orgID, teamID, "admin", "admin")
	memb := pgtest.SeedUser(t, h, "member")
	pgtest.AddOrgMember(t, h, memb, orgID, teamID, "member", "member")

	return &orgMembersRig{
		h:     h,
		omh:   &orgMembersHandler{tx: s.tx, az: s.az},
		orgID: orgID,
		owner: owner,
		admin: admin,
		memb:  memb,
	}
}

// req builds a request to the roster handler as callerID, with the org_id (and
// optional user_id) path values set and the caller's claims injected — the
// state withSession/withOrg would normally seed. We invoke the handler method
// directly (not through the mux) so the test doesn't need the full cookie-
// session dance just to exercise authz + RLS.
func (r *orgMembersRig) req(method, callerID, targetID string, body any) *http.Request {
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	path := "/api/orgs/" + r.orgID + "/members"
	if targetID != "" {
		path += "/" + targetID
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("org_id", r.orgID)
	if targetID != "" {
		req.SetPathValue("user_id", targetID)
	}
	ctx := httpx.WithClaims(req.Context(), &verify.Claims{Subject: callerID})
	return req.WithContext(ctx)
}

func (r *orgMembersRig) roleOf(t *testing.T, userID string) string {
	t.Helper()
	var role string
	if err := r.h.AdminDB.QueryRow(
		`SELECT role::text FROM org_memberships WHERE org_id = $1 AND user_id = $2`,
		r.orgID, userID,
	).Scan(&role); err != nil {
		t.Fatalf("read role for %s: %v", userID, err)
	}
	return role
}

func (r *orgMembersRig) isMember(t *testing.T, userID string) bool {
	t.Helper()
	var n int
	if err := r.h.AdminDB.QueryRow(
		`SELECT COUNT(*) FROM org_memberships WHERE org_id = $1 AND user_id = $2`,
		r.orgID, userID,
	).Scan(&n); err != nil {
		t.Fatalf("count membership for %s: %v", userID, err)
	}
	return n > 0
}

// TestOrgMembersList_AnyMemberReads: a plain member (no admin role) can GET
// the full roster, and each row carries its org role + host-scoped identity
// readiness — the admin's seeded GitHub login resolves; the others read as
// "Not connected" (null).
func TestOrgMembersList_AnyMemberReads(t *testing.T) {
	r := newOrgMembersRig(t)

	// Pin the org's GitHub host + give the admin a login on it. The roster
	// must surface a *peer's* readiness (the per-user identity RLS is
	// self-only, so this proves the admin-pool enrichment path works).
	pgtest.MustExec(t, r.h.AdminDB,
		`INSERT INTO org_settings (org_id, github_base_url) VALUES ($1, 'https://github.com')`, r.orgID)
	pgtest.MustExec(t, r.h.AdminDB,
		`INSERT INTO user_github_identities (user_id, github_base_url, login, source, verified_at)
		 VALUES ($1, 'https://github.com', 'admin-gh', 'pat', now())`, r.admin)

	rec := httptest.NewRecorder()
	r.omh.handleOrgMembersList(rec, r.req(http.MethodGet, r.memb, "", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var resp orgMembersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Members) != 3 {
		t.Fatalf("members = %d, want 3 (owner, admin, member)", len(resp.Members))
	}
	byID := map[string]orgMemberRow{}
	for _, m := range resp.Members {
		byID[m.UserID] = m
	}
	if got := byID[r.owner].Role; got != "owner" {
		t.Errorf("owner role = %q, want owner", got)
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
	// Identity readiness: the admin's seeded login resolves; a member with no
	// binding reads as null ("Not connected").
	if byID[r.admin].GitHubUsername == nil || *byID[r.admin].GitHubUsername != "admin-gh" {
		t.Errorf("admin github_username = %v, want admin-gh", byID[r.admin].GitHubUsername)
	}
	if byID[r.memb].GitHubUsername != nil {
		t.Errorf("member github_username = %v, want nil (not connected)", *byID[r.memb].GitHubUsername)
	}
	if byID[r.admin].JiraAccountID != nil {
		t.Errorf("admin jira_account_id = %v, want nil (not connected)", *byID[r.admin].JiraAccountID)
	}
}

// TestOrgMemberRoleChange_NonAdminIs404: a plain member cannot change roles —
// the org-admin gate answers 404 (not 403), and the row is untouched.
func TestOrgMemberRoleChange_NonAdminIs404(t *testing.T) {
	r := newOrgMembersRig(t)

	rec := httptest.NewRecorder()
	r.omh.handleOrgMemberRoleChange(rec, r.req(http.MethodPatch, r.memb, r.admin, map[string]string{"role": "member"}))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (non-admin); body=%s", rec.Code, rec.Body.String())
	}
	if got := r.roleOf(t, r.admin); got != "admin" {
		t.Errorf("admin role = %q after blocked change, want admin (unchanged)", got)
	}
}

// TestOrgMemberRoleChange_AdminPromotes: an org admin promotes a member to
// admin — 204 and the role flips.
func TestOrgMemberRoleChange_AdminPromotes(t *testing.T) {
	r := newOrgMembersRig(t)

	rec := httptest.NewRecorder()
	r.omh.handleOrgMemberRoleChange(rec, r.req(http.MethodPatch, r.admin, r.memb, map[string]string{"role": "admin"}))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if got := r.roleOf(t, r.memb); got != "admin" {
		t.Errorf("member role = %q after promote, want admin", got)
	}
}

// TestOrgMemberRoleChange_InvalidRoleIs400: an unknown role value is rejected
// before it reaches the column.
func TestOrgMemberRoleChange_InvalidRoleIs400(t *testing.T) {
	r := newOrgMembersRig(t)

	rec := httptest.NewRecorder()
	r.omh.handleOrgMemberRoleChange(rec, r.req(http.MethodPatch, r.admin, r.memb, map[string]string{"role": "superuser"}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (invalid role); body=%s", rec.Code, rec.Body.String())
	}
}

// TestOrgMemberRoleChange_LastOwnerIs409: demoting the org's sole owner trips
// the guard_org_owners trigger; the handler surfaces a friendly 409, not a
// 500, and the owner keeps their role.
func TestOrgMemberRoleChange_LastOwnerIs409(t *testing.T) {
	r := newOrgMembersRig(t)

	rec := httptest.NewRecorder()
	r.omh.handleOrgMemberRoleChange(rec, r.req(http.MethodPatch, r.admin, r.owner, map[string]string{"role": "member"}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (last owner); body=%s", rec.Code, rec.Body.String())
	}
	if got := r.roleOf(t, r.owner); got != "owner" {
		t.Errorf("owner role = %q after blocked demote, want owner (unchanged)", got)
	}
}

// TestOrgMemberRemove_LastOwnerIs409: removing the sole owner trips the same
// guard — 409, owner still present.
func TestOrgMemberRemove_LastOwnerIs409(t *testing.T) {
	r := newOrgMembersRig(t)

	rec := httptest.NewRecorder()
	r.omh.handleOrgMemberRemove(rec, r.req(http.MethodDelete, r.admin, r.owner, nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (last owner); body=%s", rec.Code, rec.Body.String())
	}
	if !r.isMember(t, r.owner) {
		t.Errorf("owner removed despite last-owner guard, want still a member")
	}
}

// TestOrgMemberRemove_SelfLeaveNonOwner: a non-owner may remove themselves
// without admin — 204, and they're gone.
func TestOrgMemberRemove_SelfLeaveNonOwner(t *testing.T) {
	r := newOrgMembersRig(t)

	rec := httptest.NewRecorder()
	r.omh.handleOrgMemberRemove(rec, r.req(http.MethodDelete, r.memb, r.memb, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (self-leave); body=%s", rec.Code, rec.Body.String())
	}
	if r.isMember(t, r.memb) {
		t.Errorf("member still present after self-leave, want removed")
	}
}

// TestOrgMemberRemove_NonAdminCantRemoveOther: a plain member removing a
// *different* user is 404 (not self, not admin) and the target stays.
func TestOrgMemberRemove_NonAdminCantRemoveOther(t *testing.T) {
	r := newOrgMembersRig(t)

	rec := httptest.NewRecorder()
	r.omh.handleOrgMemberRemove(rec, r.req(http.MethodDelete, r.memb, r.admin, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (non-admin removing other); body=%s", rec.Code, rec.Body.String())
	}
	if !r.isMember(t, r.admin) {
		t.Errorf("admin removed by non-admin, want still a member")
	}
}

// TestOrgMembers_LocalIs404: the whole surface is hosted-only — every method
// 404s in local mode regardless of identity.
func TestOrgMembers_LocalIs404(t *testing.T) {
	r := newOrgMembersRig(t)
	runmode.SetForTest(t, runmode.ModeLocal)

	for _, tc := range []struct {
		name   string
		method string
		fn     func(http.ResponseWriter, *http.Request)
		target string
		body   any
	}{
		{"list", http.MethodGet, r.omh.handleOrgMembersList, "", nil},
		{"role", http.MethodPatch, r.omh.handleOrgMemberRoleChange, r.memb, map[string]string{"role": "admin"}},
		{"remove", http.MethodDelete, r.omh.handleOrgMemberRemove, r.memb, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.fn(rec, r.req(tc.method, r.owner, tc.target, tc.body))
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s status = %d, want 404 in local mode", tc.name, rec.Code)
			}
		})
	}
}
