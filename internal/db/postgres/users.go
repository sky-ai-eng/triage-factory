package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// usersStore is the Postgres impl of db.UsersStore. Holds two pools:
//
//   - q: app pool (tf_app, RLS-active). Every request-equivalent
//     consumer hits this side. RLS policies gate by user_id
//     identity once they land.
//   - admin: admin pool (BYPASSRLS). Claims-free system callers: the
//     poller bootstrap's GetGitHubLoginSystem read at startup, the
//     router's reverse identity lookups, and the dashboard-history
//     backfill worker's marker read/write (TFAC-396) — all run before or
//     outside any JWT-claims context.
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

func (s *usersStore) GetGitHubIdentity(ctx context.Context, userID, githubBaseURL string) (*domain.UserGitHubIdentity, error) {
	var (
		id       domain.UserGitHubIdentity
		ghUserID sql.NullString
		verified sql.NullTime
	)
	err := s.q.QueryRowContext(ctx,
		`SELECT login, github_user_id, source, verified_at
		   FROM user_github_identities
		  WHERE user_id = $1 AND github_base_url = $2`,
		userID, db.NormalizeGitHubHost(githubBaseURL),
	).Scan(&id.Login, &ghUserID, &id.Source, &verified)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read user_github_identities: %w", err)
	}
	id.GitHubUserID = ghUserID.String
	if verified.Valid {
		id.VerifiedAt = verified.Time.UTC()
	}
	return &id, nil
}

func (s *usersStore) GetGitHubLoginSystem(ctx context.Context, userID, githubBaseURL string) (string, error) {
	return getGitHubLogin(ctx, s.admin, userID, githubBaseURL)
}

func (s *usersStore) DashboardBackfilledAtSystem(ctx context.Context, userID, githubBaseURL string) (*time.Time, error) {
	var at sql.NullTime
	err := s.admin.QueryRowContext(ctx,
		`SELECT dashboard_backfilled_at FROM user_github_identities WHERE user_id = $1 AND github_base_url = $2`,
		userID, db.NormalizeGitHubHost(githubBaseURL),
	).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read user_github_identities.dashboard_backfilled_at: %w", err)
	}
	if !at.Valid {
		return nil, nil
	}
	return &at.Time, nil
}

func (s *usersStore) MarkDashboardBackfilledSystem(ctx context.Context, userID, githubBaseURL, login string) error {
	// login in the WHERE: only stamp if the row still carries the login we
	// actually backfilled. A re-bind to a different login mid-flight changes the
	// row, so this no-ops rather than marking the new login done — see the
	// interface doc.
	if _, err := s.admin.ExecContext(ctx,
		`UPDATE user_github_identities SET dashboard_backfilled_at = now() WHERE user_id = $1 AND github_base_url = $2 AND login = $3`,
		userID, db.NormalizeGitHubHost(githubBaseURL), login,
	); err != nil {
		return fmt.Errorf("mark user_github_identities.dashboard_backfilled_at: %w", err)
	}
	return nil
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

func (s *usersStore) UserIDsForGitHubLoginSystem(ctx context.Context, githubBaseURL, login string) ([]string, error) {
	// Reverse of getGitHubLogin: (host, login) → user_id(s). Host is
	// normalized the same way the writers store it so the key matches;
	// login matches on lower() because GitHub logins are case-insensitive
	// while the writers persist them as captured (user_github_identities_
	// login_lookup_idx serves it, mirroring user_identities' lower(email)).
	// Returns every matching row — two users may bind one login on a host
	// (PK is per-user), and source/verified_at are out of scope.
	rows, err := s.admin.QueryContext(ctx, `
		SELECT user_id::text
		FROM user_github_identities
		WHERE github_base_url = $1 AND lower(login) = lower($2)
		ORDER BY user_id ASC
	`, db.NormalizeGitHubHost(githubBaseURL), login)
	if err != nil {
		return nil, fmt.Errorf("read user_github_identities by login: %w", err)
	}
	return scanIDs(rows, "user_github_identities.user_id")
}

func (s *usersStore) UserIDsForVerifiedEmailSystem(ctx context.Context, email string) ([]string, error) {
	// Mirrors auth_handlers.go's inline auto-link lookup: lowercase/trim
	// before matching against public.user_identities' lower(email) index.
	email = strings.ToLower(strings.TrimSpace(email))
	rows, err := s.admin.QueryContext(ctx, `
		SELECT DISTINCT user_id::text
		FROM public.user_identities
		WHERE lower(email) = $1 AND email_verified
		ORDER BY user_id ASC
	`, email)
	if err != nil {
		return nil, fmt.Errorf("read user_identities by verified email: %w", err)
	}
	return scanIDs(rows, "user_identities.user_id")
}

// scanIDs drains rows of a single user_id::text column into a []string,
// closing rows and wrapping any scan error with errContext. Shared by the
// UserIDsFor*System reverse lookups above and below, which differ only in
// their query and error context.
func scanIDs(rows *sql.Rows, errContext string) ([]string, error) {
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan %s: %w", errContext, err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *usersStore) UpsertGitHubIdentity(ctx context.Context, userID, githubBaseURL, login, githubUserID, email, source string) error {
	// FK on user_id enforces the row-exists contract: a missing user
	// surfaces as a foreign_key_violation, matching the old
	// SetGitHubUsername "user not found" guard.
	//
	// github_user_id is COALESCEd, unlike login: a capture that learned no id
	// leaves a known one alone rather than erasing it, so the column fills in
	// opportunistically. A re-bind to a different account carries that
	// account's id, so the COALESCE never pins a stale one.
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO user_github_identities
			(user_id, github_base_url, login, github_user_id, email, source, verified_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, now(), now(), now())
		ON CONFLICT (user_id, github_base_url) DO UPDATE SET
			login          = EXCLUDED.login,
			github_user_id = COALESCE(EXCLUDED.github_user_id, user_github_identities.github_user_id),
			email           = CASE
				WHEN user_github_identities.login IS DISTINCT FROM EXCLUDED.login THEN EXCLUDED.email
				ELSE COALESCE(EXCLUDED.email, user_github_identities.email) END,
			source         = EXCLUDED.source,
			verified_at    = EXCLUDED.verified_at,
			updated_at     = now(),
			-- A rename / re-bind to a different login invalidates the prior
			-- login's dashboard backfill: clear the marker so the new login's
			-- history is re-seeded (TFAC-396). A no-op re-bind (same login)
			-- keeps the marker, so it never re-fires the search burst.
			dashboard_backfilled_at = CASE
				WHEN user_github_identities.login IS DISTINCT FROM EXCLUDED.login THEN NULL
				ELSE user_github_identities.dashboard_backfilled_at END
	`, userID, db.NormalizeGitHubHost(githubBaseURL), login, nullString(githubUserID), nullString(email), source)
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

func (s *usersStore) GetProfile(ctx context.Context, userID string) (string, string, error) {
	var name, avatar sql.NullString
	err := s.q.QueryRowContext(ctx,
		`SELECT display_name, avatar_url FROM users WHERE id = $1`,
		userID,
	).Scan(&name, &avatar)
	if err == sql.ErrNoRows {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("read users profile: %w", err)
	}
	return name.String, avatar.String, nil
}

func (s *usersStore) GetJiraIdentity(ctx context.Context, userID, jiraBaseURL string) (string, string, error) {
	return getJiraIdentity(ctx, s.q, userID, jiraBaseURL)
}

func (s *usersStore) GetJiraIdentitySystem(ctx context.Context, userID, jiraBaseURL string) (string, string, error) {
	return getJiraIdentity(ctx, s.admin, userID, jiraBaseURL)
}

func (s *usersStore) UserIDsForJiraAccountSystem(ctx context.Context, jiraBaseURL, accountID string) ([]string, error) {
	// Reverse of getJiraIdentity: (host, account_id) → user_id(s). Host is
	// normalized the same way the writers store it so the key matches;
	// account_id matches verbatim (Atlassian account ids are stable
	// identifiers, stored as captured). Returns every matching row — two
	// users may bind one account on a host (PK is per-user), and
	// source/verified_at are out of scope.
	rows, err := s.admin.QueryContext(ctx, `
		SELECT user_id::text
		FROM user_jira_identities
		WHERE jira_base_url = $1 AND account_id = $2
		ORDER BY user_id ASC
	`, db.NormalizeJiraHost(jiraBaseURL), accountID)
	if err != nil {
		return nil, fmt.Errorf("read user_jira_identities by account: %w", err)
	}
	return scanIDs(rows, "user_jira_identities.user_id")
}

func getJiraIdentity(ctx context.Context, q queryer, userID, jiraBaseURL string) (string, string, error) {
	var accountID, displayName sql.NullString
	err := q.QueryRowContext(ctx,
		`SELECT account_id, display_name FROM user_jira_identities WHERE user_id = $1 AND jira_base_url = $2`,
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

func (s *usersStore) UpsertJiraIdentity(ctx context.Context, userID, jiraBaseURL, accountID, displayName, source string) error {
	// account_id is the assignee-match key, so an empty one is a useless
	// row that never matches any check. NOT NULL only rejects SQL NULL,
	// not "" — reject it here so the contract is self-enforcing for any
	// future caller (the only caller today already guards on StableID()).
	if accountID == "" {
		return fmt.Errorf("upsert user_jira_identities: account_id required")
	}
	// FK on user_id enforces the row-exists contract: a missing user
	// surfaces as a foreign_key_violation. display_name is nullable —
	// "" stores NULL.
	var nameVal any
	if displayName != "" {
		nameVal = displayName
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO user_jira_identities
			(user_id, jira_base_url, account_id, display_name, source, verified_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, now(), now(), now())
		ON CONFLICT (user_id, jira_base_url) DO UPDATE SET
			account_id   = EXCLUDED.account_id,
			display_name = EXCLUDED.display_name,
			source       = EXCLUDED.source,
			verified_at  = EXCLUDED.verified_at,
			updated_at   = now()
	`, userID, db.NormalizeJiraHost(jiraBaseURL), accountID, nameVal, source)
	if err != nil {
		return fmt.Errorf("upsert user_jira_identities: %w", err)
	}
	return nil
}

func (s *usersStore) ClearJiraIdentity(ctx context.Context, userID, jiraBaseURL string) error {
	if _, err := s.q.ExecContext(ctx,
		`DELETE FROM user_jira_identities WHERE user_id = $1 AND jira_base_url = $2`,
		userID, db.NormalizeJiraHost(jiraBaseURL),
	); err != nil {
		return fmt.Errorf("delete user_jira_identities: %w", err)
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
	// No row reads as the zero value rather than an error: every field's
	// absent value already says what "never written" says, so a first-touch
	// user and a user who has set nothing are the same answer.
	var seenAt sql.NullTime
	err := s.q.QueryRowContext(ctx,
		`SELECT overview_seen_at FROM user_settings WHERE user_id = $1`,
		userID,
	).Scan(&seenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.UserSettings{}, nil
	}
	if err != nil {
		return domain.UserSettings{}, fmt.Errorf("read user_settings: %w", err)
	}
	var out domain.UserSettings
	if seenAt.Valid {
		at := seenAt.Time.UTC()
		out.OverviewSeenAt = &at
	}
	return out, nil
}

func (s *usersStore) UpdateSettings(ctx context.Context, userID string, updates domain.UserSettings) (domain.UserSettings, error) {
	// updates is the row's end state, so every field lands as given and a nil
	// one clears its column. The partial-write caller composes that end state
	// from a read in the same transaction.
	//
	// The RETURNING runs under user_settings_select on the app pool, which
	// gates the same user_id the write's own policy does — so the conflict arm
	// hands its row back rather than yielding zero rows from a statement that
	// updated one.
	var seenAt sql.NullTime
	if updates.OverviewSeenAt != nil {
		seenAt = sql.NullTime{Time: updates.OverviewSeenAt.UTC(), Valid: true}
	}
	var stored sql.NullTime
	err := s.q.QueryRowContext(ctx, `
		INSERT INTO user_settings (user_id, overview_seen_at, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (user_id) DO UPDATE SET
			overview_seen_at = excluded.overview_seen_at,
			updated_at       = now()
		RETURNING overview_seen_at
	`, userID, seenAt).Scan(&stored)
	if err != nil {
		return domain.UserSettings{}, fmt.Errorf("upsert user_settings: %w", err)
	}
	var out domain.UserSettings
	if stored.Valid {
		at := stored.Time.UTC()
		out.OverviewSeenAt = &at
	}
	return out, nil
}
