package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// invitesStore is the Postgres impl of db.InvitesStore. Holds both pools —
// see the InvitesStore interface comment for the pool-split rationale.
//
//   - app: Create, ListActive, Revoke. Request-handler reads/writes gated
//     by the org_invites_{select,insert,update} RLS policies (org-admin in
//     the current org).
//   - admin: GetByTokenHashSystem, IsOrgMemberSystem. The redeem actor is
//     a token-bearing outsider with no membership, so RLS can't express
//     the lookup; the token is the authorization.
type invitesStore struct {
	app   queryer
	admin queryer
}

func newInvitesStore(app, admin queryer) db.InvitesStore {
	return &invitesStore{app: app, admin: admin}
}

var _ db.InvitesStore = (*invitesStore)(nil)

func (s *invitesStore) Create(ctx context.Context, p domain.CreateInviteParams) (string, error) {
	// Free the active-unique slot from a stale row first. The partial-unique
	// index (org_invites_active_uniq) keys only on accepted_at/revoked_at, so
	// an *expired* pending invite still occupies the slot and would block a
	// re-invite with a unique violation until someone revoked it by hand.
	// Revoking the expired row here (same tx, same org-admin claims) makes
	// expiry behave terminally for re-invite purposes: a live pending invite
	// is untouched (and the INSERT below still 409s against it), but an
	// expired one yields. ON CONFLICT can't express "ignore only the expired
	// duplicate", so this is a pre-step rather than an upsert.
	if _, err := s.app.ExecContext(ctx, `
		UPDATE org_invites SET revoked_at = now()
		WHERE org_id = $1 AND lower(email) = lower($2)
		  AND accepted_at IS NULL AND revoked_at IS NULL
		  AND expires_at <= now()
	`, p.OrgID, p.Email); err != nil {
		return "", fmt.Errorf("revoke expired invite: %w", err)
	}

	var id string
	err := s.app.QueryRowContext(ctx, `
		INSERT INTO org_invites (org_id, email, role, target_team_id, token_hash, invited_by, expires_at)
		VALUES ($1, lower($2), $3, $4, $5, $6, $7)
		RETURNING id::text
	`,
		p.OrgID, p.Email, p.Role,
		nullableUUID(p.TargetTeamID), p.TokenHash, nullableUUID(p.InvitedBy), p.ExpiresAt,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert org_invite: %w", err)
	}
	return id, nil
}

func (s *invitesStore) ListActive(ctx context.Context, orgID string) ([]domain.OrgInvite, error) {
	rows, err := s.app.QueryContext(ctx, `
		SELECT id::text, org_id::text, email, role::text,
		       COALESCE(target_team_id::text, ''), COALESCE(invited_by::text, ''),
		       expires_at, created_at
		FROM org_invites
		WHERE org_id = $1 AND accepted_at IS NULL AND revoked_at IS NULL
		  AND expires_at > now()
		ORDER BY created_at DESC, id ASC
	`, orgID)
	if err != nil {
		return nil, fmt.Errorf("list active org_invites: %w", err)
	}
	defer rows.Close()

	out := []domain.OrgInvite{}
	for rows.Next() {
		var iv domain.OrgInvite
		if err := rows.Scan(
			&iv.ID, &iv.OrgID, &iv.Email, &iv.Role,
			&iv.TargetTeamID, &iv.InvitedBy, &iv.ExpiresAt, &iv.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan org_invite: %w", err)
		}
		out = append(out, iv)
	}
	return out, rows.Err()
}

func (s *invitesStore) Revoke(ctx context.Context, orgID, inviteID string) (db.RevokeOutcome, error) {
	// Single statement: stamp revoked_at on a not-yet-accepted invite and
	// report enough state in RETURNING to disambiguate the outcomes. The
	// WHERE accepted_at IS NULL guard makes "revoke an accepted invite" a
	// no-op (zero rows); we then SELECT to tell not-found from
	// already-accepted. Re-revoking an already-revoked row matches and is a
	// harmless idempotent re-stamp (RevokeOK).
	res, err := s.app.ExecContext(ctx, `
		UPDATE org_invites SET revoked_at = now()
		WHERE id = $1 AND org_id = $2 AND accepted_at IS NULL
	`, inviteID, orgID)
	if err != nil {
		return db.RevokeNotFound, fmt.Errorf("revoke org_invite: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return db.RevokeNotFound, fmt.Errorf("revoke org_invite rows: %w", err)
	}
	if n > 0 {
		return db.RevokeOK, nil
	}
	// Zero rows updated: either no such invite in the org, or it's accepted.
	var accepted bool
	err = s.app.QueryRowContext(ctx, `
		SELECT accepted_at IS NOT NULL FROM org_invites WHERE id = $1 AND org_id = $2
	`, inviteID, orgID).Scan(&accepted)
	if errors.Is(err, sql.ErrNoRows) {
		return db.RevokeNotFound, nil
	}
	if err != nil {
		return db.RevokeNotFound, fmt.Errorf("revoke org_invite state: %w", err)
	}
	if accepted {
		return db.RevokeAlreadyAccepted, nil
	}
	// Row exists, not accepted, yet the UPDATE matched nothing — shouldn't
	// happen (the only other state is already-revoked, which the UPDATE would
	// have re-stamped). Treat as OK.
	return db.RevokeOK, nil
}

func (s *invitesStore) GetByTokenHashSystem(ctx context.Context, tokenHash []byte) (*domain.OrgInvite, error) {
	var iv domain.OrgInvite
	err := s.admin.QueryRowContext(ctx, `
		SELECT i.id::text, i.org_id::text, i.email, i.role::text,
		       COALESCE(i.target_team_id::text, ''), COALESCE(i.invited_by::text, ''),
		       i.expires_at, i.accepted_at, COALESCE(i.accepted_by::text, ''),
		       i.revoked_at, i.created_at,
		       o.name, COALESCE(u.display_name, '')
		FROM org_invites i
		JOIN orgs o ON o.id = i.org_id
		LEFT JOIN users u ON u.id = i.invited_by
		WHERE i.token_hash = $1
	`, tokenHash).Scan(
		&iv.ID, &iv.OrgID, &iv.Email, &iv.Role,
		&iv.TargetTeamID, &iv.InvitedBy,
		&iv.ExpiresAt, &iv.AcceptedAt, &iv.AcceptedBy,
		&iv.RevokedAt, &iv.CreatedAt,
		&iv.OrgName, &iv.InvitedByName,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get org_invite by token hash: %w", err)
	}
	return &iv, nil
}

func (s *invitesStore) IsOrgMemberSystem(ctx context.Context, userID, orgID string) (bool, error) {
	var exists bool
	err := s.admin.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM org_memberships WHERE user_id = $1 AND org_id = $2
		)
	`, userID, orgID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check org membership: %w", err)
	}
	return exists, nil
}
