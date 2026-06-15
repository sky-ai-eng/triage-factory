package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// jiraAppsStore is the SQLite (local-mode) impl of db.JiraAppsStore. One
// connection — no RLS, no pool split — so GetForOrgSystem == GetForOrg.
type jiraAppsStore struct {
	q queryer
}

func newJiraAppsStore(q queryer) db.JiraAppsStore {
	return &jiraAppsStore{q: q}
}

var _ db.JiraAppsStore = (*jiraAppsStore)(nil)

func (s *jiraAppsStore) GetForOrg(ctx context.Context, orgID string) (*domain.OrgJiraApp, error) {
	var (
		a     domain.OrgJiraApp
		regBy sql.NullString
	)
	err := s.q.QueryRowContext(ctx, `
		SELECT org_id, client_id, client_secret_ref, registered_at, registered_by_user_id
		  FROM org_jira_apps
		 WHERE org_id = ?
	`, orgID).Scan(
		&a.OrgID, &a.ClientID, &a.ClientSecretRef, &a.RegisteredAt, &regBy,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get org_jira_apps: %w", err)
	}
	a.RegisteredByUserID = regBy.String
	return &a, nil
}

// GetForOrgSystem is identical to GetForOrg in local mode — single conn, no
// RLS — but exists so the resolver uses the same interface shape it would in
// multi mode.
func (s *jiraAppsStore) GetForOrgSystem(ctx context.Context, orgID string) (*domain.OrgJiraApp, error) {
	return s.GetForOrg(ctx, orgID)
}

func (s *jiraAppsStore) UpsertForOrg(ctx context.Context, app domain.OrgJiraApp) error {
	// registered_at is set only on insert (defaulting to CURRENT_TIMESTAMP) and
	// preserved on conflict; client_id, client_secret_ref, and the registrant
	// are refreshed so re-entering the BYO app replaces the credentials in place.
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO org_jira_apps (org_id, client_id, client_secret_ref, registered_by_user_id)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(org_id) DO UPDATE SET
			client_id             = excluded.client_id,
			client_secret_ref     = excluded.client_secret_ref,
			registered_by_user_id = excluded.registered_by_user_id
	`, app.OrgID, app.ClientID, app.ClientSecretRef, nullStringValue(app.RegisteredByUserID))
	if err != nil {
		return fmt.Errorf("upsert org_jira_apps: %w", err)
	}
	return nil
}

func (s *jiraAppsStore) DeleteForOrg(ctx context.Context, orgID string) error {
	if _, err := s.q.ExecContext(ctx, `
		DELETE FROM org_jira_apps WHERE org_id = ?
	`, orgID); err != nil {
		return fmt.Errorf("delete org_jira_apps: %w", err)
	}
	return nil
}
