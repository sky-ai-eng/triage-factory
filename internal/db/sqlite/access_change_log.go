package sqlite

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// accessChangeLogStore is the SQLite impl of db.AccessChangeLogStore. SQLite is
// single-tenant (local mode, N=1) with no RLS — the org_id column exists for
// parity with the Postgres baseline but every row defaults to LocalDefaultOrgID.
// The Postgres impl gates writes/reads with an org-scoped RLS policy; here both
// pools collapse to the one connection. See TFAC-471.
type accessChangeLogStore struct{ q queryer }

func newAccessChangeLogStore(q queryer) db.AccessChangeLogStore {
	return &accessChangeLogStore{q: q}
}

var _ db.AccessChangeLogStore = (*accessChangeLogStore)(nil)

func (s *accessChangeLogStore) Record(ctx context.Context, orgID string, e domain.AccessChange) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	id := e.ID
	if id == "" {
		id = uuid.New().String()
	}
	// created_at is left to the column DEFAULT CURRENT_TIMESTAMP. Empty
	// nullable fields serialize to SQL NULL via nullIfEmpty.
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO access_change_log
			(id, org_id, actor_user_id, action, target_user_id, team_id, detail_json)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`,
		id, orgID, nullIfEmpty(e.ActorUserID), e.Action,
		nullIfEmpty(e.TargetUserID), nullIfEmpty(e.TeamID), nullIfEmpty(e.DetailJSON),
	)
	return err
}

func (s *accessChangeLogStore) ListByOrg(ctx context.Context, orgID string, opts domain.AccessChangeListOpts) ([]domain.AccessChange, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}
	// org_id filter first; an optional category narrows to its action set via a
	// dynamic IN clause; LIMIT/OFFSET page the newest-first window.
	args := []any{orgID}
	where := "org_id = ?"
	if actions := domain.AccessActionsInCategory(opts.Category); len(actions) > 0 {
		ph := make([]string, len(actions))
		for i, a := range actions {
			ph[i] = "?"
			args = append(args, a)
		}
		where += " AND action IN (" + strings.Join(ph, ", ") + ")"
	}
	args = append(args, limit)
	offsetClause := ""
	if opts.Offset > 0 {
		offsetClause = " OFFSET ?"
		args = append(args, opts.Offset)
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, org_id,
		       COALESCE(actor_user_id, ''), action,
		       COALESCE(target_user_id, ''), COALESCE(team_id, ''),
		       COALESCE(detail_json, ''), created_at
		  FROM access_change_log
		 WHERE `+where+`
		 ORDER BY created_at DESC, id DESC
		 LIMIT ?`+offsetClause, args...)
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
