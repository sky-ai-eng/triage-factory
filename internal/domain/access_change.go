package domain

import "time"

// AccessChange is one row in access_change_log — a governance action that has
// no external entity: org/team membership & role grants/changes/revokes, and
// credential bind/rotate (GitHub PAT, Jira org + per-user, Anthropic key).
// Append-only, low-volume, org-scoped. The capture layer for the future
// org-governance / team-activity audit view (TFAC-449 bucket C/D); the read UI
// itself is out of scope here.
//
// Each row is written in the SAME transaction as the action it records, so the
// log can't diverge from reality — a log-write failure rolls the action back.
//
// Actor is the request's authenticated user (ClaimsFrom(ctx).Subject); empty
// serializes to SQL NULL for the rare system/bootstrap write with no actor. On
// invite-accept the actor is the invitee, with the invite id carried in
// DetailJSON. Target/Team/DetailJSON are likewise optional (empty → NULL).
// OrgID + ID + CreatedAt are populated on read; Record takes orgID as a
// separate argument and lets the column DEFAULTs stamp id + created_at. See
// TFAC-471.
type AccessChange struct {
	ID    string `json:"id"`
	OrgID string `json:"org_id"`
	// ActorUserID is who performed the action (claims subject). Empty for a
	// system/bootstrap write with no human actor → SQL NULL.
	ActorUserID string `json:"actor_user_id,omitempty"`
	// Action is the discriminator — one of the Access* constants below. Free
	// text (extensible — no CHECK constraint on the column).
	Action string `json:"action"`
	// TargetUserID is the subject of a membership/role action (the member
	// granted/changed/revoked). Empty for credential actions → SQL NULL.
	TargetUserID string `json:"target_user_id,omitempty"`
	// TeamID is set for team-scoped actions (and for an org grant that also
	// places the member on a team). Empty → SQL NULL.
	TeamID string `json:"team_id,omitempty"`
	// DetailJSON carries the action-specific payload, e.g.
	// {"old_role":"member","new_role":"admin"} for a role change,
	// {"kind":"github_pat","host":"..."} for a credential set,
	// {"invite_id":"..."} for an invite-accept grant. Empty → SQL NULL.
	DetailJSON string    `json:"detail_json,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// Access-change action discriminators (text, extensible — no CHECK constraint).
// See TFAC-471 §2.
const (
	AccessActionOrgMemberGranted        = "org_member_granted"
	AccessActionOrgRoleChanged          = "org_role_changed"
	AccessActionOrgMemberRevoked        = "org_member_revoked"
	AccessActionOrgOwnershipTransferred = "org_ownership_transferred"
	AccessActionTeamMemberAdded         = "team_member_added"
	AccessActionTeamRoleChanged         = "team_role_changed"
	AccessActionTeamMemberRemoved       = "team_member_removed"
	AccessActionCredentialSet           = "credential_set"
)

// Credential kinds carried in a credential_set action's DetailJSON {"kind":...}.
// Bind-vs-rotate isn't reliably distinguishable at the write-point, so every
// credential write records credential_set and lets the detail carry the kind
// (and host where cheap). See TFAC-471 §2.
const (
	CredentialKindGitHubPAT    = "github_pat"
	CredentialKindJiraOrg      = "jira_org"
	CredentialKindJiraUser     = "jira_user"
	CredentialKindAnthropicKey = "anthropic_key"
)

// AccessChangeListOpts bounds a ListByOrg read for the audit viewer (TFAC-484).
// Rows come back newest-first (matching the (org_id, created_at DESC) index).
type AccessChangeListOpts struct {
	// Limit is the page size. A value ≤ 0 falls back to the store impls' own
	// internal default (100) — but the HTTP viewer resolves a concrete page size
	// before it calls, so that 100 fallback only applies to a direct ListByOrg
	// caller. The viewer requests Limit+1 and peeks at the extra row to learn
	// whether a next page exists, without a separate COUNT.
	Limit int
	// Offset skips the first N rows for pagination. 0 (or negative, clamped by
	// the caller) is the first page.
	Offset int
	// Category, when one of AccessCategory*, narrows the read to that bucket's
	// action discriminators (via AccessActionsInCategory). An empty or
	// unrecognized value means no category filter — every action.
	Category string
}

// Access-change filter categories for the audit viewer (TFAC-484). The viewer
// groups the action discriminators into two buckets so an org admin can narrow
// the log to membership/role/ownership changes vs credential binds/rotations.
const (
	AccessCategoryMembership = "membership"
	AccessCategoryCredential = "credential"
)

// AccessActionsInCategory returns the action discriminators that make up a
// filter category, or nil for "" / an unrecognized category (the caller treats
// nil as "no filter — every action"). Membership covers every org/team
// grant / role-change / revoke / ownership-transfer; credential is the lone
// credential_set. Keeping the grouping here, next to the action constants, means
// a newly-added action is classified in exactly one place.
func AccessActionsInCategory(category string) []string {
	switch category {
	case AccessCategoryMembership:
		return []string{
			AccessActionOrgMemberGranted,
			AccessActionOrgRoleChanged,
			AccessActionOrgMemberRevoked,
			AccessActionOrgOwnershipTransferred,
			AccessActionTeamMemberAdded,
			AccessActionTeamRoleChanged,
			AccessActionTeamMemberRemoved,
		}
	case AccessCategoryCredential:
		return []string{AccessActionCredentialSet}
	default:
		return nil
	}
}
