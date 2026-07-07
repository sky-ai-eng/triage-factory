package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"

	"github.com/google/uuid"
)

// Signup no longer provisions a tenant.
//
// Org creation is a deliberate user action in both deployment modes
// (the parity contract): the multi-mode onboarding entry's
// "Start your Factory" CTA → the create-org flow, and local mode's
// "Start your factory" action. The OAuth callback therefore
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

// execer is the minimal write surface shared by *sql.Tx and *sql.DB so
// grantOrgMembership can run against whichever pool the caller controls —
// provisionOrg passes its own tx; invite-accept passes the admin *sql.DB
// (or an admin-pool tx). Mirrors database/sql's ExecContext signature.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// grantOrgMembership inserts the org_memberships row and, when teamID is
// valid, the team memberships row. Runs against the passed execer (a
// *sql.Tx or the admin *sql.DB) so callers control the pool — provisionOrg
// uses its own tx; invite-accept uses the admin pool (BYPASSRLS) because
// the invitee has no standing to insert their own membership under RLS.
// ON CONFLICT DO NOTHING keeps it idempotent (re-redeeming a token, or a
// JIT/SCIM re-grant later, is a no-op).
//
// LIMITATION — this GRANTS membership, it does not UPGRADE it. If the user is
// already an org member, ON CONFLICT DO NOTHING swallows the row, so the
// orgRole passed here is NOT applied to the existing membership. An existing
// 'member' who accepts an 'admin' invite stays a 'member' (accept still
// returns 200 — "already a member"). Role *changes* on an existing membership
// are a distinct operation (the org People-roster role-change path, TFAC-414's
// roster sub-issue) and deliberately out of scope for the invite/JIT grant.
// The invite flow's job is onboarding net-new members, not re-roling existing
// ones.
//
// This is the seam SSO/JIT and SCIM graft onto later (TFAC-414): the one
// code path that turns "this user belongs to this org (and maybe this
// team), with this role" into rows.
//
// teamID NULL → only the org_memberships row is written: the deliberate
// "org member, zero teams" state an org-only invite produces.
//
// Returns orgRowInserted — whether the org_memberships INSERT actually landed a
// NET-NEW row (RowsAffected > 0) vs. an ON CONFLICT DO NOTHING no-op on an
// existing member. The JIT seam (which fires on every SSO login) gates its audit
// write on this so a returning member isn't logged as "joined" on each login;
// provisionOrg + invite-accept ignore it (they grant net-new members by
// construction). It tracks ONLY the org row, not the team row.
func grantOrgMembership(ctx context.Context, ex execer,
	userID, orgID uuid.UUID, orgRole string,
	teamID uuid.NullUUID, teamRole string) (orgRowInserted bool, err error) {
	res, err := ex.ExecContext(ctx, `
		INSERT INTO public.org_memberships (user_id, org_id, role)
		VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING
	`, userID, orgID, orgRole)
	if err != nil {
		return false, fmt.Errorf("insert org_memberships: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("org_memberships rows affected: %w", err)
	}
	orgRowInserted = n > 0
	if teamID.Valid {
		if _, err := ex.ExecContext(ctx, `
			INSERT INTO public.memberships (user_id, team_id, role)
			VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING
		`, userID, teamID.UUID, teamRole); err != nil {
			return orgRowInserted, fmt.Errorf("insert memberships: %w", err)
		}
	}
	return orgRowInserted, nil
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

// provisionOrg creates a net-new org for the founder with default
// settings: the org row, its Default team, the founder's owner/admin
// memberships, and the org_settings + team_settings rows — all in one
// transaction. Returns the new org id, its Default team id, and the
// slug actually allocated.
//
// LOAD-BEARING: this runs on the ADMIN pool (s.db, BYPASSRLS), NOT on
// the app pool (tf_app + RLS). The founder has no membership at the
// moment of creation, so RLS would reject every insert here; the admin
// pool bypasses it and the owner_user_id FK is set explicitly to keep
// the ownership invariant intact. The caller owns nothing inside the
// tx — it's fully self-contained — but the shipped-defaults bootstrap
// (BootstrapNewOrg) MUST run after this commits, outside the tx: its
// seeders route through the admin pool and refuse to run inside a
// WithTx.
func (s *Server) provisionOrg(ctx context.Context, userID uuid.UUID, name, slugBase string) (orgID, teamID uuid.UUID, slug string, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return uuid.Nil, uuid.Nil, "", fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Per-user advisory lock serializes concurrent create calls for the
	// same founder (double-submit safety). Released on commit/rollback.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, userLockKey(userID)); err != nil {
		return uuid.Nil, uuid.Nil, "", fmt.Errorf("acquire advisory lock: %w", err)
	}

	// Race-safe slug allocation. `ON CONFLICT (slug) DO NOTHING
	// RETURNING id` lets the loser of a cross-user race cleanly observe
	// zero rows and advance to the next candidate (slugBase-2, -3, …)
	// instead of erroring out on the unique violation a plain INSERT would
	// raise. Note this avoids the *error*, not the *wait*: if a concurrent
	// tx holds an uncommitted row on the same slug, Postgres still blocks
	// this INSERT until that tx commits or rolls back before deciding
	// whether the conflict stands — a tail-latency cost under contention,
	// not a failure. owner_user_id is set explicitly — the admin pool is
	// BYPASSRLS, so the orgs INSERT's RLS WITH CHECK isn't what guards
	// ownership here.
	for i := 0; i < 64; i++ {
		candidate := slugBase
		if i > 0 {
			candidate = fmt.Sprintf("%s-%d", slugBase, i+1)
		}
		var got uuid.NullUUID
		serr := tx.QueryRowContext(ctx, `
			INSERT INTO public.orgs (slug, name, owner_user_id)
			VALUES ($1, $2, $3)
			ON CONFLICT (slug) DO NOTHING
			RETURNING id
		`, candidate, name, userID).Scan(&got)
		if errors.Is(serr, sql.ErrNoRows) {
			continue
		}
		if serr != nil {
			return uuid.Nil, uuid.Nil, "", fmt.Errorf("insert orgs: %w", serr)
		}
		if !got.Valid {
			continue
		}
		orgID = got.UUID
		slug = candidate
		break
	}
	if orgID == uuid.Nil {
		return uuid.Nil, uuid.Nil, "", fmt.Errorf("could not allocate slug starting from %q after 64 tries", slugBase)
	}

	if err := tx.QueryRowContext(ctx, `
		INSERT INTO public.teams (org_id, slug, name, created_by_user_id)
		VALUES ($1, 'default', 'Default', $2)
		RETURNING id
	`, orgID, userID).Scan(&teamID); err != nil {
		return uuid.Nil, uuid.Nil, "", fmt.Errorf("insert teams: %w", err)
	}

	// Founder gets org-level 'owner' + team-level 'admin' on the Default
	// team, through the shared primitive invite-accept (and later JIT/SCIM)
	// also use. The org + team were just created in this tx, so the
	// ON CONFLICT DO NOTHING inside never trips here — behaviour unchanged.
	//
	// TFAC-471: the founder self-grant is deliberately NOT written to
	// access_change_log. The first owner is self-evident — orgs.owner_user_id
	// and this org_memberships row already record it durably, and there is no
	// third-party actor whose action needs auditing (the founder grants
	// themselves). The audit log captures governance changes *after* the org
	// exists (role changes, revokes, transfers, later grants via invite).
	if _, err := grantOrgMembership(ctx, tx, userID, orgID, "owner",
		uuid.NullUUID{UUID: teamID, Valid: true}, "admin"); err != nil {
		return uuid.Nil, uuid.Nil, "", err
	}

	if err := seedSettingsRows(ctx, tx, orgID, teamID); err != nil {
		return uuid.Nil, uuid.Nil, "", err
	}

	if err := tx.Commit(); err != nil {
		return uuid.Nil, uuid.Nil, "", fmt.Errorf("commit: %w", err)
	}

	orgsLog.Info("provisioned org", "org", orgID, "team", teamID, "owner", userID, "slug", slug)
	return orgID, teamID, slug, nil
}

// seedSettingsRows inserts the org_settings + team_settings rows every
// org+team needs at provisioning time. Both rows take their values
// entirely from the schema DEFAULT clauses — listing only the
// foreign-key column lets the NOT NULL DEFAULTs (poll intervals, clone
// protocol, model tier, auto_delegate_enabled=false) populate the rest.
// ON CONFLICT DO NOTHING so a backfill/re-run can't clobber an admin's
// edits.
func seedSettingsRows(ctx context.Context, tx *sql.Tx, orgID, teamID uuid.UUID) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO public.org_settings (org_id) VALUES ($1)
		ON CONFLICT (org_id) DO NOTHING
	`, orgID); err != nil {
		return fmt.Errorf("insert org_settings: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO public.team_settings (team_id) VALUES ($1)
		ON CONFLICT (team_id) DO NOTHING
	`, teamID); err != nil {
		return fmt.Errorf("insert team_settings: %w", err)
	}
	return nil
}

// userLockKey hashes a user UUID into a 64-bit signed key for
// pg_advisory_xact_lock. FNV-1a over the raw 16-byte UUID is stable
// per-input, so two concurrent create calls for the same user serialize
// on the same key while distinct users get distinct keys.
func userLockKey(userID uuid.UUID) int64 {
	h := fnv.New64a()
	_, _ = h.Write(userID[:])
	return int64(h.Sum64())
}
