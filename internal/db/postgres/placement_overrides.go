package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

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
const pgPlacementOverrideColumns = `org_id::text, repository_id::text, COALESCE(pinned_instance_id, ''), replicas, updated_at`

func scanPlacementOverride(scan func(...any) error) (domain.PlacementOverride, error) {
	var ov domain.PlacementOverride
	err := scan(&ov.OrgID, &ov.RepositoryID, &ov.PinnedInstanceID, &ov.Replicas, &ov.UpdatedAt)
	return ov, err
}

// Get answers a non-uuid org or repository id with no override, the read
// convention in uuid.go: no row can carry one.
func (s *placementOverrideStore) Get(ctx context.Context, orgID, repositoryID string) (*domain.PlacementOverride, error) {
	if !isValidUUID(orgID) || !isValidUUID(repositoryID) {
		return nil, nil
	}
	ov, err := scanPlacementOverride(s.admin.QueryRowContext(ctx, `
		SELECT `+pgPlacementOverrideColumns+`
		FROM placement_overrides
		WHERE org_id = $1 AND repository_id = $2
	`, orgID, repositoryID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, wrapAdminPoolPermErr(err, "placement_overrides.Get")
	}
	return &ov, nil
}

// List orders by the repository's name, which the override does not store:
// the join reads it off the registry row for the ORDER BY alone, so the rows
// keep Get's projection.
func (s *placementOverrideStore) List(ctx context.Context, orgID string) ([]domain.PlacementOverride, error) {
	if !isValidUUID(orgID) {
		return nil, nil
	}
	rows, err := s.admin.QueryContext(ctx, `
		SELECT `+qualifiedPlacementOverrideColumns+`
		FROM placement_overrides p
		JOIN repositories r ON r.id = p.repository_id AND r.org_id = p.org_id
		WHERE p.org_id = $1
		ORDER BY r.host, lower(r.owner), lower(r.repo), p.repository_id
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

// qualifiedPlacementOverrideColumns is pgPlacementOverrideColumns qualified by
// List's alias, where repositories shares the org_id and updated_at names.
const qualifiedPlacementOverrideColumns = `p.org_id::text, p.repository_id::text, COALESCE(p.pinned_instance_id, ''), p.replicas, p.updated_at`

// Upsert maps a repository id no registry row in the org carries — not a
// uuid, or refused by the foreign key — to db.ErrNoSuchRepository.
func (s *placementOverrideStore) Upsert(ctx context.Context, ov domain.PlacementOverride) (domain.PlacementOverride, error) {
	if err := validRepoID(ov.RepositoryID); err != nil {
		return domain.PlacementOverride{}, err
	}
	stored, err := scanPlacementOverride(s.admin.QueryRowContext(ctx, `
		INSERT INTO placement_overrides (org_id, repository_id, pinned_instance_id, replicas, updated_at)
		VALUES ($1, $2, NULLIF($3, ''), $4, now())
		ON CONFLICT (org_id, repository_id) DO UPDATE SET
			pinned_instance_id = EXCLUDED.pinned_instance_id,
			replicas           = EXCLUDED.replicas,
			updated_at         = now()
		RETURNING `+pgPlacementOverrideColumns,
		ov.OrgID, ov.RepositoryID, ov.PinnedInstanceID, ov.Replicas).Scan)
	if isForeignKeyViolation(err) {
		return domain.PlacementOverride{}, fmt.Errorf("%w: %s", db.ErrNoSuchRepository, ov.RepositoryID)
	}
	if err != nil {
		return domain.PlacementOverride{}, wrapAdminPoolPermErr(err, "placement_overrides.Upsert")
	}
	return stored, nil
}

func (s *placementOverrideStore) Delete(ctx context.Context, orgID, repositoryID string) (bool, error) {
	if !isValidUUID(orgID) || !isValidUUID(repositoryID) {
		return false, nil
	}
	res, err := s.admin.ExecContext(ctx, `
		DELETE FROM placement_overrides WHERE org_id = $1 AND repository_id = $2
	`, orgID, repositoryID)
	if err != nil {
		return false, wrapAdminPoolPermErr(err, "placement_overrides.Delete")
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
