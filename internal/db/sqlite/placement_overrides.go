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
// the placement pin/replica overrides (TFAC-587). SQLite is N=1 and unscoped:
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

func (s *placementOverrideStore) Get(ctx context.Context, orgID, keyKind, keyValue string) (*domain.PlacementOverride, error) {
	var ov domain.PlacementOverride
	var pinned sql.NullString
	err := s.q.QueryRowContext(ctx, `
		SELECT org_id, key_kind, key_value, pinned_instance_id, replicas, updated_at
		FROM placement_overrides
		WHERE org_id = ? AND key_kind = ? AND key_value = ?
	`, orgID, keyKind, keyValue).Scan(&ov.OrgID, &ov.KeyKind, &ov.KeyValue, &pinned, &ov.Replicas, &ov.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ov.PinnedInstanceID = pinned.String
	return &ov, nil
}

func (s *placementOverrideStore) List(ctx context.Context, orgID string) ([]domain.PlacementOverride, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT org_id, key_kind, key_value, pinned_instance_id, replicas, updated_at
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
		var ov domain.PlacementOverride
		var pinned sql.NullString
		if err := rows.Scan(&ov.OrgID, &ov.KeyKind, &ov.KeyValue, &pinned, &ov.Replicas, &ov.UpdatedAt); err != nil {
			return nil, err
		}
		ov.PinnedInstanceID = pinned.String
		out = append(out, ov)
	}
	return out, rows.Err()
}

func (s *placementOverrideStore) Upsert(ctx context.Context, ov domain.PlacementOverride) error {
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO placement_overrides (org_id, key_kind, key_value, pinned_instance_id, replicas, updated_at)
		VALUES (?, ?, ?, NULLIF(?, ''), ?, ?)
		ON CONFLICT(org_id, key_kind, key_value) DO UPDATE SET
			pinned_instance_id = excluded.pinned_instance_id,
			replicas           = excluded.replicas,
			updated_at         = excluded.updated_at
	`, ov.OrgID, ov.KeyKind, ov.KeyValue, ov.PinnedInstanceID, ov.Replicas, time.Now().UTC())
	return err
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
