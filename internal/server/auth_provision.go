package server

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"
)

// Signup no longer provisions a tenant.
//
// Org creation is a deliberate user action in both deployment modes
// (the SKY-435 parity contract): the multi-mode onboarding entry's
// "Start your Factory" CTA → the create-org flow, and local mode's
// "Start your Triage Factory" action. The OAuth callback therefore
// never mints an org on first login — a fresh user lands with zero
// memberships and the frontend routes them to the onboarding screen.
//
// This retires the earlier three-way join policy (personal-org-on-signup
// / auto-join-default / invite-only) and its silent provisioning
// helpers. Whether the onboarding screen's create affordance is enabled
// is governed by a single deployment flag, runmode.OrgCreationEnabled()
// (TF_PREVENT_ORG_CREATION); it does not change what happens at signup.
//
// All the callback still needs from this file is which org a returning
// user's session should default to — their earliest existing
// membership, or none.

// membershipQueryer is the slice of *sql.DB / *sql.Tx that
// lookupEarliestMembership needs. Kept as an interface so the helper can
// run against either the connection pool or a transaction.
type membershipQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// lookupEarliestMembership returns the user's earliest org membership
// (created_at ASC, org_id ASC as deterministic tiebreak), or
// uuid.NullUUID{Valid: false} if the user has zero memberships.
// sql.ErrNoRows is folded into the Valid=false return so callers
// only branch on Valid.
//
// The OAuth callback uses this to default a returning user's session to
// their earliest org. A genuinely-first-signup user has no memberships
// (signup provisions nothing), so this returns invalid and the session
// is created with a NULL active_org_id — the zero-membership state the
// onboarding entry handles.
func (s *Server) lookupEarliestMembership(ctx context.Context, q membershipQueryer, userID uuid.UUID) (uuid.NullUUID, error) {
	var existing uuid.NullUUID
	err := q.QueryRowContext(ctx, `
		SELECT org_id
		  FROM public.org_memberships
		 WHERE user_id = $1
		 ORDER BY created_at ASC, org_id ASC
		 LIMIT 1
	`, userID).Scan(&existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return uuid.NullUUID{}, err
	}
	return existing, nil
}
