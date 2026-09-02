package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// orgMembershipsStore is the Postgres impl of db.OrgMembershipsStore. Holds
// both pools:
//
//   - app: the core roster read (org_memberships ⋈ users) and the
//     UpdateRole / Remove mutations. org_memberships_select + users_select
//     both permit co-org reads under the caller's claims, and the
//     update/delete policies gate org-admin / self — so RLS is the
//     defense-in-depth backstop behind the handler's front gate.
//   - admin: the GitHub/Jira identity enrichment only. The
//     user_github_identities / user_jira_identities select policies are
//     strictly self-only (a member can't read a peer's identity row under
//     RLS), but the roster's whole purpose is to show every member's
//     identity readiness to their teammates. The admin-pool reads are scoped
//     to this org by an org_memberships join, so they surface only the
//     identities of the same members the RLS-gated roster already returned —
//     no cross-org reach.
type orgMembershipsStore struct {
	app   queryer
	admin queryer
}

func newOrgMembershipsStore(app, admin queryer) db.OrgMembershipsStore {
	return &orgMembershipsStore{app: app, admin: admin}
}

var _ db.OrgMembershipsStore = (*orgMembershipsStore)(nil)

func (s *orgMembershipsStore) ListWithIdentity(ctx context.Context, orgID, githubBaseURL, jiraBaseURL string, opts db.ListOpts) ([]domain.OrgMember, int, error) {
	// 1. Core roster on the app pool (RLS-gated). display_name is nullable —
	//    COALESCE renders the empty string, matching GetDisplayName's "".
	//    The count runs on the same pool and predicate as the page, so
	//    "showing 50 of 213" cannot be assembled from two different snapshots.
	var total int
	if err := s.app.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM org_memberships om
		JOIN users u ON u.id = om.user_id
		WHERE om.org_id = $1
	`, orgID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count org members: %w", err)
	}
	if opts.CountOnly {
		return []domain.OrgMember{}, total, nil
	}
	query := `
		SELECT om.user_id::text, COALESCE(u.display_name, ''), om.role::text
		FROM org_memberships om
		JOIN users u ON u.id = om.user_id
		WHERE om.org_id = $1
		ORDER BY COALESCE(u.display_name, ''), om.user_id`
	args := []any{orgID}
	if opts.Limit > 0 {
		query += `
		LIMIT $2 OFFSET $3`
		args = append(args, opts.Limit, opts.Offset)
	}
	rows, err := s.app.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list org members: %w", err)
	}
	defer rows.Close()
	out := []domain.OrgMember{}
	for rows.Next() {
		var m domain.OrgMember
		if err := rows.Scan(&m.UserID, &m.DisplayName, &m.Role); err != nil {
			return nil, 0, fmt.Errorf("scan org member: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate org members: %w", err)
	}

	// 2. Identity enrichment on the admin pool, scoped to this org's members.
	//    Resolve to the host identities are actually keyed under — the same
	//    reverse-lookup rule the poller + review routing use. GitHub: an unset
	//    github_base_url resolves to the deployment default (EffectiveGitHubHost), where
	//    those identities live — a raw NormalizeGitHubHost would key host=""
	//    and miss every github.com binding, so the whole roster would read
	//    "Not connected". Jira has no public default host, so an unset
	//    jira_base_url normalizes to "" and matches nothing — correct, since
	//    that org has no Jira configured.
	ghMap, err := s.identityMap(ctx, `
		SELECT gh.user_id::text, gh.login
		FROM user_github_identities gh
		JOIN org_memberships om ON om.user_id = gh.user_id AND om.org_id = $1
		WHERE gh.github_base_url = $2
	`, orgID, db.EffectiveGitHubHost(githubBaseURL))
	if err != nil {
		return nil, 0, fmt.Errorf("list org member github identities: %w", err)
	}
	jiraMap, err := s.identityMap(ctx, `
		SELECT j.user_id::text, j.account_id
		FROM user_jira_identities j
		JOIN org_memberships om ON om.user_id = j.user_id AND om.org_id = $1
		WHERE j.jira_base_url = $2
	`, orgID, db.NormalizeJiraHost(jiraBaseURL))
	if err != nil {
		return nil, 0, fmt.Errorf("list org member jira identities: %w", err)
	}

	// 3. Merge. An absent (or empty) binding leaves the pointer nil — the
	//    "Not connected" state, matching config_handler's "" → nil rule.
	for i := range out {
		if login, ok := ghMap[out[i].UserID]; ok {
			out[i].GitHubUsername = &login
		}
		if acct, ok := jiraMap[out[i].UserID]; ok {
			out[i].JiraAccountID = &acct
		}
	}
	return out, total, nil
}

// identityMap runs a (user_id, value) admin-pool query and returns the
// user_id → value map, dropping empty values so an "" binding reads as "not
// connected" (nil pointer) rather than a present-but-blank handle.
func (s *orgMembershipsStore) identityMap(ctx context.Context, query string, args ...any) (map[string]string, error) {
	rows, err := s.admin.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var id, val string
		if err := rows.Scan(&id, &val); err != nil {
			return nil, err
		}
		if val != "" {
			m[id] = val
		}
	}
	return m, rows.Err()
}

func (s *orgMembershipsStore) RoleFor(ctx context.Context, orgID, userID string) (string, error) {
	var role string
	err := s.app.QueryRowContext(ctx, `
		SELECT role::text FROM org_memberships WHERE org_id = $1 AND user_id = $2
	`, orgID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read org member role: %w", err)
	}
	return role, nil
}

func (s *orgMembershipsStore) UpdateRole(ctx context.Context, orgID, userID, role string) (string, error) {
	// Capture the prior role in the same statement (the prev CTE snapshots it
	// before the UPDATE applies) and return it, so the governance audit log can
	// record the old→new transition. A zero-row UPDATE — the member doesn't
	// exist — yields no RETURNING row, which Scan surfaces as sql.ErrNoRows; map
	// that to ErrOrgMemberNotFound (the assertOneRow contract, expressed through
	// the query instead of RowsAffected). Cast the bound string to the enum
	// explicitly: role is a public.org_role column; a bare text param relies on
	// server-side inference, and the explicit ::org_role keeps it unambiguous
	// (the handler already rejected any value outside the enum, so 22P02 never
	// trips).
	var oldRole string
	err := s.app.QueryRowContext(ctx, `
		WITH prev AS (
			SELECT role FROM org_memberships
			WHERE org_id = $1 AND user_id = $2
		)
		UPDATE org_memberships m SET role = $3::org_role
		FROM prev
		WHERE m.org_id = $1 AND m.user_id = $2
		RETURNING prev.role::text
	`, orgID, userID, role).Scan(&oldRole)
	if errors.Is(err, sql.ErrNoRows) {
		return "", db.ErrOrgMemberNotFound
	}
	if err != nil {
		return "", translateOwnerGuard(fmt.Errorf("update org_membership role: %w", err))
	}
	return oldRole, nil
}

func (s *orgMembershipsStore) Remove(ctx context.Context, orgID, userID string) error {
	res, err := s.app.ExecContext(ctx, `
		DELETE FROM org_memberships
		WHERE org_id = $1 AND user_id = $2
	`, orgID, userID)
	if err != nil {
		return translateOwnerGuard(fmt.Errorf("delete org_membership: %w", err))
	}
	return assertOneRow(res, "delete org_membership")
}

// TransferOwnership runs the three ordered writes the ownership-transfer
// trigger pair requires, all on the app pool inside the caller's WithTx (which
// binds the *current owner's* claims — tf.guard_org_owner_transfer reads
// tf.current_user_id()). The order is load-bearing: the BEFORE-UPDATE trigger
// on orgs refuses to repoint owner_user_id to a user who isn't already
// role='owner', so step 1 promotes the target before step 2 moves the sentinel.
func (s *orgMembershipsStore) TransferOwnership(ctx context.Context, orgID, currentOwnerID, newOwnerID string) error {
	// 1. Promote the new owner. A 0-row result means newOwnerID isn't a member
	//    (ErrOrgMemberNotFound → the handler's 422). The org keeps its existing
	//    owner row throughout, so guard_org_owners stays satisfied here.
	res, err := s.app.ExecContext(ctx, `
		UPDATE org_memberships SET role = 'owner'::org_role
		WHERE org_id = $1 AND user_id = $2
	`, orgID, newOwnerID)
	if err != nil {
		return translateTransferGuard(fmt.Errorf("promote new org owner: %w", err))
	}
	if e := assertOneRow(res, "promote new org owner"); e != nil {
		return e
	}

	// 2. Repoint the founder sentinel. guard_org_owner_transfer checks the
	//    caller is the current owner (42501) and the new owner already holds
	//    role=owner (23514) — both satisfied by the handler gate + step 1.
	if _, err := s.app.ExecContext(ctx, `
		UPDATE orgs SET owner_user_id = $2 WHERE id = $1
	`, orgID, newOwnerID); err != nil {
		return translateTransferGuard(fmt.Errorf("reassign org owner: %w", err))
	}

	// 3. Demote the former owner. A new owner exists by now, so guard_org_owners
	//    stays satisfied. currentOwnerID is the gated caller who held role=owner
	//    through steps 1–2, so this matches exactly one row — a zero-row result
	//    is an invariant violation (their membership vanished mid-tx), NOT the
	//    "target isn't a member" case. Return a plain error so it surfaces as a
	//    500, not assertOneRow's ErrOrgMemberNotFound (which the handler maps to
	//    a 422 about the *new* owner — wrong subject, wrong status).
	res, err = s.app.ExecContext(ctx, `
		UPDATE org_memberships SET role = 'admin'::org_role
		WHERE org_id = $1 AND user_id = $2
	`, orgID, currentOwnerID)
	if err != nil {
		return translateTransferGuard(fmt.Errorf("demote former org owner: %w", err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("demote former org owner: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("demote former org owner: no row affected for %s (membership vanished mid-transaction)", currentOwnerID)
	}
	return nil
}

// translateOwnerGuard maps the tf.guard_org_owners trigger's SQLSTATE 23514
// to db.ErrLastOwnerGuard so the handler answers 409 without importing the
// pg driver. guard_org_owners is the only 23514 source on org_memberships
// (the table carries no CHECK constraints; an invalid role enum fails with a
// different code, and callers validate role before the write anyway), so the
// code→sentinel mapping is unambiguous here. Any other error passes through.
func translateOwnerGuard(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23514" {
		return errors.Join(db.ErrLastOwnerGuard, err)
	}
	return err
}

// translateTransferGuard maps the ownership-transfer trigger SQLSTATEs to db
// sentinels so the handler renders the right status without importing the pg
// driver. tf.guard_org_owner_transfer raises 42501 when the caller isn't the
// current owner (→ ErrNotOrgOwner, a 403) and 23514 when the new owner isn't
// yet role=owner; tf.guard_org_owners raises 23514 when a write would zero an
// org's owners. Both 23514 sources fold to ErrLastOwnerGuard (a 409) — the
// transfer orders its writes to avoid tripping either, so a 23514 surfacing
// here is a genuine invariant violation worth a clean 409, not a routine
// outcome. Any other error passes through.
func translateTransferGuard(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "42501":
			return errors.Join(db.ErrNotOrgOwner, err)
		case "23514":
			return errors.Join(db.ErrLastOwnerGuard, err)
		}
	}
	return err
}

// assertOneRow turns a zero-row UPDATE/DELETE result into
// db.ErrOrgMemberNotFound. The handlers gate org-admin (or self) before
// calling, so a row visible to the policy but absent means the target user
// simply isn't a member — a 404, not a silent success.
func assertOneRow(res sql.Result, op string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: rows affected: %w", op, err)
	}
	if n == 0 {
		return db.ErrOrgMemberNotFound
	}
	return nil
}
