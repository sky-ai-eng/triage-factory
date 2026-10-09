package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// linearAppsStore is the SQLite impl of db.LinearAppsStore. One connection, no
// RLS, so GetForOrgSystem == GetForOrg.
type linearAppsStore struct {
	q queryer
}

func newLinearAppsStore(q queryer) db.LinearAppsStore {
	return &linearAppsStore{q: q}
}

var _ db.LinearAppsStore = (*linearAppsStore)(nil)

// sqliteLinearAppColumns is the projection of an org_linear_apps row, in the
// order scanSQLiteLinearApp reads it. GetForOrg SELECTs it and UpsertForOrg
// RETURNs it, so the write shape cannot drift from the read shape.
const sqliteLinearAppColumns = `org_id, client_id, client_secret_ref, registered_at, registered_by_user_id`

func scanSQLiteLinearApp(scan func(...any) error) (domain.OrgLinearApp, error) {
	var (
		a     domain.OrgLinearApp
		regBy sql.NullString
	)
	if err := scan(&a.OrgID, &a.ClientID, &a.ClientSecretRef, &a.RegisteredAt, &regBy); err != nil {
		return a, err
	}
	a.RegisteredByUserID = regBy.String
	return a, nil
}

func (s *linearAppsStore) GetForOrg(ctx context.Context, orgID string) (*domain.OrgLinearApp, error) {
	a, err := scanSQLiteLinearApp(s.q.QueryRowContext(ctx, `
		SELECT `+sqliteLinearAppColumns+`
		  FROM org_linear_apps
		 WHERE org_id = ?
	`, orgID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get org_linear_apps: %w", err)
	}
	return &a, nil
}

func (s *linearAppsStore) GetForOrgSystem(ctx context.Context, orgID string) (*domain.OrgLinearApp, error) {
	return s.GetForOrg(ctx, orgID)
}

func (s *linearAppsStore) UpsertForOrg(ctx context.Context, app domain.OrgLinearApp) (domain.OrgLinearApp, error) {
	stored, err := scanSQLiteLinearApp(s.q.QueryRowContext(ctx, `
		INSERT INTO org_linear_apps (org_id, client_id, client_secret_ref, registered_by_user_id)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(org_id) DO UPDATE SET
			client_id             = excluded.client_id,
			client_secret_ref     = excluded.client_secret_ref,
			registered_by_user_id = excluded.registered_by_user_id
		RETURNING `+sqliteLinearAppColumns,
		app.OrgID, app.ClientID, app.ClientSecretRef, nullStringValue(app.RegisteredByUserID)).Scan)
	if err != nil {
		return domain.OrgLinearApp{}, fmt.Errorf("upsert org_linear_apps: %w", err)
	}
	return stored, nil
}

func (s *linearAppsStore) DeleteForOrg(ctx context.Context, orgID string) error {
	if _, err := s.q.ExecContext(ctx, `DELETE FROM org_linear_apps WHERE org_id = ?`, orgID); err != nil {
		return fmt.Errorf("delete org_linear_apps: %w", err)
	}
	return nil
}

// linearInstallsStore is the SQLite impl of db.LinearInstallsStore. Local mode
// has one connection and no admin pool, so the ...System methods run on
// whatever q is: the connection, or the transaction the store is bound to.
type linearInstallsStore struct {
	q queryer
}

func newLinearInstallsStore(q queryer) db.LinearInstallsStore {
	return &linearInstallsStore{q: q}
}

var _ db.LinearInstallsStore = (*linearInstallsStore)(nil)

// sqliteLinearInstallColumns is the projection of an org_linear_installs row,
// in the order scanSQLiteLinearInstall reads it.
const sqliteLinearInstallColumns = `org_id, install_id, workspace_id, workspace_url_key, app_user_id, app_client_id,
	installed_by_user_id, installed_at, removed_at, removed_reason`

func scanSQLiteLinearInstall(scan func(...any) error) (domain.OrgLinearInstall, error) {
	var (
		inst      domain.OrgLinearInstall
		installBy sql.NullString
		removedAt sql.NullTime
		reason    sql.NullString
	)
	if err := scan(&inst.OrgID, &inst.InstallID, &inst.WorkspaceID, &inst.WorkspaceURLKey, &inst.AppUserID, &inst.AppClientID,
		&installBy, &inst.InstalledAt, &removedAt, &reason); err != nil {
		return domain.OrgLinearInstall{}, err
	}
	inst.InstalledByUserID = installBy.String
	inst.RemovedAt = removedAt.Time
	inst.RemovedReason = reason.String
	return inst, nil
}

func (s *linearInstallsStore) GetForOrgSystem(ctx context.Context, orgID string) (*domain.OrgLinearInstall, error) {
	inst, err := scanSQLiteLinearInstall(s.q.QueryRowContext(ctx, `
		SELECT `+sqliteLinearInstallColumns+`
		  FROM org_linear_installs
		 WHERE org_id = ?
	`, orgID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get org_linear_installs: %w", err)
	}
	return &inst, nil
}

func (s *linearInstallsStore) UpsertSystem(ctx context.Context, inst domain.OrgLinearInstall) (domain.OrgLinearInstall, error) {
	stored, err := scanSQLiteLinearInstall(s.q.QueryRowContext(ctx, `
		INSERT INTO org_linear_installs
			(org_id, install_id, workspace_id, workspace_url_key, app_user_id, app_client_id,
			 installed_by_user_id, installed_at, removed_at, removed_reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, COALESCE(?, CURRENT_TIMESTAMP), NULL, NULL)
		ON CONFLICT(org_id) DO UPDATE SET
			install_id           = excluded.install_id,
			workspace_id         = excluded.workspace_id,
			workspace_url_key    = excluded.workspace_url_key,
			app_user_id          = excluded.app_user_id,
			app_client_id        = excluded.app_client_id,
			installed_by_user_id = excluded.installed_by_user_id,
			installed_at         = excluded.installed_at,
			removed_at           = NULL,
			removed_reason       = NULL
		RETURNING `+sqliteLinearInstallColumns,
		inst.OrgID, inst.InstallID, inst.WorkspaceID, inst.WorkspaceURLKey, inst.AppUserID, inst.AppClientID,
		nullStringValue(inst.InstalledByUserID), nullTimeValue(inst.InstalledAt)).Scan)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: org_linear_installs.workspace_id") {
			return domain.OrgLinearInstall{}, db.ErrWorkspaceInstalledElsewhere
		}
		return domain.OrgLinearInstall{}, fmt.Errorf("upsert org_linear_installs: %w", err)
	}
	return stored, nil
}

func (s *linearInstallsStore) MarkRemovedSystem(ctx context.Context, orgID, installID, reason string) (*domain.OrgLinearInstall, error) {
	inst, err := scanSQLiteLinearInstall(s.q.QueryRowContext(ctx, `
		UPDATE org_linear_installs
		   SET removed_at = CURRENT_TIMESTAMP, removed_reason = ?
		 WHERE org_id = ? AND install_id = ? AND removed_at IS NULL
		RETURNING `+sqliteLinearInstallColumns,
		reason, orgID, installID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("mark org_linear_installs removed: %w", err)
	}
	return &inst, nil
}
