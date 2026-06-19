package domain

import "time"

// OrgInvite is one row of the org_invites table — a TF-owned, link-based
// invitation for a human to join an org. An org admin creates one (email +
// role + optional target team) and hands the recipient a one-time
// accept_url carrying the raw token; redeeming the token mints the
// invitee's org_memberships row. The raw token is never modeled here — only
// its sha256 lives in the table, and that hash never leaves the store.
//
// Pointer timestamps (AcceptedAt / RevokedAt) are nil when the column is
// NULL — the redeem state machine branches on which of them is set.
type OrgInvite struct {
	ID           string
	OrgID        string
	Email        string
	Role         string // org_role: 'admin' | 'member' (never 'owner')
	TargetTeamID string // empty when NULL (org-only invite, zero teams)
	InvitedBy    string // empty when NULL; audit only
	ExpiresAt    time.Time
	AcceptedAt   *time.Time
	AcceptedBy   string // empty when NULL; who redeemed (audit)
	RevokedAt    *time.Time
	CreatedAt    time.Time

	// OrgName and InvitedByName are join-populated read-only display fields,
	// set only by the redeem-path read (GetByTokenHashSystem) that backs the
	// unauthenticated preview. They are empty on the admin-facing list/CRUD
	// paths, which never join orgs/users.
	OrgName       string
	InvitedByName string
}

// CreateInviteParams is the input to InvitesStore.Create. Email is already
// lower-cased + trimmed by the handler; TargetTeamID empty means org-only
// (no team membership written at accept time). TokenHash is sha256(raw).
type CreateInviteParams struct {
	OrgID        string
	Email        string
	Role         string
	TargetTeamID string // empty = org-only
	TokenHash    []byte
	InvitedBy    string // empty = unknown/unset
	ExpiresAt    time.Time
}
