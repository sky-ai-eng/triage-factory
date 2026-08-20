package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// placementOverrideStore is the Postgres impl of db.PlacementOverrideStore —
// the placement pin/replica overrides (spec §6.1). Wired against
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

// pgPlacementOverrideColumns is the canonical projection of a
// placement_overrides row, in the order scanPlacementOverride reads them. Both
// reads SELECT it and Upsert RETURNs it, so the write shape cannot drift from
// the read shape. COALESCE keeps a NULL pin scanning as the empty string the
// domain type carries.
const pgPlacementOverrideColumns = `org_id, key_kind, key_value, COALESCE(pinned_instance_id, ''), replicas, updated_at`

func scanPlacementOverride(scan func(...any) error) (domain.PlacementOverride, error) {
	var ov domain.PlacementOverride
	err := scan(&ov.OrgID, &ov.KeyKind, &ov.KeyValue, &ov.PinnedInstanceID, &ov.Replicas, &ov.UpdatedAt)
	return ov, err
}

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
		SELECT `+pgPlacementOverrideColumns+`
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
		ov, err := scanPlacementOverride(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, ov)
	}
	return out, rows.Err()
}

func (s *placementOverrideStore) Upsert(ctx context.Context, ov domain.PlacementOverride) (domain.PlacementOverride, error) {
	stored, err := scanPlacementOverride(s.admin.QueryRowContext(ctx, `
		INSERT INTO placement_overrides (org_id, key_kind, key_value, pinned_instance_id, replicas, updated_at)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5, now())
		ON CONFLICT (org_id, key_kind, key_value) DO UPDATE SET
			pinned_instance_id = EXCLUDED.pinned_instance_id,
			replicas           = EXCLUDED.replicas,
			updated_at         = now()
		RETURNING `+pgPlacementOverrideColumns,
		ov.OrgID, ov.KeyKind, ov.KeyValue, ov.PinnedInstanceID, ov.Replicas).Scan)
	if err != nil {
		return domain.PlacementOverride{}, wrapAdminPoolPermErr(err, "placement_overrides.Upsert")
	}
	return stored, nil
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
