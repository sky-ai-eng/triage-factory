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

// curatorHomeColumns is the canonical projection of a curator_homes row, in
// the order scanCuratorHome reads them. Get SELECTs it and Upsert RETURNs it,
// so the write shape cannot drift from the read shape.
const curatorHomeColumns = `org_id, project_id, home_instance_id, home_boot_epoch, homed_at, updated_at`

func scanCuratorHome(scan func(...any) error) (domain.CuratorHome, error) {
	var h domain.CuratorHome
	err := scan(&h.OrgID, &h.ProjectID, &h.HomeInstanceID, &h.HomeBootEpoch, &h.HomedAt, &h.UpdatedAt)
	return h, err
}

func (s *curatorHomeStore) Get(ctx context.Context, orgID, projectID string) (*domain.CuratorHome, error) {
	h, err := scanCuratorHome(s.q.QueryRowContext(ctx, `
		SELECT `+curatorHomeColumns+`
		FROM curator_homes
		WHERE org_id = ? AND project_id = ?
	`, orgID, projectID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &h, nil
}

func (s *curatorHomeStore) Upsert(ctx context.Context, orgID, projectID, instanceID string, bootEpoch int64) (domain.CuratorHome, error) {
	now := time.Now().UTC()
	// RETURNING carries the homed_at the conflict arm preserved — the one
	// column this call cannot describe, because on a re-home it belongs to the
	// call that first homed the project.
	return scanCuratorHome(s.q.QueryRowContext(ctx, `
		INSERT INTO curator_homes (org_id, project_id, home_instance_id, home_boot_epoch, homed_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(org_id, project_id) DO UPDATE SET
			home_instance_id = excluded.home_instance_id,
			home_boot_epoch  = excluded.home_boot_epoch,
			updated_at       = excluded.updated_at
		RETURNING `+curatorHomeColumns,
		orgID, projectID, instanceID, bootEpoch, now, now).Scan)
}

func (s *curatorHomeStore) Clear(ctx context.Context, orgID, projectID string) error {
	_, err := s.q.ExecContext(ctx, `
		DELETE FROM curator_homes WHERE org_id = ? AND project_id = ?
	`, orgID, projectID)
	return err
}
