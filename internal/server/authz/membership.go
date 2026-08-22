package authz

import (
	"context"
	"database/sql"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// membership is every fact about who a caller is that a gate here needs, and
// the one place the deployment shape decides those facts.
//
// The Postgres answers are tf.* SQL helpers with ::uuid casts — under SQLite not
// merely unhelpful but a syntax error — so a gate reaching one in local mode
// 500s. Behind this interface none can: a gate asks a question and gets this
// deployment's answer, and a new fact does not build until the local
// implementation says what N=1 means for it.
//
// Everything here is a fact, never a decision. Whether a false answer is a 403
// or a 404 stays with the gate, beside the disclosure rule it follows.
type membership interface {
	// teamInOrg reports whether teamID belongs to the caller's active org.
	teamInOrg(ctx context.Context, orgID, userID, teamID string) (bool, error)
	// teamArchived reports whether teamID is archived. A missing team is not
	// archived: existence is teamInOrg's question, asked first.
	teamArchived(ctx context.Context, orgID, userID, teamID string) (bool, error)
	// userIsTeamAdmin reports the 'admin' role on teamID.
	userIsTeamAdmin(ctx context.Context, orgID, userID, teamID string) (bool, error)
	// userCanWriteTeam reports a non-viewer role on teamID.
	userCanWriteTeam(ctx context.Context, orgID, userID, teamID string) (bool, error)
	// userHasOrgAccess reports membership of orgID in any role.
	userHasOrgAccess(ctx context.Context, userID, orgID string) (bool, error)
	// userIsOrgAdmin reports an 'owner' or 'admin' role in orgID.
	userIsOrgAdmin(ctx context.Context, userID, orgID string) (bool, error)
	// userOwnsOrg reports the founder sentinel, orgs.owner_user_id. Distinct
	// from admin because ownership transfer is owner-only.
	userOwnsOrg(ctx context.Context, userID, orgID string) (bool, error)
	// orgMemberCount counts the org's members.
	orgMemberCount(ctx context.Context, orgID, userID string) (int, error)
	// teamMemberCountAndRole returns the team's member count and the caller's
	// role in it ("admin" / "member", or "" when not a member).
	teamMemberCountAndRole(ctx context.Context, orgID, userID, teamID string) (int, string, error)
	// taskWritable answers both halves of the task-write gate in one read:
	// whether the caller can see taskID at all, and whether they may write it.
	// One read, because the two drive a 403 and a deliberate silence and must
	// not disagree.
	taskWritable(ctx context.Context, orgID, userID, taskID string) (visible, canWrite bool, err error)
}

// localMembership is N=1: one org, one user, and that user is its admin and its
// owner. Nothing reads anything — the local schema holds no memberships, and the
// sole user is every role by construction.
type localMembership struct{}

func (localMembership) teamInOrg(context.Context, string, string, string) (bool, error) {
	return true, nil
}

func (localMembership) teamArchived(context.Context, string, string, string) (bool, error) {
	// N=1 never archives its sole team.
	return false, nil
}

func (localMembership) userIsTeamAdmin(context.Context, string, string, string) (bool, error) {
	return true, nil
}

func (localMembership) userCanWriteTeam(context.Context, string, string, string) (bool, error) {
	return true, nil
}

func (localMembership) userHasOrgAccess(context.Context, string, string) (bool, error) {
	return true, nil
}

func (localMembership) userIsOrgAdmin(context.Context, string, string) (bool, error) {
	return true, nil
}

func (localMembership) userOwnsOrg(context.Context, string, string) (bool, error) {
	return true, nil
}

func (localMembership) orgMemberCount(context.Context, string, string) (int, error) {
	return 1, nil
}

func (localMembership) teamMemberCountAndRole(context.Context, string, string, string) (int, string, error) {
	return 1, "admin", nil
}

func (localMembership) taskWritable(context.Context, string, string, string) (bool, bool, error) {
	return true, true, nil
}

// pgMembership answers from Postgres, inside db.WithTx so every read sees the
// caller's claims. Most probe the same tf.* helper the row policies call, so a
// gate and the policy behind it cannot drift; the rest read columns no helper
// exposes. Which claims each carries is load-bearing: the team-scoped helpers
// read memberships under RLS, which gates on tf.current_org_id(), so a Sub-only
// claim would miscount.
type pgMembership struct{ db *sql.DB }

// probe runs a one-column boolean SQL function under the given claims.
func (m pgMembership) probe(ctx context.Context, claims db.Claims, query string, arg any) (bool, error) {
	var ok bool
	err := db.WithTx(ctx, m.db, claims, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, query, arg).Scan(&ok)
	})
	return ok, err
}

func (m pgMembership) teamInOrg(ctx context.Context, orgID, userID, teamID string) (bool, error) {
	return m.probe(ctx, db.Claims{Sub: userID, OrgID: orgID}, `SELECT tf.team_in_current_org($1::uuid)`, teamID)
}

func (m pgMembership) teamArchived(ctx context.Context, orgID, userID, teamID string) (bool, error) {
	var archived sql.NullBool
	err := db.WithTx(ctx, m.db, db.Claims{Sub: userID, OrgID: orgID}, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx,
			`SELECT deleted_at IS NOT NULL FROM teams WHERE id = $1::uuid`, teamID,
		).Scan(&archived)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return archived.Valid && archived.Bool, nil
}

func (m pgMembership) userIsTeamAdmin(ctx context.Context, orgID, userID, teamID string) (bool, error) {
	return m.probe(ctx, db.Claims{Sub: userID, OrgID: orgID}, `SELECT tf.user_is_team_admin($1::uuid)`, teamID)
}

func (m pgMembership) userCanWriteTeam(ctx context.Context, orgID, userID, teamID string) (bool, error) {
	return m.probe(ctx, db.Claims{Sub: userID, OrgID: orgID}, `SELECT tf.user_can_write_team($1::uuid)`, teamID)
}

// The three org-scoped probes carry a Sub-only claim: their helpers read
// request.jwt.claims through tf.current_user_id() and take the org as an
// argument, so a missing or wrong claim resolves to NULL — no membership —
// even if a future bug let a wrong userID argument land here.
func (m pgMembership) userHasOrgAccess(ctx context.Context, userID, orgID string) (bool, error) {
	return m.probe(ctx, db.Claims{Sub: userID}, `SELECT tf.user_has_org_access($1::uuid)`, orgID)
}

func (m pgMembership) userIsOrgAdmin(ctx context.Context, userID, orgID string) (bool, error) {
	return m.probe(ctx, db.Claims{Sub: userID}, `SELECT tf.user_is_org_admin($1::uuid)`, orgID)
}

func (m pgMembership) userOwnsOrg(ctx context.Context, userID, orgID string) (bool, error) {
	return m.probe(ctx, db.Claims{Sub: userID}, `SELECT tf.user_owns_org($1::uuid)`, orgID)
}

func (m pgMembership) orgMemberCount(ctx context.Context, orgID, userID string) (int, error) {
	var n int
	err := db.WithTx(ctx, m.db, db.Claims{Sub: userID, OrgID: orgID}, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM org_memberships WHERE org_id = $1::uuid`, orgID,
		).Scan(&n)
	})
	return n, err
}

// One transaction under one claims context, because the count and the caller's
// role are read against the same membership set and the role arm resolves the
// caller through tf.current_user_id() rather than an argument.
func (m pgMembership) teamMemberCountAndRole(ctx context.Context, orgID, userID, teamID string) (int, string, error) {
	var (
		n    int
		role string
	)
	err := db.WithTx(ctx, m.db, db.Claims{Sub: userID, OrgID: orgID}, func(tx *sql.Tx) error {
		if e := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM memberships WHERE team_id = $1::uuid`, teamID,
		).Scan(&n); e != nil {
			return e
		}
		e := tx.QueryRowContext(ctx,
			`SELECT role FROM memberships
			  WHERE team_id = $1::uuid AND user_id = tf.current_user_id()`, teamID,
		).Scan(&role)
		if errors.Is(e, sql.ErrNoRows) {
			return nil
		}
		return e
	})
	return n, role, err
}

// taskWritable mirrors the team branches of the tasks_update write RLS policy
// arm-for-arm. It reads tables, but what it encodes is the policy, so it is
// spelled out here to be diffable against the policy it shadows.
func (m pgMembership) taskWritable(ctx context.Context, orgID, userID, taskID string) (bool, bool, error) {
	var visible, canWrite bool
	err := db.WithTx(ctx, m.db, db.Claims{Sub: userID, OrgID: orgID}, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT
			  EXISTS (SELECT 1 FROM tasks t WHERE t.id = $1::uuid AND t.org_id = $2::uuid) AS visible,
			  EXISTS (
			    SELECT 1 FROM tasks t
			    WHERE t.id = $1::uuid AND t.org_id = $2::uuid AND (
			      (t.visibility = 'private' AND t.creator_user_id = tf.current_user_id())
			      OR (t.visibility = 'org' AND tf.user_is_org_admin(t.org_id))
			      OR (t.visibility = 'team' AND (
			           (t.team_id IS NOT NULL AND tf.user_can_write_team(t.team_id))
			           OR EXISTS (SELECT 1 FROM task_teams tt WHERE tt.task_id = t.id AND tf.user_can_write_team(tt.team_id))
			      ))
			    )
			  ) AS can_write`,
			taskID, orgID,
		).Scan(&visible, &canWrite)
	})
	return visible, canWrite, err
}
