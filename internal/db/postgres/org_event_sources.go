package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// orgEventSourceStore is the Postgres impl of db.OrgEventSourceStore.
// Split-pool: q is the app pool (RLS — member SELECT, admin write), admin
// serves the ...System read for the router and the poller, neither of which
// has request claims for a policy to gate on.
type orgEventSourceStore struct {
	q     queryer
	admin queryer
}

func newOrgEventSourceStore(q, admin queryer) db.OrgEventSourceStore {
	return &orgEventSourceStore{q: q, admin: admin}
}

var _ db.OrgEventSourceStore = (*orgEventSourceStore)(nil)

// pgOrgEventSourceCols is the column list every read here projects, shared by
// the point read and the write's RETURNING so the two shapes cannot drift.
const pgOrgEventSourceCols = `org_id, kind, disabled, disabled_at, disabled_by`

func (s *orgEventSourceStore) ListDisabled(ctx context.Context, orgID string) ([]string, error) {
	return listDisabledSources(ctx, s.q, orgID)
}

func (s *orgEventSourceStore) ListDisabledSystem(ctx context.Context, orgID string) ([]string, error) {
	return listDisabledSources(ctx, s.admin, orgID)
}

func listDisabledSources(ctx context.Context, q queryer, orgID string) ([]string, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT kind FROM org_event_sources WHERE org_id = $1 AND disabled ORDER BY kind`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			return nil, err
		}
		out = append(out, kind)
	}
	return out, rows.Err()
}

func (s *orgEventSourceStore) SetDisabled(ctx context.Context, orgID, kind string, disabled bool, actorUserID string) (domain.OrgEventSource, error) {
	// Both stamps are derived from `disabled` in the statement itself rather
	// than by the caller passing three consistent values: an enabled row with
	// a disabled_at on it would be a lie the schema has no way to refuse.
	var (
		at    any
		actor any
	)
	if disabled {
		at = time.Now().UTC()
		if actorUserID != "" {
			actor = actorUserID
		}
	}
	return scanOrgEventSourceRow(s.q.QueryRowContext(ctx, `
		INSERT INTO org_event_sources (org_id, kind, disabled, disabled_at, disabled_by)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (org_id, kind) DO UPDATE SET
			disabled    = EXCLUDED.disabled,
			disabled_at = EXCLUDED.disabled_at,
			disabled_by = EXCLUDED.disabled_by
		RETURNING `+pgOrgEventSourceCols,
		orgID, kind, disabled, at, actor))
}

func scanOrgEventSourceRow(row *sql.Row) (domain.OrgEventSource, error) {
	var (
		out domain.OrgEventSource
		at  sql.NullTime
		by  sql.NullString
	)
	if err := row.Scan(&out.OrgID, &out.Kind, &out.Disabled, &at, &by); err != nil {
		return domain.OrgEventSource{}, err
	}
	if at.Valid {
		t := at.Time.UTC()
		out.DisabledAt = &t
	}
	if by.Valid {
		v := by.String
		out.DisabledBy = &v
	}
	return out, nil
}

func (s *orgEventSourceStore) Get(ctx context.Context, orgID, kind string) (*domain.OrgEventSource, error) {
	row, err := scanOrgEventSourceRow(s.q.QueryRowContext(ctx,
		`SELECT `+pgOrgEventSourceCols+` FROM org_event_sources WHERE org_id = $1 AND kind = $2`,
		orgID, kind))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}
