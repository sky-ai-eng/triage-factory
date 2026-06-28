package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// accessChangeLogStore is the Postgres impl of db.AccessChangeLogStore. Holds
// both pools, the same split ExternalActions uses:
//
//   - app: app pool (RLS-active). Record composes inside the claims-bearing
//     WithTx that runs the governance action it audits, so the
//     access_change_log_all policy's WITH CHECK (org_id = tf.current_org_id() AND
//     tf.user_has_org_access(org_id)) is exercised on write, and the audit view's
//     ListByOrg reads under the same org-scoped USING clause.
//   - admin: admin pool (BYPASSRLS). RecordSystem routes here for the SSO JIT
//     auto-provisioning seam, whose user has no membership/claims at grant time.
//
// org_id stays in every statement as defense in depth. See TFAC-471 / TFAC-486.
//
// (The invite-accept + JIT org_member_granted writes are the exception — they run
// on the admin pool's raw transaction, atomically with the grant, because the
// user has no RLS standing to insert their own membership; that seam writes the
// audit row via a raw INSERT in the server package, not through this store.)
type accessChangeLogStore struct {
	app   queryer
	admin queryer
}

func newAccessChangeLogStore(app, admin queryer) db.AccessChangeLogStore {
	return &accessChangeLogStore{app: app, admin: admin}
}

var _ db.AccessChangeLogStore = (*accessChangeLogStore)(nil)

func (s *accessChangeLogStore) Record(ctx context.Context, orgID string, e domain.AccessChange) error {
	return s.record(ctx, s.app, orgID, e)
}

// RecordSystem runs the same insert on the admin pool (BYPASSRLS) for the SSO JIT
// auto-provisioning seam, which holds an org identity but no JWT-claims context.
// See the type doc.
func (s *accessChangeLogStore) RecordSystem(ctx context.Context, orgID string, e domain.AccessChange) error {
	return s.record(ctx, s.admin, orgID, e)
}

func (s *accessChangeLogStore) record(ctx context.Context, q queryer, orgID string, e domain.AccessChange) error {
	// A caller-supplied e.ID is honored (parity with the SQLite impl); an empty
	// e.ID falls back to gen_random_uuid() server-side. created_at is left to
	// the column DEFAULT now(). Empty nullable columns ('') → NULL via NULLIF;
	// the uuid columns cast after the NULLIF so an empty string never reaches
	// ::uuid.
	_, err := q.ExecContext(ctx, `
		INSERT INTO access_change_log
			(id, org_id, actor_user_id, action, target_user_id, team_id, detail_json)
		VALUES (
			COALESCE(NULLIF($1, '')::uuid, gen_random_uuid()), $2,
			NULLIF($3, '')::uuid, $4, NULLIF($5, '')::uuid, NULLIF($6, '')::uuid,
			NULLIF($7, '')
		)
	`,
		e.ID, orgID, e.ActorUserID, e.Action, e.TargetUserID, e.TeamID, e.DetailJSON,
	)
	return err
}

func (s *accessChangeLogStore) ListByOrg(ctx context.Context, orgID string, opts domain.AccessChangeListOpts) ([]domain.AccessChange, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}
	// org_id filter first; an optional category narrows to its action set via a
	// dynamic IN clause; LIMIT/OFFSET page the newest-first window. Placeholders
	// are numbered as they're appended ($1 is org_id).
	args := []any{orgID}
	where := "org_id = $1"
	n := 1
	if actions := domain.AccessActionsInCategory(opts.Category); len(actions) > 0 {
		ph := make([]string, len(actions))
		for i, a := range actions {
			n++
			ph[i] = fmt.Sprintf("$%d", n)
			args = append(args, a)
		}
		where += " AND action IN (" + strings.Join(ph, ", ") + ")"
	}
	n++
	limitPh := fmt.Sprintf("$%d", n)
	args = append(args, limit)
	offsetClause := ""
	if opts.Offset > 0 {
		n++
		offsetClause = fmt.Sprintf(" OFFSET $%d", n)
		args = append(args, opts.Offset)
	}
	rows, err := s.app.QueryContext(ctx, `
		SELECT id::text, org_id::text,
		       COALESCE(actor_user_id::text, ''), action,
		       COALESCE(target_user_id::text, ''), COALESCE(team_id::text, ''),
		       COALESCE(detail_json, ''), created_at
		  FROM access_change_log
		 WHERE `+where+`
		 ORDER BY created_at DESC, id DESC
		 LIMIT `+limitPh+offsetClause, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.AccessChange
	for rows.Next() {
		var e domain.AccessChange
		if err := rows.Scan(
			&e.ID, &e.OrgID, &e.ActorUserID, &e.Action,
			&e.TargetUserID, &e.TeamID, &e.DetailJSON, &e.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
