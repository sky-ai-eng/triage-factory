package server

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TFAC-471 handler coverage: each org-roster write-point must emit exactly one
// access_change_log row with the right action/target/detail, in the SAME tx as
// the mutation. These ride the orgMembersRig (a real Postgres-backed Server, so
// RLS + the guard triggers are live) and skip without Docker like the rest of
// the pg-backed server tests.

type auditRow struct {
	actor  sql.NullString
	action string
	target sql.NullString
	team   sql.NullString
	detail sql.NullString
}

// auditRows returns every access_change_log row for the rig's org with the
// given action, newest-first. Read on the admin pool (bypasses RLS).
func (r *orgMembersRig) auditRows(t *testing.T, action string) []auditRow {
	t.Helper()
	rows, err := r.h.AdminDB.Query(`
		SELECT actor_user_id::text, action, target_user_id::text, team_id::text, detail_json
		  FROM access_change_log
		 WHERE org_id = $1 AND action = $2
		 ORDER BY created_at DESC
	`, r.orgID, action)
	if err != nil {
		t.Fatalf("query audit rows: %v", err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var a auditRow
		if err := rows.Scan(&a.actor, &a.action, &a.target, &a.team, &a.detail); err != nil {
			t.Fatalf("scan audit row: %v", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("audit rows: %v", err)
	}
	return out
}

// TestOrgMemberRoleChange_EmitsAuditRow: a successful role change writes one
// org_role_changed row with actor=admin, target=member, detail={new_role}.
func TestOrgMemberRoleChange_EmitsAuditRow(t *testing.T) {
	r := newOrgMembersRig(t)

	rec := httptest.NewRecorder()
	r.omh.handleOrgMemberRoleChange(rec, r.req(http.MethodPatch, r.admin, r.memb, map[string]string{"role": "admin"}))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	got := r.auditRows(t, domain.AccessActionOrgRoleChanged)
	if len(got) != 1 {
		t.Fatalf("audit rows = %d, want exactly 1", len(got))
	}
	row := got[0]
	if row.actor.String != r.admin {
		t.Errorf("actor = %q, want %q (the admin)", row.actor.String, r.admin)
	}
	if row.target.String != r.memb {
		t.Errorf("target = %q, want %q (the member)", row.target.String, r.memb)
	}
	if row.detail.String != `{"new_role":"admin"}` {
		t.Errorf("detail = %q, want {\"new_role\":\"admin\"}", row.detail.String)
	}
}

// TestOrgMemberRemove_EmitsAuditRow: an admin removing a member writes one
// org_member_revoked row with actor=admin, target=member, no detail.
func TestOrgMemberRemove_EmitsAuditRow(t *testing.T) {
	r := newOrgMembersRig(t)

	rec := httptest.NewRecorder()
	r.omh.handleOrgMemberRemove(rec, r.req(http.MethodDelete, r.admin, r.memb, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	got := r.auditRows(t, domain.AccessActionOrgMemberRevoked)
	if len(got) != 1 {
		t.Fatalf("audit rows = %d, want exactly 1", len(got))
	}
	if got[0].actor.String != r.admin || got[0].target.String != r.memb {
		t.Errorf("actor/target = %q/%q, want %q/%q", got[0].actor.String, got[0].target.String, r.admin, r.memb)
	}
	if got[0].detail.Valid {
		t.Errorf("detail = %q, want NULL (a revoke carries no detail)", got[0].detail.String)
	}
}

// TestOrgOwnershipTransfer_EmitsAuditRow: a successful transfer writes one
// org_ownership_transferred row with actor=former owner, target=new owner.
func TestOrgOwnershipTransfer_EmitsAuditRow(t *testing.T) {
	r := newOrgMembersRig(t)

	rec := httptest.NewRecorder()
	r.omh.handleOrgOwnershipTransfer(rec, r.transferReq(r.owner, r.admin))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	got := r.auditRows(t, domain.AccessActionOrgOwnershipTransferred)
	if len(got) != 1 {
		t.Fatalf("audit rows = %d, want exactly 1", len(got))
	}
	if got[0].actor.String != r.owner {
		t.Errorf("actor = %q, want %q (former owner)", got[0].actor.String, r.owner)
	}
	if got[0].target.String != r.admin {
		t.Errorf("target = %q, want %q (new owner)", got[0].target.String, r.admin)
	}
}

// TestOrgMemberRoleChange_FailedActionWritesNoAuditRow: the same-tx invariant —
// demoting the sole owner trips the guard trigger (409), the whole tx rolls
// back, and NO org_role_changed row is left behind. Proves the Record shares the
// mutation's transaction rather than committing independently.
func TestOrgMemberRoleChange_FailedActionWritesNoAuditRow(t *testing.T) {
	r := newOrgMembersRig(t)

	rec := httptest.NewRecorder()
	r.omh.handleOrgMemberRoleChange(rec, r.req(http.MethodPatch, r.admin, r.owner, map[string]string{"role": "member"}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (last-owner guard); body=%s", rec.Code, rec.Body.String())
	}

	if got := r.auditRows(t, domain.AccessActionOrgRoleChanged); len(got) != 0 {
		t.Errorf("audit rows = %d after a rolled-back change, want 0", len(got))
	}
}
