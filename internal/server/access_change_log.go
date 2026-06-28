package server

import (
	"context"
	"encoding/json"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// Access-change audit log (TFAC-471) write helpers shared across the
// governance handlers. The bulk of the write-points record through the
// tx-bound db.AccessChangeLogStore inside their existing WithTx; this file
// holds the one raw-execer path (invite-accept, which runs on the admin pool)
// plus the small detail_json builders the handlers reuse.

// recordAccessChangeTx writes one access_change_log row through a raw execer —
// the seam for the invite-accept grant, which runs on the admin pool's raw
// *sql.Tx because the invitee has no RLS standing to insert their own
// membership and therefore can't route through the app-pool store. Every other
// write-point uses the tx-bound db.AccessChangeLogStore inside its WithTx. The
// admin pool is BYPASSRLS, so org_id is set explicitly (defense in depth);
// id + created_at fall to their column DEFAULTs. Postgres-only (the invite
// surface is multi-mode); mirrors grantOrgMembership's raw style. Empty
// actor/target/team/detail values serialize to SQL NULL.
func recordAccessChangeTx(ctx context.Context, ex execer, orgID string, e domain.AccessChange) error {
	_, err := ex.ExecContext(ctx, `
		INSERT INTO public.access_change_log
			(org_id, actor_user_id, action, target_user_id, team_id, detail_json)
		VALUES ($1, NULLIF($2, '')::uuid, $3, NULLIF($4, '')::uuid, NULLIF($5, '')::uuid, NULLIF($6, ''))
	`, orgID, e.ActorUserID, e.Action, e.TargetUserID, e.TeamID, e.DetailJSON)
	return err
}

// accessDetailRoleChange builds the detail_json for a role-change action,
// recording the old→new transition (the store's UpdateRole / ChangeMemberRole
// return the prior role from the same UPDATE). The from→to delta is what makes
// a privilege change auditable — promote vs. demote.
func accessDetailRoleChange(oldRole, newRole string) string {
	b, _ := json.Marshal(struct {
		OldRole string `json:"old_role"`
		NewRole string `json:"new_role"`
	}{OldRole: oldRole, NewRole: newRole})
	return string(b)
}

// accessDetailAddedRole builds the detail_json for a member-ADD action, which
// has no prior role — only the role the member was added at. Named for the add
// case so it isn't mistaken for the role-change builder: role *changes* use
// accessDetailRoleChange, which also carries old_role.
func accessDetailAddedRole(role string) string {
	b, _ := json.Marshal(struct {
		NewRole string `json:"new_role"`
	}{NewRole: role})
	return string(b)
}

// accessDetailCredential builds the detail_json for a credential_set action:
// {"kind":...} with an optional "host" when one is cheaply available (omitted
// for host-less credentials like the Anthropic key).
func accessDetailCredential(kind, host string) string {
	b, _ := json.Marshal(struct {
		Kind string `json:"kind"`
		Host string `json:"host,omitempty"`
	}{Kind: kind, Host: host})
	return string(b)
}

// accessDetailInvite builds the detail_json for an invite-accept grant,
// carrying the invite id (the actor is the invitee themselves).
func accessDetailInvite(inviteID string) string {
	b, _ := json.Marshal(struct {
		InviteID string `json:"invite_id"`
	}{InviteID: inviteID})
	return string(b)
}

// accessDetailSSOJIT builds the detail_json for an SSO JIT auto-provisioned org
// grant: {"source":"sso_jit","role":...}. The source distinguishes a
// policy-driven SSO join from an interactive invite-accept in the audit viewer
// (the label renders "joined via SSO"); role is the connection binding's default
// role the member was granted at. SCIM (future) records the same shape with its
// own source. See TFAC-486.
func accessDetailSSOJIT(role string) string {
	b, _ := json.Marshal(struct {
		Source string `json:"source"`
		Role   string `json:"role"`
	}{Source: domain.AccessSourceSSOJIT, Role: role})
	return string(b)
}
