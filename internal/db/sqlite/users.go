package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// usersStore — SQLite impl. The constructor accepts two queryers for
// signature parity with the Postgres impl (SKY-296); SQLite has one
// connection so both collapse to the same queryer. The
// `...System` variants delegate to their non-System counterparts.
type usersStore struct{ q queryer }

func newUsersStore(q, _ queryer) db.UsersStore { return &usersStore{q: q} }

var _ db.UsersStore = (*usersStore)(nil)

func (s *usersStore) GetGitHubLogin(ctx context.Context, userID, githubBaseURL string) (string, error) {
	var login string
	err := s.q.QueryRowContext(ctx,
		`SELECT login FROM user_github_identities WHERE user_id = ? AND github_base_url = ?`,
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
	// surfaces as a FOREIGN KEY constraint error, matching the old
	// SetGitHubUsername "user not found" guard.
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO user_github_identities
			(user_id, github_base_url, login, source, verified_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(user_id, github_base_url) DO UPDATE SET
			login       = excluded.login,
			source      = excluded.source,
			verified_at = excluded.verified_at,
			updated_at  = CURRENT_TIMESTAMP
	`, userID, db.NormalizeGitHubHost(githubBaseURL), login, source)
	if err != nil {
		return fmt.Errorf("upsert user_github_identities: %w", err)
	}
	return nil
}

func (s *usersStore) ClearGitHubIdentity(ctx context.Context, userID, githubBaseURL string) error {
	if _, err := s.q.ExecContext(ctx,
		`DELETE FROM user_github_identities WHERE user_id = ? AND github_base_url = ?`,
		userID, db.NormalizeGitHubHost(githubBaseURL),
	); err != nil {
		return fmt.Errorf("delete user_github_identities: %w", err)
	}
	return nil
}

func (s *usersStore) GetDisplayName(ctx context.Context, userID string) (string, error) {
	var name sql.NullString
	err := s.q.QueryRowContext(ctx,
		`SELECT display_name FROM users WHERE id = ?`,
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

func (s *usersStore) GetJiraIdentity(ctx context.Context, userID, jiraBaseURL string) (string, string, error) {
	var accountID, displayName sql.NullString
	err := s.q.QueryRowContext(ctx,
		`SELECT account_id, display_name FROM user_jira_identities WHERE user_id = ? AND jira_base_url = ?`,
		userID, db.NormalizeJiraHost(jiraBaseURL),
	).Scan(&accountID, &displayName)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("read user_jira_identities: %w", err)
	}
	return accountID.String, displayName.String, nil
}

func (s *usersStore) GetJiraIdentitySystem(ctx context.Context, userID, jiraBaseURL string) (string, string, error) {
	return s.GetJiraIdentity(ctx, userID, jiraBaseURL)
}

func (s *usersStore) GetGitHubLoginSystem(ctx context.Context, userID, githubBaseURL string) (string, error) {
	return s.GetGitHubLogin(ctx, userID, githubBaseURL)
}

func (s *usersStore) UserIDsForGitHubLoginSystem(ctx context.Context, githubBaseURL, login string) ([]string, error) {
	// Reverse of GetGitHubLogin: (host, login) → user_id(s). Host is
	// normalized the same way the writers store it so the key matches;
	// login matches verbatim. Returns every matching row — two users may
	// bind one login on a host (PK is per-user), and source/verified_at
	// are out of scope. SQLite is N=1, so in practice this resolves the
	// one synthetic user to itself.
	rows, err := s.q.QueryContext(ctx, `
		SELECT user_id
		FROM user_github_identities
		WHERE github_base_url = ? AND login = ?
		ORDER BY user_id ASC
	`, db.NormalizeGitHubHost(githubBaseURL), login)
	if err != nil {
		return nil, fmt.Errorf("read user_github_identities by login: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan user_github_identities.user_id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *usersStore) UpsertJiraIdentity(ctx context.Context, userID, jiraBaseURL, accountID, displayName, source string) error {
	// FK on user_id enforces the row-exists contract: a missing user
	// surfaces as a FOREIGN KEY constraint error. display_name is
	// nullable — "" stores NULL.
	var nameVal any
	if displayName != "" {
		nameVal = displayName
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO user_jira_identities
			(user_id, jira_base_url, account_id, display_name, source, verified_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(user_id, jira_base_url) DO UPDATE SET
			account_id   = excluded.account_id,
			display_name = excluded.display_name,
			source       = excluded.source,
			verified_at  = excluded.verified_at,
			updated_at   = CURRENT_TIMESTAMP
	`, userID, db.NormalizeJiraHost(jiraBaseURL), accountID, nameVal, source)
	if err != nil {
		return fmt.Errorf("upsert user_jira_identities: %w", err)
	}
	return nil
}

func (s *usersStore) ClearJiraIdentity(ctx context.Context, userID, jiraBaseURL string) error {
	if _, err := s.q.ExecContext(ctx,
		`DELETE FROM user_jira_identities WHERE user_id = ? AND jira_base_url = ?`,
		userID, db.NormalizeJiraHost(jiraBaseURL),
	); err != nil {
		return fmt.Errorf("delete user_jira_identities: %w", err)
	}
	return nil
}

func (s *usersStore) GetLastActingTeam(ctx context.Context, userID string) (string, error) {
	var teamID sql.NullString
	err := s.q.QueryRowContext(ctx,
		`SELECT last_acting_team_id FROM users WHERE id = ?`,
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
		`UPDATE users SET last_acting_team_id = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
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
		`SELECT updated_at FROM user_settings WHERE user_id = ?`,
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
	// Effectively a touch in v1 — no per-user fields yet, the
	// updated_at trigger fires either way. Future per-user prefs
	// land here.
	if _, err := s.q.ExecContext(ctx, `
		INSERT INTO user_settings (user_id, updated_at)
		VALUES (?, CURRENT_TIMESTAMP)
		ON CONFLICT(user_id) DO UPDATE SET updated_at = CURRENT_TIMESTAMP
	`, userID); err != nil {
		return fmt.Errorf("upsert user_settings: %w", err)
	}
	return nil
}
