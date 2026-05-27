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

type gitHubAppsStore struct{ q queryer }

func newGitHubAppsStore(q queryer) db.GitHubAppsStore { return &gitHubAppsStore{q: q} }

var _ db.GitHubAppsStore = (*gitHubAppsStore)(nil)

func (s *gitHubAppsStore) GetForOrg(ctx context.Context, orgID string) (*domain.OrgGitHubApp, error) {
	var (
		a     domain.OrgGitHubApp
		regBy sql.NullString
	)
	err := s.q.QueryRowContext(ctx, `
		SELECT org_id, app_id, slug, client_id,
		       client_secret_ref, pem_ref, webhook_secret_ref,
		       registered_at, registered_by_user_id, active
		  FROM org_github_apps
		 WHERE org_id = ?
	`, orgID).Scan(
		&a.OrgID, &a.AppID, &a.Slug, &a.ClientID,
		&a.ClientSecretRef, &a.PEMRef, &a.WebhookSecretRef,
		&a.RegisteredAt, &regBy, &a.Active,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get org_github_apps: %w", err)
	}
	a.RegisteredByUserID = regBy.String
	return &a, nil
}

func (s *gitHubAppsStore) CreateForOrg(ctx context.Context, app domain.OrgGitHubApp) error {
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO org_github_apps (
			org_id, app_id, slug, client_id,
			client_secret_ref, pem_ref, webhook_secret_ref,
			registered_by_user_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`,
		app.OrgID, app.AppID, app.Slug, app.ClientID,
		app.ClientSecretRef, app.PEMRef, app.WebhookSecretRef,
		nullStringValue(app.RegisteredByUserID),
	)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return &db.ErrGitHubAppExists{OrgID: app.OrgID}
	}
	if err != nil {
		return fmt.Errorf("insert org_github_apps: %w", err)
	}
	return nil
}
