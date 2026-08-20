package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// placementOverrideStore is the SQLite impl of db.PlacementOverrideStore —
// the placement pin/replica overrides. SQLite is N=1 and unscoped:
// the rows are inert (the placement hash always returns self), but the store
// exists for interface + conformance symmetry across dialects.
type placementOverrideStore struct{ q queryer }

func newPlacementOverrideStore(q queryer) db.PlacementOverrideStore {
	return &placementOverrideStore{q: q}
}

// NewPlacementOverrideStore builds a standalone store against a pooled
// *sql.DB — the narrow constructor the CLI verb uses (mirrors
// NewInstanceStore).
func NewPlacementOverrideStore(conn *sql.DB) db.PlacementOverrideStore {
	return newPlacementOverrideStore(conn)
}

var _ db.PlacementOverrideStore = (*placementOverrideStore)(nil)

// placementOverrideColumns is the canonical projection of a
// placement_overrides row, in the order scanPlacementOverride reads them. Both
// reads SELECT it and Upsert RETURNs it, so the write shape cannot drift from
// the read shape.
const placementOverrideColumns = `org_id, key_kind, key_value, pinned_instance_id, replicas, updated_at`

func scanPlacementOverride(scan func(...any) error) (domain.PlacementOverride, error) {
	var ov domain.PlacementOverride
	var pinned sql.NullString
	if err := scan(&ov.OrgID, &ov.KeyKind, &ov.KeyValue, &pinned, &ov.Replicas, &ov.UpdatedAt); err != nil {
		return ov, err
	}
	ov.PinnedInstanceID = pinned.String
	return ov, nil
}

func (s *placementOverrideStore) Get(ctx context.Context, orgID, keyKind, keyValue string) (*domain.PlacementOverride, error) {
	ov, err := scanPlacementOverride(s.q.QueryRowContext(ctx, `
		SELECT `+placementOverrideColumns+`
		FROM placement_overrides
		WHERE org_id = ? AND key_kind = ? AND key_value = ?
	`, orgID, keyKind, keyValue).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ov, nil
}

func (s *placementOverrideStore) List(ctx context.Context, orgID string) ([]domain.PlacementOverride, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+placementOverrideColumns+`
		FROM placement_overrides
		WHERE org_id = ?
		ORDER BY key_kind, key_value
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.PlacementOverride
	for rows.Next() {
		ov, err := scanPlacementOverride(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, ov)
	}
	return out, rows.Err()
}

func (s *placementOverrideStore) Upsert(ctx context.Context, ov domain.PlacementOverride) (domain.PlacementOverride, error) {
	return scanPlacementOverride(s.q.QueryRowContext(ctx, `
		INSERT INTO placement_overrides (org_id, key_kind, key_value, pinned_instance_id, replicas, updated_at)
		VALUES (?, ?, ?, NULLIF(?, ''), ?, ?)
		ON CONFLICT(org_id, key_kind, key_value) DO UPDATE SET
			pinned_instance_id = excluded.pinned_instance_id,
			replicas           = excluded.replicas,
			updated_at         = excluded.updated_at
		RETURNING `+placementOverrideColumns,
		ov.OrgID, ov.KeyKind, ov.KeyValue, ov.PinnedInstanceID, ov.Replicas, time.Now().UTC()).Scan)
}

func (s *placementOverrideStore) Delete(ctx context.Context, orgID, keyKind, keyValue string) (bool, error) {
	res, err := s.q.ExecContext(ctx, `
		DELETE FROM placement_overrides WHERE org_id = ? AND key_kind = ? AND key_value = ?
	`, orgID, keyKind, keyValue)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
