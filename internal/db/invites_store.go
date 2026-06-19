package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// RevokeOutcome is the result of InvitesStore.Revoke. The handler maps it
// to an HTTP status: NotFound → 404, AlreadyAccepted → 409, OK → 200.
// Revoking an already-revoked invite folds into OK (idempotent).
type RevokeOutcome int

const (
	// RevokeNotFound — no matching pending/terminal invite in the org.
	RevokeNotFound RevokeOutcome = iota
	// RevokeOK — the invite is now revoked (or was already revoked).
	RevokeOK
	// RevokeAlreadyAccepted — the invite was redeemed; can't revoke it.
	RevokeAlreadyAccepted
)

// InvitesStore owns the org_invites table — TF-owned, link-based org
// invitations. Postgres-only: local mode (N=1) never mounts the invite
// routes, so the SQLite impl is a multi-only stub that returns
// ErrNotApplicableInLocal.
//
// # Pool split (Postgres)
//
//   - Create, ListActive, Revoke run on the app pool. The
//     org_invites_{select,insert,update} RLS policies gate every operation
//     on org-admin in the current org; the request-handler caller has set
//     the JWT claims via the TxRunner.
//   - GetByTokenHashSystem and IsOrgMemberSystem run on the admin pool
//     (BYPASSRLS). The redeem actor is an outsider holding a token with no
//     membership, so RLS can't express the lookup — the token, validated
//     app-side, is the authorization. Mirrors provisionOrg's documented
//     BYPASSRLS rationale.
//
// The accept *write* (grant membership + stamp accepted) is orchestrated in
// the server handler against an admin-pool tx so it can compose the shared
// grantOrgMembership primitive with the org_invites UPDATE atomically; it
// is deliberately not a store method.
type InvitesStore interface {
	// Create inserts a pending invite and returns its id. A second *live*
	// pending invite to the same (org, lower(email)) trips the partial-unique
	// index (org_invites_active_uniq) — the handler maps that to 409. An
	// *expired* pending invite to the same address does not block: Create
	// first revokes it (same tx) so expiry behaves terminally for re-invite.
	// App pool.
	Create(ctx context.Context, p domain.CreateInviteParams) (string, error)

	// ListActive returns the org's active invites (accepted_at IS NULL AND
	// revoked_at IS NULL), newest first. Drives the pending-ghost-rows UI
	// (a follow-up ticket). App pool.
	ListActive(ctx context.Context, orgID string) ([]domain.OrgInvite, error)

	// Revoke marks a pending invite revoked in the given org and reports the
	// outcome (not-found / ok / already-accepted). Idempotent: revoking an
	// already-revoked invite returns RevokeOK. App pool.
	Revoke(ctx context.Context, orgID, inviteID string) (RevokeOutcome, error)

	// GetByTokenHashSystem looks up an invite by its token hash for the
	// redeem paths (preview + accept). Returns (nil, nil) when no row
	// matches. OrgName + InvitedByName are join-populated for the preview.
	// Admin pool (the redeem actor has no membership).
	GetByTokenHashSystem(ctx context.Context, tokenHash []byte) (*domain.OrgInvite, error)

	// IsOrgMemberSystem reports whether the user already holds an
	// org_memberships row in the org — the redeem state machine's
	// "already a member" idempotency branch. Admin pool.
	IsOrgMemberSystem(ctx context.Context, userID, orgID string) (bool, error)
}
