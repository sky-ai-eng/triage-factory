package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// placementOverrideStore is the Postgres impl of db.PlacementOverrideStore —
// the placement pin/replica overrides (TFAC-587, spec §6.1). Wired against
// the ADMIN (BYPASSRLS) pool in postgres.New: placement_overrides is a
// system-only table (RLS deny-by-default, REVOKEd from the app roles, exactly
// like instances), read for an already-authorized, operator-gated orgID
// rather than under a per-request RLS context.
type placementOverrideStore struct{ admin queryer }

func newPlacementOverrideStore(admin queryer) db.PlacementOverrideStore {
	return &placementOverrideStore{admin: admin}
}

// NewPlacementOverrideStore builds a standalone store against a pooled
// *sql.DB — the narrow constructor the CLI verb uses (mirrors
// NewInstanceStore) instead of the full db.Stores bundle.
func NewPlacementOverrideStore(admin *sql.DB) db.PlacementOverrideStore {
	return newPlacementOverrideStore(admin)
}

var _ db.PlacementOverrideStore = (*placementOverrideStore)(nil)

func (s *placementOverrideStore) Get(ctx context.Context, orgID, keyKind, keyValue string) (*domain.PlacementOverride, error) {
	var ov domain.PlacementOverride
	err := s.admin.QueryRowContext(ctx, `
		SELECT org_id, key_kind, key_value, COALESCE(pinned_instance_id, ''), replicas, updated_at
		FROM placement_overrides
		WHERE org_id = $1 AND key_kind = $2 AND key_value = $3
	`, orgID, keyKind, keyValue).Scan(&ov.OrgID, &ov.KeyKind, &ov.KeyValue, &ov.PinnedInstanceID, &ov.Replicas, &ov.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, wrapAdminPoolPermErr(err, "placement_overrides.Get")
	}
	return &ov, nil
}

func (s *placementOverrideStore) List(ctx context.Context, orgID string) ([]domain.PlacementOverride, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT org_id, key_kind, key_value, COALESCE(pinned_instance_id, ''), replicas, updated_at
		FROM placement_overrides
		WHERE org_id = $1
		ORDER BY key_kind, key_value
	`, orgID)
	if err != nil {
		return nil, wrapAdminPoolPermErr(err, "placement_overrides.List")
	}
	defer rows.Close()
	var out []domain.PlacementOverride
	for rows.Next() {
		var ov domain.PlacementOverride
		if err := rows.Scan(&ov.OrgID, &ov.KeyKind, &ov.KeyValue, &ov.PinnedInstanceID, &ov.Replicas, &ov.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, ov)
	}
	return out, rows.Err()
}

func (s *placementOverrideStore) Upsert(ctx context.Context, ov domain.PlacementOverride) error {
	_, err := s.admin.ExecContext(ctx, `
		INSERT INTO placement_overrides (org_id, key_kind, key_value, pinned_instance_id, replicas, updated_at)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5, now())
		ON CONFLICT (org_id, key_kind, key_value) DO UPDATE SET
			pinned_instance_id = EXCLUDED.pinned_instance_id,
			replicas           = EXCLUDED.replicas,
			updated_at         = now()
	`, ov.OrgID, ov.KeyKind, ov.KeyValue, ov.PinnedInstanceID, ov.Replicas)
	return wrapAdminPoolPermErr(err, "placement_overrides.Upsert")
}

func (s *placementOverrideStore) Delete(ctx context.Context, orgID, keyKind, keyValue string) (bool, error) {
	res, err := s.admin.ExecContext(ctx, `
		DELETE FROM placement_overrides WHERE org_id = $1 AND key_kind = $2 AND key_value = $3
	`, orgID, keyKind, keyValue)
	if err != nil {
		return false, wrapAdminPoolPermErr(err, "placement_overrides.Delete")
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
