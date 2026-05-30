package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// UsersStore owns the users table — identity facts that aren't secrets
// (display_name, github_username) live on the row. The keychain holds
// only actual credentials (PATs in local mode); usernames and display
// names live in the DB so local mode and multi mode share storage.
//
// Local mode iterates a single synthetic LocalDefaultUserID row;
// multi mode (post-SKY-251) has one row per authenticated user.
//
// # Pool split (Postgres)
//
// All methods run on the app pool. There's no admin-pool routing
// because user-row creation is an auth-flow concern (SKY-251) and
// this store only mutates existing rows.
type UsersStore interface {
	// GetGitHubUsername returns users.github_username for a user row,
	// or "" if the column is NULL or the row does not exist. Used by
	// the SKY-264 predicate matcher (author_in / reviewer_in /
	// commenter_in allowlists), the poller startup, and several
	// display surfaces.
	GetGitHubUsername(ctx context.Context, userID string) (string, error)

	// SetGitHubUsername writes users.github_username for an existing
	// user row. Passing "" clears the column (NULL). Returns an error
	// when the target row does not exist — bootstrap paths own row
	// creation; this store only mutates existing rows. Idempotent on
	// identical input.
	SetGitHubUsername(ctx context.Context, userID, login string) error

	// GetDisplayName returns users.display_name, or "" if NULL or the
	// row is missing. The team-members endpoint surfaces this in
	// Variant B's roster dropdown.
	GetDisplayName(ctx context.Context, userID string) (string, error)

	// GetJiraIdentity returns (jira_account_id, jira_display_name) for
	// a user row, both "" if the columns are NULL or the row does not
	// exist. Used by the SKY-270 predicate matcher (assignee_in /
	// reporter_in / commenter_in allowlists), the stock handler's
	// "is this assigned to me" check, and the optimistic post-claim
	// snapshot update.
	GetJiraIdentity(ctx context.Context, userID string) (accountID, displayName string, err error)

	// SetJiraIdentity writes both jira_account_id and jira_display_name
	// for an existing user row in a single UPDATE. Both come from one
	// auth.ValidateJira call (Atlassian's /myself endpoint returns
	// them together), so pairing them keeps the columns consistent.
	// Passing "" for either clears the column (NULL). Returns an error
	// when the target row does not exist — bootstrap paths own row
	// creation; this store only mutates existing rows. Idempotent on
	// identical input.
	SetJiraIdentity(ctx context.Context, userID, accountID, displayName string) error

	// --- Admin-pool variants (`...System`) ---
	//
	// GetGitHubUsernameSystem mirrors GetGitHubUsername but routes
	// through the admin pool in Postgres. The single consumer is the
	// poller bootstrap, which reads each repo owner's stored login at
	// server boot to seed the GitHub poller's identity allowlist —
	// there is no JWT-claims context at that point, and the read
	// spans every user the poller intends to act for. Behavior
	// matches GetGitHubUsername; SQLite collapses the two variants
	// to one connection.
	GetGitHubUsernameSystem(ctx context.Context, userID string) (string, error)

	// GetJiraIdentitySystem mirrors GetJiraIdentity but routes through
	// the admin pool in Postgres. The router's inline close-check on
	// Jira reassignment consumes this from its eventbus subscriber
	// goroutine, which has no JWT-claims context.
	GetJiraIdentitySystem(ctx context.Context, userID string) (accountID, displayName string, err error)

	// GetPreferredTeam returns users.preferred_team_id — the user's
	// sticky default team — or "" if unset (NULL) or the row is missing.
	// The acting-team resolver consults it (after an explicit pick,
	// before the sole-team fallback), and the selector endpoint surfaces
	// it so the per-page filter and write picker seed to it. The returned
	// id is not membership-validated here; the resolver re-checks it
	// against the caller's current-org teams, so a stale id (team since
	// deleted, or in another org) is simply ignored downstream.
	GetPreferredTeam(ctx context.Context, userID string) (string, error)

	// SetPreferredTeam writes users.preferred_team_id for an existing
	// user row. Passing "" clears it (NULL). Returns an error when the
	// row does not exist — bootstrap owns row creation; this store only
	// mutates. The caller validates team membership before persisting
	// (you can only pin a team you belong to). Postgres routes through
	// the app pool (users_modify RLS gates id = current_user_id()).
	SetPreferredTeam(ctx context.Context, userID, teamID string) error

	// GetSettings returns the user's settings row. Empty for v1
	// post-SKY-354 cleanup — the AI model + auto-delegate toggle
	// moved to team_settings. The store call exists so future
	// per-user prefs (theme, notification destinations, swipe
	// sensitivity, onboarding state) can be added without a
	// signature change. Returns a zero-value UserSettings with nil
	// error when no row exists yet. Postgres runs on the app pool
	// (user_settings_select RLS gates by user_id = current_user_id()).
	GetSettings(ctx context.Context, userID string) (domain.UserSettings, error)

	// UpdateSettings upserts the user's settings row. v1 carries
	// no per-user fields, so the call is effectively a touch — the
	// updated_at trigger fires either way. Future prefs land here.
	// Postgres runs on the app pool (user_settings_modify RLS gates
	// by user_id = current_user_id()).
	UpdateSettings(ctx context.Context, userID string, updates domain.UserSettings) error
}
