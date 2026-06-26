package postgres

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// accessChangeLogStore is the Postgres impl of db.AccessChangeLogStore. Wired
// against the APP pool (RLS-active): every Record call composes inside the
// claims-bearing WithTx that runs the governance action it audits, so the
// access_change_log_all policy's WITH CHECK (org_id = tf.current_org_id() AND
// tf.user_has_org_access(org_id)) is exercised on write, and the future audit
// view's ListByOrg reads under the same org-scoped USING clause. org_id stays
// in every statement as defense in depth. See TFAC-471.
//
// (The invite-accept org_member_granted write is the one exception — it runs on
// the admin pool's raw transaction because the invitee has no RLS standing to
// insert their own membership; that seam writes the audit row via a raw INSERT
// in the server package, not through this store.)
type accessChangeLogStore struct{ app queryer }

func newAccessChangeLogStore(app queryer) db.AccessChangeLogStore {
	return &accessChangeLogStore{app: app}
}

var _ db.AccessChangeLogStore = (*accessChangeLogStore)(nil)

func (s *accessChangeLogStore) Record(ctx context.Context, orgID string, e domain.AccessChange) error {
	// A caller-supplied e.ID is honored (parity with the SQLite impl); an empty
	// e.ID falls back to gen_random_uuid() server-side. created_at is left to
	// the column DEFAULT now(). Empty nullable columns ('') → NULL via NULLIF;
	// the uuid columns cast after the NULLIF so an empty string never reaches
	// ::uuid.
	_, err := s.app.ExecContext(ctx, `
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
	rows, err := s.app.QueryContext(ctx, `
		SELECT id::text, org_id::text,
		       COALESCE(actor_user_id::text, ''), action,
		       COALESCE(target_user_id::text, ''), COALESCE(team_id::text, ''),
		       COALESCE(detail_json, ''), created_at
		  FROM access_change_log
		 WHERE org_id = $1
		 ORDER BY created_at DESC, id DESC
		 LIMIT $2
	`, orgID, limit)
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
