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

// gitHubAppsStore is the SQLite (local-mode) impl. One connection — no RLS,
// no pool split — so the admin/app distinction the Postgres impl draws
// collapses here; GetForOrgSystem == GetForOrg and installation writes go to
// the same conn. secrets reads the App PEM for backfill (the keychain has no
// claims, so GetSystem == Get locally too).
type gitHubAppsStore struct {
	q       queryer
	secrets db.SecretStore
}

func newGitHubAppsStore(q queryer, secrets db.SecretStore) db.GitHubAppsStore {
	return &gitHubAppsStore{q: q, secrets: secrets}
}

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

// GetForOrgSystem is identical to GetForOrg in local mode — single conn,
// no RLS, no claims — but exists so system callers (webhook handler,
// backfill) use the same interface shape they would in multi mode.
func (s *gitHubAppsStore) GetForOrgSystem(ctx context.Context, orgID string) (*domain.OrgGitHubApp, error) {
	return s.GetForOrg(ctx, orgID)
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

func (s *gitHubAppsStore) ListInstallationsForOrg(ctx context.Context, orgID string) ([]domain.OrgGitHubAppInstallation, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT installation_id, org_id, account_type, account_login, installed_at
		  FROM org_github_app_installations
		 WHERE org_id = ? AND removed_at IS NULL
		 ORDER BY account_login
	`, orgID)
	if err != nil {
		return nil, fmt.Errorf("list org_github_app_installations: %w", err)
	}
	defer rows.Close()

	out := make([]domain.OrgGitHubAppInstallation, 0)
	for rows.Next() {
		var inst domain.OrgGitHubAppInstallation
		if err := rows.Scan(
			&inst.InstallationID, &inst.OrgID, &inst.AccountType,
			&inst.AccountLogin, &inst.InstalledAt,
		); err != nil {
			return nil, fmt.Errorf("scan org_github_app_installations: %w", err)
		}
		out = append(out, inst)
	}
	return out, rows.Err()
}

// ListInstallationsForOrgSystem is identical to ListInstallationsForOrg in
// local mode — single conn, no RLS, no claims — but exists so the
// credential resolver uses the same interface shape it would in multi mode.
func (s *gitHubAppsStore) ListInstallationsForOrgSystem(ctx context.Context, orgID string) ([]domain.OrgGitHubAppInstallation, error) {
	return s.ListInstallationsForOrg(ctx, orgID)
}

// UpsertInstallation mirrors one installation, idempotent on installation_id.
// installed_at is set only on insert (defaulting to CURRENT_TIMESTAMP for a
// zero InstalledAt) and preserved on conflict; removed_at is cleared so a
// reinstall revives the row.
func (s *gitHubAppsStore) UpsertInstallation(ctx context.Context, inst domain.OrgGitHubAppInstallation) error {
	var installedAt sql.NullTime
	if !inst.InstalledAt.IsZero() {
		installedAt = sql.NullTime{Time: inst.InstalledAt, Valid: true}
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO org_github_app_installations
			(installation_id, org_id, account_type, account_login, installed_at, removed_at)
		VALUES (?, ?, ?, ?, COALESCE(?, CURRENT_TIMESTAMP), NULL)
		ON CONFLICT(org_id, installation_id) DO UPDATE SET
			account_type  = excluded.account_type,
			account_login = excluded.account_login,
			removed_at    = NULL
	`, inst.InstallationID, inst.OrgID, inst.AccountType, inst.AccountLogin, installedAt)
	if err != nil {
		return fmt.Errorf("upsert org_github_app_installations: %w", err)
	}
	return nil
}

func (s *gitHubAppsStore) MarkInstallationRemoved(ctx context.Context, orgID, installationID string) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE org_github_app_installations
		   SET removed_at = CURRENT_TIMESTAMP
		 WHERE org_id = ? AND installation_id = ? AND removed_at IS NULL
	`, orgID, installationID)
	if err != nil {
		return fmt.Errorf("mark org_github_app_installations removed: %w", err)
	}
	return nil
}

func (s *gitHubAppsStore) activeInstallationIDs(ctx context.Context, orgID string) ([]string, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT installation_id FROM org_github_app_installations
		 WHERE org_id = ? AND removed_at IS NULL
	`, orgID)
	if err != nil {
		return nil, fmt.Errorf("read active installations: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *gitHubAppsStore) BackfillInstallationsFromAPI(ctx context.Context, orgID string) error {
	var appID, pemRef, baseURL string
	err := s.q.QueryRowContext(ctx, `
		SELECT a.app_id, a.pem_ref, COALESCE(s.github_base_url, '')
		  FROM org_github_apps a
		  LEFT JOIN org_settings s ON s.org_id = a.org_id
		 WHERE a.org_id = ? AND a.active = 1
	`, orgID).Scan(&appID, &pemRef, &baseURL)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // no registered App for this org — nothing to backfill
	}
	if err != nil {
		return fmt.Errorf("read org_github_apps for backfill: %w", err)
	}

	insts, err := db.DiscoverAppInstallations(ctx, s.secrets, orgID, appID, pemRef, baseURL)
	if err != nil {
		return err
	}
	active, err := s.activeInstallationIDs(ctx, orgID)
	if err != nil {
		return err
	}
	return db.ReconcileInstallations(insts, active,
		func(i domain.OrgGitHubAppInstallation) error { return s.UpsertInstallation(ctx, i) },
		func(id string) error { return s.MarkInstallationRemoved(ctx, orgID, id) },
	)
}
