package server

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// Access-change audit log (TFAC-471) write helpers shared across the
// governance handlers. The bulk of the write-points record through the
// tx-bound db.AccessChangeLogStore inside their existing WithTx; this file
// holds the raw-execer path (invite-accept + SSO JIT, which run on the admin
// pool) plus the small detail_json builders the handlers reuse.

// recordAccessChangeTx writes one access_change_log row through a raw execer —
// the seam for the admin-pool grants that compose their audit row atomically
// with the membership INSERT: invite-accept (the invitee has no RLS standing to
// insert their own membership) and SSO JIT (the user has no claims/membership at
// provisioning time); a future SCIM grant grafts here too. Each runs on the admin
// pool's raw *sql.Tx, so it can't route through the app-pool store — this is how
// the audit row commits or rolls back together with the grant, and it's why these
// callers use this rather than the store's RecordSystem (which can't compose into
// a caller-supplied tx). Every other write-point uses the tx-bound
// db.AccessChangeLogStore inside its WithTx. The admin pool is BYPASSRLS, so
// org_id is set explicitly (defense in depth); id + created_at fall to their
// column DEFAULTs. Postgres-only (these surfaces are multi-mode); mirrors
// grantOrgMembership's raw style. Empty actor/target/team/detail values serialize
// to SQL NULL.
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

// accessDetailCredential builds the detail_json for a credential_set /
// credential_removed action: {"kind":...} with an optional "host" when one is
// cheaply available (omitted for host-less credentials like the Anthropic key).
func accessDetailCredential(kind, host string) string {
	return accessDetailCredentialNamed(kind, host, "")
}

// accessDetailCredentialNamed is accessDetailCredential plus a "name" — the
// human handle of the specific credential, where one exists and is not itself
// secret: a GitHub App's slug, the @login a per-user identity binds to. It's
// what lets the viewer say WHICH App was registered or torn down rather than
// just "a GitHub App", which is the whole question an admin is asking of the log.
//
// Delegates so core and the ee write-points emit one shape, not two that drift.
func accessDetailCredentialNamed(kind, host, name string) string {
	return domain.AccessDetailCredential(kind, host, name)
}

// configuredLLMCredentialKinds names the org-level LLM credentials the settings
// row says are actually stored. The refs are the record here, not the vault: the
// deletes that clear a provider are idempotent and can't report whether anything
// was there, and a credential_removed row for a provider that was never
// configured is a phantom revocation in an audit log. Used by the "system
// credentials" arm, the one path that drops BOTH providers at once.
func configuredLLMCredentialKinds(s domain.OrgSettings) []string {
	var kinds []string
	if s.AnthropicAPIKeyRef != "" {
		kinds = append(kinds, domain.CredentialKindAnthropicKey)
	}
	if s.BedrockCredentialsRef != "" {
		kinds = append(kinds, domain.CredentialKindBedrock)
	}
	return kinds
}

// recordCredentialRemovals writes one host-less credential_removed row per kind
// through the caller's tx, so the audit rows commit with the removal itself. A
// nil/empty kinds slice is a no-op.
func recordCredentialRemovals(ctx context.Context, tx db.TxStores, orgID, userID string, kinds []string) error {
	for _, kind := range kinds {
		if err := tx.AccessChangeLog.Record(ctx, orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialRemoved,
			DetailJSON:  accessDetailCredential(kind, ""),
		}); err != nil {
			return fmt.Errorf("audit credential removal: %w", err)
		}
	}
	return nil
}

// auditJiraHost canonicalizes a Jira base URL for a credential row's "host", so
// org-credential rows share a host string with the per-user jira_user rows for
// the same instance — both key off the same resolved org_settings.jira_base_url,
// whereas a connect/settings write carries whatever the admin typed (trailing
// slash, casing). Falls back to the raw value when it doesn't resolve, so a
// misconfigured URL still records something.
func auditJiraHost(rawURL string) string {
	if canon, ok := resolveJiraHost(rawURL); ok {
		return canon
	}
	return rawURL
}

// accessDetailInviteCreated builds the detail_json for an invite_created action:
// the invited address + the role it grants, so the log answers "who did we open
// the door to, and at what privilege" before the invite is ever accepted (the
// later org_member_granted row carries the invite id and closes the loop).
func accessDetailInviteCreated(inviteID, email, role string) string {
	b, _ := json.Marshal(struct {
		InviteID string `json:"invite_id"`
		Email    string `json:"email"`
		Role     string `json:"role"`
	}{InviteID: inviteID, Email: email, Role: role})
	return string(b)
}

// accessDetailInviteRevoked builds the detail_json for an invite_revoked action.
// The email is best-effort — resolved from the org's active invites in the same
// tx — so a row whose address didn't resolve still records the revocation.
func accessDetailInviteRevoked(inviteID, email string) string {
	b, _ := json.Marshal(struct {
		InviteID string `json:"invite_id"`
		Email    string `json:"email,omitempty"`
	}{InviteID: inviteID, Email: email})
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
