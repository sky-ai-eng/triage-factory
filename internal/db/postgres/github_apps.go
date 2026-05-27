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

type gitHubAppsStore struct{ q queryer }

func newGitHubAppsStore(q queryer) db.GitHubAppsStore {
	return &gitHubAppsStore{q: q}
}

var _ db.GitHubAppsStore = (*gitHubAppsStore)(nil)

func (s *gitHubAppsStore) GetForOrg(ctx context.Context, orgID string) (*domain.OrgGitHubApp, error) {
	if !isValidUUID(orgID) {
		return nil, nil
	}
	var (
		a          domain.OrgGitHubApp
		regBy      sql.NullString
		registedAt sql.NullTime
	)
	err := s.q.QueryRowContext(ctx, `
		SELECT org_id, app_id, slug, client_id,
		       client_secret_ref, pem_ref, webhook_secret_ref,
		       registered_at, registered_by_user_id, active
		  FROM org_github_apps
		 WHERE org_id = $1
	`, orgID).Scan(
		&a.OrgID, &a.AppID, &a.Slug, &a.ClientID,
		&a.ClientSecretRef, &a.PEMRef, &a.WebhookSecretRef,
		&registedAt, &regBy, &a.Active,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get org_github_apps: %w", err)
	}
	if registedAt.Valid {
		a.RegisteredAt = registedAt.Time
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
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`,
		app.OrgID, app.AppID, app.Slug, app.ClientID,
		app.ClientSecretRef, app.PEMRef, app.WebhookSecretRef,
		nullString(app.RegisteredByUserID),
	)
	var pgErr *pgconn.PgError
	if err != nil && errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return &db.ErrGitHubAppExists{OrgID: app.OrgID}
	}
	if err != nil {
		return fmt.Errorf("insert org_github_apps: %w", err)
	}
	return nil
}
