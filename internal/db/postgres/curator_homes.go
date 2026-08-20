package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// curatorHomeStore is the Postgres impl of db.CuratorHomeStore — the
// (org, project) -> home executor map for curator homing (spec §6.3). Wired
// against the ADMIN (BYPASSRLS) pool in postgres.New: curator_homes is a
// system-only table (RLS deny-by-default, REVOKEd from the app roles, exactly
// like instances / placement_overrides), read and written for an explicit
// orgID rather than under a per-request RLS context.
type curatorHomeStore struct{ admin queryer }

func newCuratorHomeStore(admin queryer) db.CuratorHomeStore {
	return &curatorHomeStore{admin: admin}
}

// NewCuratorHomeStore builds a standalone store against a pooled *sql.DB — the
// admin pool — mirroring NewPlacementOverrideStore (used by the conformance
// suite).
func NewCuratorHomeStore(admin *sql.DB) db.CuratorHomeStore {
	return newCuratorHomeStore(admin)
}

var _ db.CuratorHomeStore = (*curatorHomeStore)(nil)

// pgCuratorHomeColumns is the canonical projection of a curator_homes row, in
// the order scanCuratorHome reads them. Get SELECTs it and Upsert RETURNs it,
// so the write shape cannot drift from the read shape.
const pgCuratorHomeColumns = `org_id, project_id, home_instance_id, home_boot_epoch, homed_at, updated_at`

func scanCuratorHome(scan func(...any) error) (domain.CuratorHome, error) {
	var h domain.CuratorHome
	err := scan(&h.OrgID, &h.ProjectID, &h.HomeInstanceID, &h.HomeBootEpoch, &h.HomedAt, &h.UpdatedAt)
	return h, err
}

func (s *curatorHomeStore) Get(ctx context.Context, orgID, projectID string) (*domain.CuratorHome, error) {
	h, err := scanCuratorHome(s.admin.QueryRowContext(ctx, `
		SELECT `+pgCuratorHomeColumns+`
		FROM curator_homes
		WHERE org_id = $1 AND project_id = $2
	`, orgID, projectID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, wrapAdminPoolPermErr(err, "curator_homes.Get")
	}
	return &h, nil
}

func (s *curatorHomeStore) Upsert(ctx context.Context, orgID, projectID, instanceID string, bootEpoch int64) (domain.CuratorHome, error) {
	// RETURNING carries the homed_at the conflict arm preserved — the one
	// column this call cannot describe, because on a re-home it belongs to the
	// call that first homed the project.
	h, err := scanCuratorHome(s.admin.QueryRowContext(ctx, `
		INSERT INTO curator_homes (org_id, project_id, home_instance_id, home_boot_epoch, homed_at, updated_at)
		VALUES ($1, $2, $3, $4, now(), now())
		ON CONFLICT (org_id, project_id) DO UPDATE SET
			home_instance_id = EXCLUDED.home_instance_id,
			home_boot_epoch  = EXCLUDED.home_boot_epoch,
			updated_at       = now()
		RETURNING `+pgCuratorHomeColumns,
		orgID, projectID, instanceID, bootEpoch).Scan)
	if err != nil {
		return domain.CuratorHome{}, wrapAdminPoolPermErr(err, "curator_homes.Upsert")
	}
	return h, nil
}

func (s *curatorHomeStore) Clear(ctx context.Context, orgID, projectID string) error {
	_, err := s.admin.ExecContext(ctx, `
		DELETE FROM curator_homes WHERE org_id = $1 AND project_id = $2
	`, orgID, projectID)
	return wrapAdminPoolPermErr(err, "curator_homes.Clear")
}
