package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// orgEventSourceStore is the SQLite impl of db.OrgEventSourceStore. Local mode
// is N=1 — one process, one org — so the app/admin split the Postgres impl
// draws collapses here and the ...System variants share the same connection,
// with the org asserted rather than gated.
type orgEventSourceStore struct{ q queryer }

func newOrgEventSourceStore(q queryer) db.OrgEventSourceStore {
	return &orgEventSourceStore{q: q}
}

var _ db.OrgEventSourceStore = (*orgEventSourceStore)(nil)

// sqliteOrgEventSourceCols is the column list every read here projects, shared
// by the point read and the write's RETURNING so the two shapes cannot drift.
const sqliteOrgEventSourceCols = `org_id, kind, disabled, disabled_at, disabled_by`

func (s *orgEventSourceStore) ListDisabled(ctx context.Context, orgID string) ([]string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx,
		`SELECT kind FROM org_event_sources WHERE org_id = ? AND disabled = 1 ORDER BY kind`, orgID)
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

func (s *orgEventSourceStore) ListDisabledSystem(ctx context.Context, orgID string) ([]string, error) {
	return s.ListDisabled(ctx, orgID)
}

func (s *orgEventSourceStore) SetDisabled(ctx context.Context, orgID, kind string, disabled bool, actorUserID string) (domain.OrgEventSource, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.OrgEventSource{}, err
	}
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
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (org_id, kind) DO UPDATE SET
			disabled    = excluded.disabled,
			disabled_at = excluded.disabled_at,
			disabled_by = excluded.disabled_by
		RETURNING `+sqliteOrgEventSourceCols,
		orgID, kind, disabled, at, actor))
}

func (s *orgEventSourceStore) Get(ctx context.Context, orgID, kind string) (*domain.OrgEventSource, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row, err := scanOrgEventSourceRow(s.q.QueryRowContext(ctx,
		`SELECT `+sqliteOrgEventSourceCols+` FROM org_event_sources WHERE org_id = ? AND kind = ?`,
		orgID, kind))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
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
