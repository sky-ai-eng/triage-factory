package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/sessions"
)

// orgMembersRig is the multi-mode fixture for the org People roster handler:
// a real Postgres-backed Server (so RLS + the guard_org_owners trigger are
// live) plus a fully-wired org with three roles to act as/against — owner O,
// admin A, member M, all on one default team.
type orgMembersRig struct {
	h     *pgtest.Harness
	omh   *orgMembersHandler
	sess  *sessions.Store
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
	s := New(h.AdminDB, stores)

	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, h, "founder")
	admin := pgtest.SeedUser(t, h, "admin")
	pgtest.AddOrgMember(t, h, admin, orgID, teamID, "admin", "admin")
	memb := pgtest.SeedUser(t, h, "member")
	pgtest.AddOrgMember(t, h, memb, orgID, teamID, "member", "member")

	// A real sessions store so the TFAC-487 deprovisioning revoke can be
	// exercised end-to-end. The closure mirrors server.go's wiring: parse the
	// string ids the handler carries and call the org-scoped revoke.
	var encKey [32]byte
	if _, err := rand.Read(encKey[:]); err != nil {
		t.Fatalf("seed session key: %v", err)
	}
	sessStore := sessions.NewStore(h.AdminDB, sessions.Key(encKey))
	omh := &orgMembersHandler{tx: s.tx, az: s.az,
		revokeUserOrgSessions: func(ctx context.Context, userID, orgID string) (int64, error) {
			uid, err := uuid.Parse(userID)
			if err != nil {
				return 0, err
			}
			oid, err := uuid.Parse(orgID)
			if err != nil {
				return 0, err
			}
			return sessStore.RevokeForUserInOrgSystem(ctx, uid, oid)
		},
	}

	return &orgMembersRig{
		h:     h,
		omh:   omh,
		sess:  sessStore,
		orgID: orgID,
		owner: owner,
		admin: admin,
		memb:  memb,
	}
}

// newSession creates an active session for userID scoped to the given org and
// returns its id. orgID may be "" for a NULL active-org session.
func (r *orgMembersRig) newSession(t *testing.T, userID, orgID string) uuid.UUID {
	t.Helper()
	active := uuid.NullUUID{}
	if orgID != "" {
		active = uuid.NullUUID{UUID: uuid.MustParse(orgID), Valid: true}
	}
	s, err := r.sess.CreateSystem(context.Background(), uuid.MustParse(userID),
		"jwt", "ref", time.Now().Add(time.Hour), time.Now().Add(24*time.Hour),
		"ua", "1.2.3.4", active)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return s.ID
}

// sessionRevoked reports whether the session row's revoked_at is set.
func (r *orgMembersRig) sessionRevoked(t *testing.T, sid uuid.UUID) bool {
	t.Helper()
	var revoked bool
	if err := r.h.AdminDB.QueryRow(
		`SELECT revoked_at IS NOT NULL FROM public.sessions WHERE id = $1`, sid,
	).Scan(&revoked); err != nil {
		t.Fatalf("read revoked_at for %s: %v", sid, err)
	}
	return revoked
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

// ownerUserID reads the org's founder sentinel (orgs.owner_user_id) so the
// transfer tests can assert it moved (or didn't).
func (r *orgMembersRig) ownerUserID(t *testing.T) string {
	t.Helper()
	var id string
	if err := r.h.AdminDB.QueryRow(
		`SELECT owner_user_id::text FROM orgs WHERE id = $1`, r.orgID,
	).Scan(&id); err != nil {
		t.Fatalf("read owner_user_id: %v", err)
	}
	return id
}

// transferReq builds a POST to the transfer-ownership handler as callerID with
// the { new_owner_user_id } body. It reuses req's path-value + claims wiring;
// the cosmetic "/members" path is irrelevant since the test invokes the
// handler method directly (it reads org_id from the path value + the body, not
// a {user_id} segment).
func (r *orgMembersRig) transferReq(callerID, newOwnerID string) *http.Request {
	return r.req(http.MethodPost, callerID, "", map[string]string{"new_owner_user_id": newOwnerID})
}

// TestOrgMembersList_AnyMemberReads: a plain member (no admin role) can GET
// the full roster, and each row carries its org role + host-scoped identity
// readiness — the admin's seeded GitHub login resolves; the others read as
// "Not connected" (null).
func TestOrgMembersList_AnyMemberReads(t *testing.T) {
	r := newOrgMembersRig(t)

	// Give the admin a GitHub login under the default github.com host, and
	// leave the org's github_base_url UNSET (the common github.com-org case).
	// This exercises two things at once: (1) the roster must surface a
	// *peer's* readiness, proving the admin-pool enrichment path works past
	// the self-only identity RLS; and (2) the read must resolve the unset host
	// to github.com (EffectiveGitHubHost) — a raw normalize would key host=""
	// and miss this binding, reading "Not connected" for every github.com org.
	pgtest.MustExec(t, r.h.AdminDB,
		`INSERT INTO user_github_identities (user_id, github_base_url, login, source, verified_at)
		 VALUES ($1, 'https://github.com', 'admin-gh', 'pat', now())`, r.admin)

	rec := httptest.NewRecorder()
	r.omh.handleOrgMembersList(rec, r.req(http.MethodPost, r.memb, "", map[string]any{}))
	page := decodeList[orgMemberRow](t, rec)
	if len(page.Items) != 3 || page.Total() != 3 {
		t.Fatalf("members = %d (total %d), want 3 (owner, admin, member)", len(page.Items), page.Total())
	}
	byID := map[string]orgMemberRow{}
	for _, m := range page.Items {
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
	if byID[r.owner].IsCurrentUser {
		t.Errorf("owner row is_current_user = true, want false")
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

// TestOrgMemberRoleChange_NonAdminIsForbidden: a plain member cannot change
// roles — the org-admin gate answers 403 naming the missing role (a member can
// see their own org, so a 404 would deny something they can list), and the row
// is untouched.
func TestOrgMemberRoleChange_NonAdminIsForbidden(t *testing.T) {
	r := newOrgMembersRig(t)

	rec := httptest.NewRecorder()
	r.omh.handleOrgMemberRoleChange(rec, r.req(http.MethodPatch, r.memb, r.admin, map[string]string{"role": "member"}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (non-admin); body=%s", rec.Code, rec.Body.String())
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

// TestOrgMemberRemove_AdminRemovesMember: the happy path through the admin
// branch — an org admin removes a non-owner member (not themselves) — 204 and
// the member is gone.
func TestOrgMemberRemove_AdminRemovesMember(t *testing.T) {
	r := newOrgMembersRig(t)

	rec := httptest.NewRecorder()
	r.omh.handleOrgMemberRemove(rec, r.req(http.MethodDelete, r.admin, r.memb, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (admin removes member); body=%s", rec.Code, rec.Body.String())
	}
	if r.isMember(t, r.memb) {
		t.Errorf("member still present after admin removal, want removed")
	}
}

// TestOrgMemberRemove_RevokesOrgSession: removing a member revokes the
// member's session whose active_org_id is this org (TFAC-487 deprovisioning
// hygiene), but leaves a session of theirs active in ANOTHER org alone
// (org-scoped revoke, mirroring the WS-kick's org filter).
func TestOrgMemberRemove_RevokesOrgSession(t *testing.T) {
	r := newOrgMembersRig(t)

	// A second org (owned by the founder so the FK resolves) where the member
	// also holds a session — that session must survive the removal.
	otherOrg := pgtest.SeedOrg(t, r.h, "other-org", r.owner)
	thisOrgSid := r.newSession(t, r.memb, r.orgID)
	otherOrgSid := r.newSession(t, r.memb, otherOrg)

	rec := httptest.NewRecorder()
	r.omh.handleOrgMemberRemove(rec, r.req(http.MethodDelete, r.admin, r.memb, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if !r.sessionRevoked(t, thisOrgSid) {
		t.Errorf("member's session for this org not revoked after removal")
	}
	if r.sessionRevoked(t, otherOrgSid) {
		t.Errorf("member's other-org session revoked — org-scoped revoke bled across orgs")
	}
}

// TestOrgMemberRemove_SelfLeaveRevokesOwnSession: a self-leave revokes the
// leaver's own session scoped to this org, same path as an admin removal.
func TestOrgMemberRemove_SelfLeaveRevokesOwnSession(t *testing.T) {
	r := newOrgMembersRig(t)

	sid := r.newSession(t, r.memb, r.orgID)

	rec := httptest.NewRecorder()
	r.omh.handleOrgMemberRemove(rec, r.req(http.MethodDelete, r.memb, r.memb, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (self-leave); body=%s", rec.Code, rec.Body.String())
	}
	if !r.sessionRevoked(t, sid) {
		t.Errorf("leaver's own org session not revoked after self-leave")
	}
}

// TestOrgMemberRemove_NonAdminCantRemoveOther: a plain member removing a
// *different* user is 403 (not self, not admin) — they are a member of this
// org, so the denial names the missing role — and the target stays.
func TestOrgMemberRemove_NonAdminCantRemoveOther(t *testing.T) {
	r := newOrgMembersRig(t)

	rec := httptest.NewRecorder()
	r.omh.handleOrgMemberRemove(rec, r.req(http.MethodDelete, r.memb, r.admin, nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (non-admin removing other); body=%s", rec.Code, rec.Body.String())
	}
	if !r.isMember(t, r.admin) {
		t.Errorf("admin removed by non-admin, want still a member")
	}
}

// TestOrgMemberRemove_SoleOwnerSelfLeaveIs409: the sole owner clicking Leave
// (a self-delete) trips guard_org_owners — 409, owner still present. This is
// the dead-end TFAC-420's frontend turns into a "transfer ownership first"
// prompt instead of a bare error.
func TestOrgMemberRemove_SoleOwnerSelfLeaveIs409(t *testing.T) {
	r := newOrgMembersRig(t)

	rec := httptest.NewRecorder()
	r.omh.handleOrgMemberRemove(rec, r.req(http.MethodDelete, r.owner, r.owner, nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (sole-owner self-leave); body=%s", rec.Code, rec.Body.String())
	}
	if !r.isMember(t, r.owner) {
		t.Errorf("owner removed despite last-owner guard, want still a member")
	}
}

// TestOrgOwnershipTransfer_PromotesReassignsDemotes: the owner hands ownership
// to the admin. The endpoint promotes the target to owner, repoints the
// founder sentinel orgs.owner_user_id, and demotes the former owner to admin —
// all in one transaction.
func TestOrgOwnershipTransfer_PromotesReassignsDemotes(t *testing.T) {
	r := newOrgMembersRig(t)

	rec := httptest.NewRecorder()
	r.omh.handleOrgOwnershipTransfer(rec, r.transferReq(r.owner, r.admin))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if got := r.roleOf(t, r.admin); got != "owner" {
		t.Errorf("new owner membership role = %q, want owner", got)
	}
	if got := r.roleOf(t, r.owner); got != "admin" {
		t.Errorf("former owner membership role = %q, want admin", got)
	}
	if got := r.ownerUserID(t); got != r.admin {
		t.Errorf("orgs.owner_user_id = %q, want %q (new owner)", got, r.admin)
	}
}

// TestOrgOwnershipTransfer_NonOwnerIs403: a non-owner caller can't transfer
// ownership — whether they're an org admin or a plain member, both hit the same
// UserOwnsOrg gate, get 403, and nothing moves. (Ownership is the founder
// sentinel, not the admin role — being an org admin is not enough.)
func TestOrgOwnershipTransfer_NonOwnerIs403(t *testing.T) {
	for _, tc := range []struct {
		name   string
		caller func(*orgMembersRig) (callerID, targetID, wantRole string)
	}{
		{"admin caller", func(r *orgMembersRig) (string, string, string) { return r.admin, r.memb, "member" }},
		{"member caller", func(r *orgMembersRig) (string, string, string) { return r.memb, r.admin, "admin" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newOrgMembersRig(t)
			caller, target, wantRole := tc.caller(r)

			rec := httptest.NewRecorder()
			r.omh.handleOrgOwnershipTransfer(rec, r.transferReq(caller, target))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (non-owner caller); body=%s", rec.Code, rec.Body.String())
			}
			if got := r.ownerUserID(t); got != r.owner {
				t.Errorf("orgs.owner_user_id = %q, want %q (unchanged)", got, r.owner)
			}
			if got := r.roleOf(t, target); got != wantRole {
				t.Errorf("target role = %q, want %q (untouched)", got, wantRole)
			}
		})
	}
}

// TestOrgOwnershipTransfer_NonMemberIs422: transferring to a user who isn't in
// the org is a 422, and ownership is unchanged.
func TestOrgOwnershipTransfer_NonMemberIs422(t *testing.T) {
	r := newOrgMembersRig(t)
	outsider := pgtest.SeedUser(t, r.h, "outsider")

	rec := httptest.NewRecorder()
	r.omh.handleOrgOwnershipTransfer(rec, r.transferReq(r.owner, outsider))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (non-member target); body=%s", rec.Code, rec.Body.String())
	}
	if got := r.ownerUserID(t); got != r.owner {
		t.Errorf("orgs.owner_user_id = %q, want %q (unchanged)", got, r.owner)
	}
}

// TestOrgOwnershipTransfer_SelfIs400: transferring to yourself is rejected up
// front (it would otherwise promote-then-demote the same row into a confusing
// last-owner 409).
func TestOrgOwnershipTransfer_SelfIs400(t *testing.T) {
	r := newOrgMembersRig(t)

	rec := httptest.NewRecorder()
	r.omh.handleOrgOwnershipTransfer(rec, r.transferReq(r.owner, r.owner))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (self-transfer); body=%s", rec.Code, rec.Body.String())
	}
	if got := r.roleOf(t, r.owner); got != "owner" {
		t.Errorf("owner role = %q after self-transfer, want owner (unchanged)", got)
	}
}

// TestOrgOwnershipTransfer_ThenFormerOwnerCanLeave: the guard only blocks the
// *sole* owner. After transferring, the former owner is a plain admin and can
// leave cleanly — the UX the transfer prompt unblocks.
func TestOrgOwnershipTransfer_ThenFormerOwnerCanLeave(t *testing.T) {
	r := newOrgMembersRig(t)

	rec := httptest.NewRecorder()
	r.omh.handleOrgOwnershipTransfer(rec, r.transferReq(r.owner, r.admin))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("transfer status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	r.omh.handleOrgMemberRemove(rec, r.req(http.MethodDelete, r.owner, r.owner, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("former-owner leave status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if r.isMember(t, r.owner) {
		t.Errorf("former owner still present after leave, want removed")
	}
}

// TestOrgMembers_LocalIs404: the whole surface is multi-only — every method
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
		{"transfer", http.MethodPost, r.omh.handleOrgOwnershipTransfer, "", map[string]string{"new_owner_user_id": r.admin}},
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
