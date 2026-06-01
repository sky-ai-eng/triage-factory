package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// UsersStore owns the users table — identity facts that aren't secrets
// (display_name, jira_*) live on the row, plus the host-scoped GitHub
// identity bindings in user_github_identities (SKY-396). The keychain
// holds only actual credentials (PATs in local mode); usernames and
// display names live in the DB so local mode and multi mode share storage.
//
// Local mode iterates a single synthetic LocalDefaultUserID row;
// multi mode (post-SKY-251) has one row per authenticated user.
//
// # Pool split (Postgres)
//
// Most methods run on the app pool. There's no admin-pool routing for
// row mutation because user-row creation is an auth-flow concern
// (SKY-251) and this store only mutates existing rows. The `...System`
// read variants route through the admin pool for claims-free callers —
// boot-time callers (poller bootstrap) and the routing eventbus
// subscriber (reverse identity lookup) — that have no JWT-claims
// context.
type UsersStore interface {
	// GetGitHubLogin returns the user's GitHub login on a specific host
	// (user_github_identities, keyed on (user_id, github_base_url)), or
	// "" when no row exists for that (user, host) pair. Used by the
	// SKY-264 predicate matcher (author_in / reviewer_in / commenter_in
	// allowlists) and several display surfaces. Callers resolve the host
	// from the org's org_settings.github_base_url so the lookup matches
	// the host the binding was captured against. An absent row degrades
	// exactly as the old NULL github_username column did.
	GetGitHubLogin(ctx context.Context, userID, githubBaseURL string) (string, error)

	// UpsertGitHubIdentity writes (or refreshes) the user's GitHub login
	// for a host. The host is an explicit parameter so callers bind to
	// the org's host deliberately. source records how the binding was
	// captured ('pat' | 'connect_oauth' | 'scim' | 'login_claim');
	// verified_at is stamped to now() on every call (this IS the
	// authenticated confirmation). Upserts on the (user_id,
	// github_base_url) key. Returns an error when the user row does not
	// exist — bootstrap paths own row creation.
	UpsertGitHubIdentity(ctx context.Context, userID, githubBaseURL, login, source string) error

	// ClearGitHubIdentity deletes the user's GitHub identity row for a
	// host (the disconnect path — GitHub URL/PAT cleared in Settings).
	// No-op (nil error) when no row exists for that (user, host) pair.
	ClearGitHubIdentity(ctx context.Context, userID, githubBaseURL string) error

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
	// GetGitHubLoginSystem mirrors GetGitHubLogin but routes through
	// the admin pool in Postgres. The single consumer is the poller
	// bootstrap, which reads the local user's stored login at server
	// boot to seed the GitHub poller's identity allowlist — there is no
	// JWT-claims context at that point. Behavior matches GetGitHubLogin;
	// SQLite collapses the two variants to one connection.
	GetGitHubLoginSystem(ctx context.Context, userID, githubBaseURL string) (string, error)

	// UserIDsForGitHubLoginSystem returns every TF user bound to login
	// on githubBaseURL in user_github_identities — the reverse of
	// GetGitHubLogin. Host-scoped, org-agnostic (identity is keyed on
	// (user_id, github_base_url), no org). Empty slice (not error) when
	// no binding exists. The author-/reviewer-routing routers consume
	// it from their eventbus subscriber goroutines, which carry no
	// JWT-claims context.
	//
	// Returns a set deliberately. The table's PK is (user_id,
	// github_base_url) — it constrains uniqueness per user, so nothing
	// stops two TF users binding the same login on one host (a shared
	// bot account, a stale pre-rename row, a typo). Callers union the
	// resulting teams. githubBaseURL is required and normalized the same
	// way writes are (db.NormalizeGitHubHost), so the key matches what
	// the capture paths stored. Source / verified_at are out of scope:
	// every matching row is returned regardless of how it was captured
	// or whether it has been host-verified.
	//
	// Admin pool / claims-free: system/router callers only. Exposing
	// this reverse lookup to a request handler would be a cross-user
	// identity probe (resolve any login → any user); a request-path
	// consumer would need a separate claims-scoped method with its own
	// RLS story, and none is needed today. SQLite collapses to one
	// connection.
	UserIDsForGitHubLoginSystem(ctx context.Context, githubBaseURL, login string) ([]string, error)

	// GetJiraIdentitySystem mirrors GetJiraIdentity but routes through
	// the admin pool in Postgres. The router's inline close-check on
	// Jira reassignment consumes this from its eventbus subscriber
	// goroutine, which has no JWT-claims context.
	GetJiraIdentitySystem(ctx context.Context, userID string) (accountID, displayName string, err error)

	// GetLastActingTeam returns users.last_acting_team_id — the user's
	// sticky default team — or "" if unset (NULL) or the row is missing.
	// The acting-team resolver consults it (after an explicit pick,
	// before the sole-team fallback), and the selector endpoint surfaces
	// it so the per-page filter and write picker seed to it. The returned
	// id is not membership-validated here; the resolver re-checks it
	// against the caller's current-org teams, so a stale id (team since
	// deleted, or in another org) is simply ignored downstream.
	GetLastActingTeam(ctx context.Context, userID string) (string, error)

	// SetLastActingTeam writes users.last_acting_team_id for an existing
	// user row. Passing "" clears it (NULL). Returns an error when the
	// row does not exist — bootstrap owns row creation; this store only
	// mutates. The caller validates team membership before persisting
	// (you can only pin a team you belong to). Postgres routes through
	// the app pool (users_modify RLS gates id = current_user_id()).
	SetLastActingTeam(ctx context.Context, userID, teamID string) error

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
