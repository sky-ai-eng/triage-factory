package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
const placementOverrideColumns = `org_id, repository_id, pinned_instance_id, replicas, updated_at`

func scanPlacementOverride(scan func(...any) error) (domain.PlacementOverride, error) {
	var ov domain.PlacementOverride
	var pinned sql.NullString
	if err := scan(&ov.OrgID, &ov.RepositoryID, &pinned, &ov.Replicas, &ov.UpdatedAt); err != nil {
		return ov, err
	}
	ov.PinnedInstanceID = pinned.String
	return ov, nil
}

func (s *placementOverrideStore) Get(ctx context.Context, orgID, repositoryID string) (*domain.PlacementOverride, error) {
	ov, err := scanPlacementOverride(s.q.QueryRowContext(ctx, `
		SELECT `+placementOverrideColumns+`
		FROM placement_overrides
		WHERE org_id = ? AND repository_id = ?
	`, orgID, repositoryID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ov, nil
}

// List orders by the repository's name, which the override does not store:
// the join reads it off the registry row for the ORDER BY alone, so the rows
// keep Get's projection.
func (s *placementOverrideStore) List(ctx context.Context, orgID string) ([]domain.PlacementOverride, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT p.org_id, p.repository_id, p.pinned_instance_id, p.replicas, p.updated_at
		FROM placement_overrides p
		JOIN repositories r ON r.id = p.repository_id
		WHERE p.org_id = ?
		ORDER BY r.host, LOWER(r.owner), LOWER(r.repo), p.repository_id
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

// Upsert maps a repository id no registry row in the org carries to
// db.ErrNoSuchRepository. The foreign key refuses an id no row carries; the
// guard in the SELECT refuses another org's row, which the foreign key, on
// the id alone, would admit.
func (s *placementOverrideStore) Upsert(ctx context.Context, ov domain.PlacementOverride) (domain.PlacementOverride, error) {
	stored, err := scanPlacementOverride(s.q.QueryRowContext(ctx, `
		INSERT INTO placement_overrides (org_id, repository_id, pinned_instance_id, replicas, updated_at)
		SELECT ?, ?, NULLIF(?, ''), ?, ?
		 WHERE EXISTS (SELECT 1 FROM repositories WHERE id = ? AND org_id = ?)
		ON CONFLICT(org_id, repository_id) DO UPDATE SET
			pinned_instance_id = excluded.pinned_instance_id,
			replicas           = excluded.replicas,
			updated_at         = excluded.updated_at
		RETURNING `+placementOverrideColumns,
		ov.OrgID, ov.RepositoryID, ov.PinnedInstanceID, ov.Replicas, time.Now().UTC(),
		ov.RepositoryID, ov.OrgID).Scan)
	if errors.Is(err, sql.ErrNoRows) || isForeignKeyViolation(err) {
		return domain.PlacementOverride{}, fmt.Errorf("%w: %s", db.ErrNoSuchRepository, ov.RepositoryID)
	}
	return stored, err
}

func (s *placementOverrideStore) Delete(ctx context.Context, orgID, repositoryID string) (bool, error) {
	res, err := s.q.ExecContext(ctx, `
		DELETE FROM placement_overrides WHERE org_id = ? AND repository_id = ?
	`, orgID, repositoryID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
