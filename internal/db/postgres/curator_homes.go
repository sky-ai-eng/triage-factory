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

func (s *curatorHomeStore) Get(ctx context.Context, orgID, projectID string) (*domain.CuratorHome, error) {
	var h domain.CuratorHome
	err := s.admin.QueryRowContext(ctx, `
		SELECT org_id, project_id, home_instance_id, home_boot_epoch, homed_at, updated_at
		FROM curator_homes
		WHERE org_id = $1 AND project_id = $2
	`, orgID, projectID).Scan(&h.OrgID, &h.ProjectID, &h.HomeInstanceID, &h.HomeBootEpoch, &h.HomedAt, &h.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, wrapAdminPoolPermErr(err, "curator_homes.Get")
	}
	return &h, nil
}

func (s *curatorHomeStore) Upsert(ctx context.Context, orgID, projectID, instanceID string, bootEpoch int64) error {
	_, err := s.admin.ExecContext(ctx, `
		INSERT INTO curator_homes (org_id, project_id, home_instance_id, home_boot_epoch, homed_at, updated_at)
		VALUES ($1, $2, $3, $4, now(), now())
		ON CONFLICT (org_id, project_id) DO UPDATE SET
			home_instance_id = EXCLUDED.home_instance_id,
			home_boot_epoch  = EXCLUDED.home_boot_epoch,
			updated_at       = now()
	`, orgID, projectID, instanceID, bootEpoch)
	return wrapAdminPoolPermErr(err, "curator_homes.Upsert")
}

func (s *curatorHomeStore) Clear(ctx context.Context, orgID, projectID string) error {
	_, err := s.admin.ExecContext(ctx, `
		DELETE FROM curator_homes WHERE org_id = $1 AND project_id = $2
	`, orgID, projectID)
	return wrapAdminPoolPermErr(err, "curator_homes.Clear")
}
