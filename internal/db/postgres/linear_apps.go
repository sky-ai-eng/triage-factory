package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// linearAppsStore holds two pools, the jiraAppsStore split: app for the
// claims-checked request-handler path (RLS gates reads by org membership,
// writes by org admin), admin for the OAuth-app resolver's GetForOrgSystem.
type linearAppsStore struct {
	app   queryer
	admin queryer
}

func newLinearAppsStore(app, admin queryer) db.LinearAppsStore {
	return &linearAppsStore{app: app, admin: admin}
}

var _ db.LinearAppsStore = (*linearAppsStore)(nil)

// pgLinearAppColumns is the projection of an org_linear_apps row, in the order
// scanLinearApp reads it. Both reads SELECT it and UpsertForOrg RETURNs it.
const pgLinearAppColumns = `org_id, client_id, client_secret_ref, registered_at, registered_by_user_id`

const selectLinearAppCols = `
	SELECT ` + pgLinearAppColumns + `
	  FROM org_linear_apps
	 WHERE org_id = $1`

func scanLinearApp(row interface{ Scan(...any) error }) (*domain.OrgLinearApp, error) {
	var (
		a     domain.OrgLinearApp
		regBy sql.NullString
	)
	err := row.Scan(&a.OrgID, &a.ClientID, &a.ClientSecretRef, &a.RegisteredAt, &regBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get org_linear_apps: %w", err)
	}
	a.RegisteredByUserID = regBy.String
	return &a, nil
}

func (s *linearAppsStore) GetForOrg(ctx context.Context, orgID string) (*domain.OrgLinearApp, error) {
	if !isValidUUID(orgID) {
		return nil, nil
	}
	app, err := scanLinearApp(s.app.QueryRowContext(ctx, selectLinearAppCols, orgID))
	return app, wrapAppPoolPermErr(err, "linear_apps.GetForOrg")
}

func (s *linearAppsStore) GetForOrgSystem(ctx context.Context, orgID string) (*domain.OrgLinearApp, error) {
	if !isValidUUID(orgID) {
		return nil, nil
	}
	return scanLinearApp(s.admin.QueryRowContext(ctx, selectLinearAppCols, orgID))
}

func (s *linearAppsStore) UpsertForOrg(ctx context.Context, app domain.OrgLinearApp) (domain.OrgLinearApp, error) {
	stored, err := scanLinearApp(s.app.QueryRowContext(ctx, `
		INSERT INTO org_linear_apps (org_id, client_id, client_secret_ref, registered_by_user_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (org_id) DO UPDATE SET
			client_id             = EXCLUDED.client_id,
			client_secret_ref     = EXCLUDED.client_secret_ref,
			registered_by_user_id = EXCLUDED.registered_by_user_id
		RETURNING `+pgLinearAppColumns,
		app.OrgID, app.ClientID, app.ClientSecretRef, nullString(app.RegisteredByUserID)))
	if err != nil {
		return domain.OrgLinearApp{}, wrapAppPoolPermErr(err, "linear_apps.UpsertForOrg")
	}
	if stored == nil {
		return domain.OrgLinearApp{}, sql.ErrNoRows
	}
	return *stored, nil
}

func (s *linearAppsStore) DeleteForOrg(ctx context.Context, orgID string) error {
	if !isValidUUID(orgID) {
		return nil
	}
	_, err := s.app.ExecContext(ctx, `DELETE FROM org_linear_apps WHERE org_id = $1`, orgID)
	return wrapAppPoolPermErr(err, "linear_apps.DeleteForOrg")
}

// linearInstallsStore runs every method on the admin pool: RLS refuses tf_app
// every write to org_linear_installs, and the uniqueness the upsert enforces
// spans orgs.
type linearInstallsStore struct {
	admin queryer
}

func newLinearInstallsStore(admin queryer) db.LinearInstallsStore {
	return &linearInstallsStore{admin: admin}
}

var _ db.LinearInstallsStore = (*linearInstallsStore)(nil)

// pgLinearInstallColumns is the projection of an org_linear_installs row, in
// the order scanLinearInstall reads it.
const pgLinearInstallColumns = `org_id, install_id, workspace_id, workspace_url_key, app_user_id, app_client_id,
	installed_by_user_id, installed_at, removed_at, removed_reason`

// linearInstallsWorkspaceLiveIndex is the partial unique index holding a live
// workspace to one org.
const linearInstallsWorkspaceLiveIndex = "org_linear_installs_workspace_live"

func scanLinearInstall(row interface{ Scan(...any) error }) (*domain.OrgLinearInstall, error) {
	var (
		inst      domain.OrgLinearInstall
		installBy sql.NullString
		removedAt sql.NullTime
		reason    sql.NullString
	)
	err := row.Scan(&inst.OrgID, &inst.InstallID, &inst.WorkspaceID, &inst.WorkspaceURLKey, &inst.AppUserID, &inst.AppClientID,
		&installBy, &inst.InstalledAt, &removedAt, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	inst.InstalledByUserID = installBy.String
	inst.RemovedAt = removedAt.Time
	inst.RemovedReason = reason.String
	return &inst, nil
}

func (s *linearInstallsStore) GetForOrgSystem(ctx context.Context, orgID string) (*domain.OrgLinearInstall, error) {
	if !isValidUUID(orgID) {
		return nil, nil
	}
	inst, err := scanLinearInstall(s.admin.QueryRowContext(ctx, `
		SELECT `+pgLinearInstallColumns+`
		  FROM org_linear_installs
		 WHERE org_id = $1
	`, orgID))
	if err != nil {
		return nil, fmt.Errorf("get org_linear_installs: %w", err)
	}
	return inst, nil
}

func (s *linearInstallsStore) UpsertSystem(ctx context.Context, inst domain.OrgLinearInstall) (domain.OrgLinearInstall, error) {
	stored, err := scanLinearInstall(s.admin.QueryRowContext(ctx, `
		INSERT INTO org_linear_installs
			(org_id, install_id, workspace_id, workspace_url_key, app_user_id, app_client_id,
			 installed_by_user_id, installed_at, removed_at, removed_reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7, COALESCE($8, now()), NULL, NULL)
		ON CONFLICT (org_id) DO UPDATE SET
			install_id           = EXCLUDED.install_id,
			workspace_id         = EXCLUDED.workspace_id,
			workspace_url_key    = EXCLUDED.workspace_url_key,
			app_user_id          = EXCLUDED.app_user_id,
			app_client_id        = EXCLUDED.app_client_id,
			installed_by_user_id = EXCLUDED.installed_by_user_id,
			installed_at         = EXCLUDED.installed_at,
			removed_at           = NULL,
			removed_reason       = NULL
		RETURNING `+pgLinearInstallColumns,
		inst.OrgID, inst.InstallID, inst.WorkspaceID, inst.WorkspaceURLKey, inst.AppUserID, inst.AppClientID,
		nullString(inst.InstalledByUserID), nullTime(inst.InstalledAt)))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == linearInstallsWorkspaceLiveIndex {
		return domain.OrgLinearInstall{}, db.ErrWorkspaceInstalledElsewhere
	}
	if err != nil {
		return domain.OrgLinearInstall{}, fmt.Errorf("upsert org_linear_installs: %w", err)
	}
	if stored == nil {
		return domain.OrgLinearInstall{}, sql.ErrNoRows
	}
	return *stored, nil
}

func (s *linearInstallsStore) MarkRemovedSystem(ctx context.Context, orgID, installID, reason string) (*domain.OrgLinearInstall, error) {
	if !isValidUUID(orgID) {
		return nil, nil
	}
	inst, err := scanLinearInstall(s.admin.QueryRowContext(ctx, `
		UPDATE org_linear_installs
		   SET removed_at = now(), removed_reason = $3
		 WHERE org_id = $1 AND install_id = $2 AND removed_at IS NULL
		RETURNING `+pgLinearInstallColumns,
		orgID, installID, reason))
	if err != nil {
		return nil, fmt.Errorf("mark org_linear_installs removed: %w", err)
	}
	return inst, nil
}
