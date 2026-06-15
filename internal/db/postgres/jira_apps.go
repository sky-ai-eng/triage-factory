package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// jiraAppsStore holds two pools. app is the claims-checked request-handler path
// (GetForOrg / UpsertForOrg / DeleteForOrg — RLS gates reads by org membership,
// writes by org admin). admin is the no-claims read the OAuth-app resolver
// needs (GetForOrgSystem). Mirrors gitHubAppsStore, minus installations.
type jiraAppsStore struct {
	app   queryer
	admin queryer
}

func newJiraAppsStore(app, admin queryer) db.JiraAppsStore {
	return &jiraAppsStore{app: app, admin: admin}
}

var _ db.JiraAppsStore = (*jiraAppsStore)(nil)

func scanJiraApp(row interface{ Scan(...any) error }) (*domain.OrgJiraApp, error) {
	var (
		a            domain.OrgJiraApp
		regBy        sql.NullString
		registeredAt sql.NullTime
	)
	err := row.Scan(&a.OrgID, &a.ClientID, &a.ClientSecretRef, &registeredAt, &regBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get org_jira_apps: %w", err)
	}
	if registeredAt.Valid {
		a.RegisteredAt = registeredAt.Time
	}
	a.RegisteredByUserID = regBy.String
	return &a, nil
}

const selectJiraAppCols = `
	SELECT org_id, client_id, client_secret_ref, registered_at, registered_by_user_id
	  FROM org_jira_apps
	 WHERE org_id = $1`

func (s *jiraAppsStore) GetForOrg(ctx context.Context, orgID string) (*domain.OrgJiraApp, error) {
	if !isValidUUID(orgID) {
		return nil, nil
	}
	app, err := scanJiraApp(s.app.QueryRowContext(ctx, selectJiraAppCols, orgID))
	return app, wrapAppPoolPermErr(err, "jira_apps.GetForOrg")
}

func (s *jiraAppsStore) GetForOrgSystem(ctx context.Context, orgID string) (*domain.OrgJiraApp, error) {
	if !isValidUUID(orgID) {
		return nil, nil
	}
	return scanJiraApp(s.admin.QueryRowContext(ctx, selectJiraAppCols, orgID))
}

func (s *jiraAppsStore) UpsertForOrg(ctx context.Context, app domain.OrgJiraApp) error {
	// App pool: the org_jira_apps_insert / _update RLS policies gate this by
	// org admin, which is the claims context the settings handler runs under.
	// registered_at is preserved on conflict; the credentials + registrant are
	// refreshed so re-entering the BYO app replaces them in place.
	_, err := s.app.ExecContext(ctx, `
		INSERT INTO org_jira_apps (org_id, client_id, client_secret_ref, registered_by_user_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (org_id) DO UPDATE SET
			client_id             = EXCLUDED.client_id,
			client_secret_ref     = EXCLUDED.client_secret_ref,
			registered_by_user_id = EXCLUDED.registered_by_user_id
	`, app.OrgID, app.ClientID, app.ClientSecretRef, nullString(app.RegisteredByUserID))
	return wrapAppPoolPermErr(err, "jira_apps.UpsertForOrg")
}

func (s *jiraAppsStore) DeleteForOrg(ctx context.Context, orgID string) error {
	if !isValidUUID(orgID) {
		return nil
	}
	_, err := s.app.ExecContext(ctx, `
		DELETE FROM org_jira_apps WHERE org_id = $1
	`, orgID)
	return wrapAppPoolPermErr(err, "jira_apps.DeleteForOrg")
}
