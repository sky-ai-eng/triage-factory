package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// curatorHomeStore is the SQLite impl of db.CuratorHomeStore — the
// (org, project) -> home executor map for curator homing (spec §6.3). SQLite is
// N=1: the one process is always its own home, so the row is inert, but the
// store exists for interface + conformance symmetry across dialects.
type curatorHomeStore struct{ q queryer }

func newCuratorHomeStore(q queryer) db.CuratorHomeStore {
	return &curatorHomeStore{q: q}
}

// NewCuratorHomeStore builds a standalone store against a pooled *sql.DB —
// mirrors NewPlacementOverrideStore (used by the conformance suite).
func NewCuratorHomeStore(conn *sql.DB) db.CuratorHomeStore {
	return newCuratorHomeStore(conn)
}

var _ db.CuratorHomeStore = (*curatorHomeStore)(nil)

func (s *curatorHomeStore) Get(ctx context.Context, orgID, projectID string) (*domain.CuratorHome, error) {
	var h domain.CuratorHome
	err := s.q.QueryRowContext(ctx, `
		SELECT org_id, project_id, home_instance_id, home_boot_epoch, homed_at, updated_at
		FROM curator_homes
		WHERE org_id = ? AND project_id = ?
	`, orgID, projectID).Scan(&h.OrgID, &h.ProjectID, &h.HomeInstanceID, &h.HomeBootEpoch, &h.HomedAt, &h.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &h, nil
}

func (s *curatorHomeStore) Upsert(ctx context.Context, orgID, projectID, instanceID string, bootEpoch int64) error {
	now := time.Now().UTC()
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO curator_homes (org_id, project_id, home_instance_id, home_boot_epoch, homed_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(org_id, project_id) DO UPDATE SET
			home_instance_id = excluded.home_instance_id,
			home_boot_epoch  = excluded.home_boot_epoch,
			updated_at       = excluded.updated_at
	`, orgID, projectID, instanceID, bootEpoch, now, now)
	return err
}

func (s *curatorHomeStore) Clear(ctx context.Context, orgID, projectID string) error {
	_, err := s.q.ExecContext(ctx, `
		DELETE FROM curator_homes WHERE org_id = ? AND project_id = ?
	`, orgID, projectID)
	return err
}
