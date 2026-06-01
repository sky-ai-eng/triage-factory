package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// usersStore is the Postgres impl of db.UsersStore. Holds two pools
// (SKY-296):
//
//   - q: app pool (tf_app, RLS-active). Every request-equivalent
//     consumer hits this side. RLS policies gate by user_id
//     identity once they land (SKY-251 territory).
//   - admin: admin pool (BYPASSRLS). The single consumer is the
//     poller bootstrap's GetGitHubLoginSystem read at startup,
//     before any JWT claims context can exist.
type usersStore struct {
	q     queryer
	admin queryer
}

func newUsersStore(q, admin queryer) db.UsersStore {
	return &usersStore{q: q, admin: admin}
}

var _ db.UsersStore = (*usersStore)(nil)

func (s *usersStore) GetGitHubLogin(ctx context.Context, userID, githubBaseURL string) (string, error) {
	return getGitHubLogin(ctx, s.q, userID, githubBaseURL)
}

func (s *usersStore) GetGitHubLoginSystem(ctx context.Context, userID, githubBaseURL string) (string, error) {
	return getGitHubLogin(ctx, s.admin, userID, githubBaseURL)
}

func getGitHubLogin(ctx context.Context, q queryer, userID, githubBaseURL string) (string, error) {
	var login string
	err := q.QueryRowContext(ctx,
		`SELECT login FROM user_github_identities WHERE user_id = $1 AND github_base_url = $2`,
		userID, db.NormalizeGitHubHost(githubBaseURL),
	).Scan(&login)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read user_github_identities.login: %w", err)
	}
	return login, nil
}

func (s *usersStore) UpsertGitHubIdentity(ctx context.Context, userID, githubBaseURL, login, source string) error {
	// FK on user_id enforces the row-exists contract: a missing user
	// surfaces as a foreign_key_violation, matching the old
	// SetGitHubUsername "user not found" guard.
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO user_github_identities
			(user_id, github_base_url, login, source, verified_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, now(), now(), now())
		ON CONFLICT (user_id, github_base_url) DO UPDATE SET
			login       = EXCLUDED.login,
			source      = EXCLUDED.source,
			verified_at = EXCLUDED.verified_at,
			updated_at  = now()
	`, userID, db.NormalizeGitHubHost(githubBaseURL), login, source)
	if err != nil {
		return fmt.Errorf("upsert user_github_identities: %w", err)
	}
	return nil
}

func (s *usersStore) ClearGitHubIdentity(ctx context.Context, userID, githubBaseURL string) error {
	if _, err := s.q.ExecContext(ctx,
		`DELETE FROM user_github_identities WHERE user_id = $1 AND github_base_url = $2`,
		userID, db.NormalizeGitHubHost(githubBaseURL),
	); err != nil {
		return fmt.Errorf("delete user_github_identities: %w", err)
	}
	return nil
}

func (s *usersStore) GetDisplayName(ctx context.Context, userID string) (string, error) {
	var name sql.NullString
	err := s.q.QueryRowContext(ctx,
		`SELECT display_name FROM users WHERE id = $1`,
		userID,
	).Scan(&name)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read users.display_name: %w", err)
	}
	return name.String, nil
}

func (s *usersStore) GetJiraIdentity(ctx context.Context, userID string) (string, string, error) {
	return getJiraIdentity(ctx, s.q, userID)
}

func (s *usersStore) GetJiraIdentitySystem(ctx context.Context, userID string) (string, string, error) {
	return getJiraIdentity(ctx, s.admin, userID)
}

func getJiraIdentity(ctx context.Context, q queryer, userID string) (string, string, error) {
	var accountID, displayName sql.NullString
	err := q.QueryRowContext(ctx,
		`SELECT jira_account_id, jira_display_name FROM users WHERE id = $1`,
		userID,
	).Scan(&accountID, &displayName)
	if err == sql.ErrNoRows {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("read users.jira_account_id/jira_display_name: %w", err)
	}
	return accountID.String, displayName.String, nil
}

func (s *usersStore) SetJiraIdentity(ctx context.Context, userID, accountID, displayName string) error {
	var accVal, nameVal any
	if accountID != "" {
		accVal = accountID
	}
	if displayName != "" {
		nameVal = displayName
	}
	result, err := s.q.ExecContext(ctx,
		`UPDATE users SET jira_account_id = $1, jira_display_name = $2, updated_at = NOW() WHERE id = $3`,
		accVal, nameVal, userID,
	)
	if err != nil {
		return fmt.Errorf("update users.jira_account_id/jira_display_name: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read users.jira_identity update result: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("update users.jira_identity: user %q not found", userID)
	}
	return nil
}

func (s *usersStore) GetLastActingTeam(ctx context.Context, userID string) (string, error) {
	var teamID sql.NullString
	err := s.q.QueryRowContext(ctx,
		`SELECT last_acting_team_id::text FROM users WHERE id = $1`,
		userID,
	).Scan(&teamID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read users.last_acting_team_id: %w", err)
	}
	return teamID.String, nil
}

func (s *usersStore) SetLastActingTeam(ctx context.Context, userID, teamID string) error {
	var val any
	if teamID != "" {
		val = teamID
	} // else val stays nil → NULL
	result, err := s.q.ExecContext(ctx,
		`UPDATE users SET last_acting_team_id = $1, updated_at = NOW() WHERE id = $2`,
		val, userID,
	)
	if err != nil {
		return fmt.Errorf("update users.last_acting_team_id: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read users.last_acting_team_id update result: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("update users.last_acting_team_id: user %q not found", userID)
	}
	return nil
}

func (s *usersStore) GetSettings(ctx context.Context, userID string) (domain.UserSettings, error) {
	// user_settings is empty post-SKY-354: just user_id + updated_at.
	// The probe stays so callers can detect "row exists" vs "first
	// touch" later when fields are added; current callers ignore the
	// difference and consume the zero-value struct either way.
	var updatedAt sql.NullTime
	err := s.q.QueryRowContext(ctx,
		`SELECT updated_at FROM user_settings WHERE user_id = $1`,
		userID,
	).Scan(&updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.UserSettings{}, nil
	}
	if err != nil {
		return domain.UserSettings{}, fmt.Errorf("read user_settings: %w", err)
	}
	return domain.UserSettings{}, nil
}

func (s *usersStore) UpdateSettings(ctx context.Context, userID string, _ domain.UserSettings) error {
	// Effectively a touch in v1 — no per-user fields yet, the updated_at
	// trigger fires either way. Future per-user prefs land here.
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO user_settings (user_id, updated_at)
		VALUES ($1, now())
		ON CONFLICT (user_id) DO UPDATE SET updated_at = now()
	`, userID)
	if err != nil {
		return fmt.Errorf("upsert user_settings: %w", err)
	}
	return nil
}
