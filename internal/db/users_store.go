package db

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// UsersStore owns the users table — identity facts that aren't secrets
// (display_name) live on the row, plus the host-scoped per-provider
// identity bindings in user_github_identities and
// user_jira_identities. The keychain holds only actual
// credentials (PATs in local mode); usernames and display names live in
// the DB so local mode and multi mode share storage.
//
// Local mode iterates a single synthetic LocalDefaultUserID row;
// multi mode has one row per authenticated user.
//
// # Pool split (Postgres)
//
// Most methods run on the app pool. There's no admin-pool routing for
// row mutation because user-row creation is an auth-flow concern
// and this store only mutates existing rows. The `...System`
// read variants route through the admin pool for claims-free callers —
// boot-time callers (poller bootstrap) and the routing eventbus
// subscriber (reverse identity lookup) — that have no JWT-claims
// context.
type UsersStore interface {
	// GetGitHubLogin returns the user's GitHub login on a specific host
	// (user_github_identities, keyed on (user_id, github_base_url)), or
	// "" when no row exists for that (user, host) pair. Used by the
	// predicate matcher (author_in / reviewer_in / commenter_in
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
	//
	// githubUserID is the account's numeric GitHub id in text form
	// (auth.GitHubUser.UserID). login records what the account is currently
	// called and githubUserID records which account it is; the second is the
	// half a rename doesn't touch. Passing "" (a host that reported no id, or
	// a caller that never had one) leaves any stored id intact rather than
	// blanking it, so the value fills in opportunistically on the next
	// capture. email is the verified primary address captured from GitHub.
	// Passing "" preserves an existing address when login is unchanged, for
	// legacy sources that do not carry one; when login changes, an omitted
	// email clears the prior account's address instead of carrying it across.
	//
	// Exempt from the returned-row rule: this store exposes the identity
	// tables as column projections, not rows — its reads answer "what login"
	// and "which account", and there is no point read whose column list and
	// scanner a RETURNING could share. Giving one a row type is a read-shape
	// change, not a write-shape convergence, and no caller reads back after
	// this write.
	UpsertGitHubIdentity(ctx context.Context, userID, githubBaseURL, login, githubUserID, email, source string) error

	// ClearGitHubIdentity deletes the user's GitHub identity row for a
	// host (the disconnect path — GitHub URL/PAT cleared in Settings).
	// No-op (nil error) when no row exists for that (user, host) pair.
	ClearGitHubIdentity(ctx context.Context, userID, githubBaseURL string) error

	// GetDisplayName returns users.display_name, or "" if NULL or the
	// row is missing. The team-members endpoint surfaces this in
	// Variant B's roster dropdown.
	GetDisplayName(ctx context.Context, userID string) (string, error)

	// GetProfile returns users.display_name + users.avatar_url together
	// (each "" when NULL or the row is missing) in one read. The usage
	// by-user / by-member breakdown uses it to render each person's name and
	// avatar without a second round-trip. avatar_url is populated from the
	// OAuth identity's claims at login (multi mode); it's typically empty in
	// local mode, where the roster falls back to a monogram.
	GetProfile(ctx context.Context, userID string) (displayName, avatarURL string, err error)

	// GetJiraIdentity returns the user's Jira (account_id, display_name)
	// on a specific host (user_jira_identities, keyed on (user_id,
	// jira_base_url)), both "" when no row exists for that (user, host)
	// pair. Used by the predicate matcher (assignee_in /
	// reporter_in / commenter_in allowlists), the stock handler's "is
	// this assigned to me" check, and the optimistic post-claim snapshot
	// update. Callers resolve the host from the org's
	// org_settings.jira_base_url (via jira.CanonicalHost) so the lookup
	// matches the host the binding was captured against. An absent row
	// degrades exactly as the old NULL jira_* columns did. The host is
	// normalized (db.NormalizeJiraHost) so a raw org_settings value and
	// the canonical capture-time host agree.
	GetJiraIdentity(ctx context.Context, userID, jiraBaseURL string) (accountID, displayName string, err error)

	// UpsertJiraIdentity writes (or refreshes) the user's Jira identity
	// for a host. The host is an explicit parameter so callers bind to
	// the org's host deliberately. account_id + display_name come from
	// one auth.ValidateJira call (Atlassian's /myself returns them
	// together); source records how the binding was captured ('pat' |
	// 'connect_oauth' | 'scim'); verified_at is stamped to now() on
	// every call (this IS the authenticated confirmation). Upserts on the
	// (user_id, jira_base_url) key. account_id is required — it's the
	// assignee-match key, so an empty one is rejected before touching the
	// DB (NOT NULL only catches SQL NULL, not ""). Passing "" for
	// display_name stores NULL. Returns an error when the user row does
	// not exist — capture paths own row creation. Mirrors
	// UpsertGitHubIdentity. App pool only: the sole writer
	// (handleJiraIdentityPAT) is a claims-bearing request handler, so
	// there's no `...System` variant yet — a future claims-free writer
	// (SCIM, a boot-time sync) would need one added, same as the GitHub
	// store.
	//
	// Exempt from the returned-row rule: same as UpsertGitHubIdentity — a
	// column-projection surface with no row read to project.
	UpsertJiraIdentity(ctx context.Context, userID, jiraBaseURL, accountID, displayName, source string) error

	// ClearJiraIdentity deletes the user's Jira identity row for a host
	// (the dedicated disconnect surface). No-op (nil error) when no row
	// exists for that (user, host) pair. Mirrors ClearGitHubIdentity.
	// Note: an org-credential disconnect deliberately does NOT call this
	// — identity is owned by its own capture surface, never swept as a
	// side effect of an org-access change. No live HTTP handler surfaces
	// this yet: the /jira/identity routes are status + PAT-bind only, so
	// a user-facing disconnect is still future capture-flow UI work
	// (symmetric with ClearGitHubIdentity, which is likewise unwired). It
	// exists now so that handler is a pure addition, not a store change.
	//
	// Exempt from the returned-row rule: it is a delete.
	ClearJiraIdentity(ctx context.Context, userID, jiraBaseURL string) error

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
	// login matches case-insensitively (lower(login), served by an index on
	// both dialects), because GitHub logins are case-insensitive while the
	// writers persist them as captured — the same treatment the sibling
	// user_identities email lookup gives lower(email). A verbatim compare
	// misses silently on capitalisation, which for a routing lookup reads as
	// "this person has no teams" rather than as an error.
	//
	// Admin pool / claims-free: system/router callers only. Exposing
	// this reverse lookup to a request handler would be a cross-user
	// identity probe (resolve any login → any user); a request-path
	// consumer would need a separate claims-scoped method with its own
	// RLS story, and none is needed today. SQLite collapses to one
	// connection.
	UserIDsForGitHubLoginSystem(ctx context.Context, githubBaseURL, login string) ([]string, error)

	// DashboardBackfilledAtSystem returns when the one-shot dashboard-history
	// backfill last completed for this (user, host) GitHub identity, or nil
	// when it hasn't run (NULL marker) or no identity row exists. It gates the
	// multi-mode backfill so a re-poll or repeated dashboard opens don't
	// re-fire the per-installation search burst (TFAC-396). Admin pool /
	// claims-free: the only caller is the background backfill worker, which
	// runs detached from any request and carries no JWT-claims context.
	DashboardBackfilledAtSystem(ctx context.Context, userID, githubBaseURL string) (*time.Time, error)

	// MarkDashboardBackfilledSystem stamps dashboard_backfilled_at = now() for
	// this (user, host) identity, recording that the backfill of login's history
	// completed so it won't run again for that login. login guards the write:
	// the stamp lands ONLY if the row's current login still equals it. The
	// backfill runs detached and can outlive a re-bind — if the user rebinds to a
	// different login mid-flight, the in-flight goroutine carries the OLD login,
	// and stamping unconditionally would mark the NEW login backfilled when its
	// history was never seeded, stranding it forever. The login-guarded write
	// no-ops in that case, leaving the marker NULL so the new login re-seeds on
	// the next trigger (TFAC-396). No-op (nil error) when no row exists or the
	// login no longer matches. Admin pool / claims-free, same caller and
	// rationale as DashboardBackfilledAtSystem. UpsertGitHubIdentity also clears
	// this marker when the bound login changes (the already-completed case);
	// together they make a rename always re-backfill the new login.
	//
	// Exempt from the returned-row rule: fire-and-forget bookkeeping. The
	// stamp is a one-shot marker the next backfill trigger reads; the guarded
	// no-op is the outcome the caller wants and it already gets it as "no
	// error".
	MarkDashboardBackfilledSystem(ctx context.Context, userID, githubBaseURL, login string) error

	// GetJiraIdentitySystem mirrors GetJiraIdentity but routes through
	// the admin pool in Postgres. The router's inline close-check on
	// Jira reassignment consumes this from its eventbus subscriber
	// goroutine, which has no JWT-claims context. Callers resolve the host
	// the same way GetJiraIdentity's do (org_settings.jira_base_url).
	GetJiraIdentitySystem(ctx context.Context, userID, jiraBaseURL string) (accountID, displayName string, err error)

	// UserIDsForJiraAccountSystem returns every TF user bound to accountID
	// on jiraBaseURL in user_jira_identities — the reverse of
	// GetJiraIdentity, and the Jira twin of UserIDsForGitHubLoginSystem.
	// Host-scoped, org-agnostic (identity is keyed on (user_id,
	// jira_base_url), no org). Empty slice (not error) when no binding
	// exists. The assignee-routing router consumes it from its eventbus
	// subscriber goroutine, which carries no JWT-claims context.
	//
	// Returns a set deliberately. The table's PK is (user_id,
	// jira_base_url) — it constrains uniqueness per user, so nothing stops
	// two TF users binding the same Atlassian account on one host (a shared
	// service account, a stale pre-rename row). Callers union the resulting
	// teams. jiraBaseURL is required and normalized the same way writes are
	// (db.NormalizeJiraHost), so the key matches what the capture paths
	// stored. source / verified_at are out of scope: every matching row is
	// returned regardless of how it was captured or whether it has been
	// host-verified.
	//
	// Admin pool / claims-free: system/router callers only. Exposing this
	// reverse lookup to a request handler would be a cross-user identity
	// probe (resolve any account → any user); a request-path consumer would
	// need a separate claims-scoped method with its own RLS story, and none
	// is needed today. SQLite collapses to one connection.
	UserIDsForJiraAccountSystem(ctx context.Context, jiraBaseURL, accountID string) ([]string, error)

	// UserIDsForVerifiedEmailSystem returns the principal user id(s) holding a
	// VERIFIED login-identity email equal to email (case-insensitive), on the
	// admin pool — the set-valued formalization of the inline auto-link query the
	// login path uses (internal/server/auth_handlers.go's resolveOrCreatePrincipal).
	// Set-valued because one verified email can appear on identities of more than
	// one principal; the caller decides what ambiguity means (the Slack identity
	// resolver, TFAC-531, treats it as unresolved and never guesses). Empty slice,
	// not error, on no match. email is lowercased/trimmed here so callers pass it
	// as captured.
	//
	// Admin pool / claims-free: the only caller (today) is the Slack identity
	// resolver, which runs detached from any request with no JWT-claims context.
	// Postgres reads public.user_identities directly (the auth-principal bridge;
	// it is NOT part of this store's own tables). SQLite has no auth-principal
	// bridge — local mode is N=1 with no login concept — so it always returns
	// (nil, nil); best-effort callers already treat an empty result as no-match.
	UserIDsForVerifiedEmailSystem(ctx context.Context, email string) ([]string, error)

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
	//
	// Exempt from the returned-row rule: the users table is a column-
	// projection surface here (GetDisplayName / GetProfile answer with fields,
	// not a row), so there is no point read a RETURNING could share a column
	// list with.
	SetLastActingTeam(ctx context.Context, userID, teamID string) error

	// GetSettings returns the user's settings row — the per-user prefs; the
	// AI model + auto-delegate toggle moved to team_settings. Returns a
	// zero-value UserSettings with nil error when no row exists yet, which is
	// what every field's absent value already means, so a caller never has to
	// tell "no row" from "nothing set". Postgres runs on the app pool
	// (user_settings_select RLS gates by user_id = current_user_id()).
	GetSettings(ctx context.Context, userID string) (domain.UserSettings, error)

	// UpdateSettings upserts the user's settings row and returns what it
	// persisted. updates is the WHOLE row's desired end state, not a delta:
	// every field lands as given, so a nil one clears its column. A caller
	// applying a partial write reads first and applies onto what it read — the
	// PATCH handler does exactly that, inside the write's own transaction.
	// Postgres runs on the app pool (user_settings_modify RLS gates by
	// user_id = current_user_id()).
	UpdateSettings(ctx context.Context, userID string, updates domain.UserSettings) (domain.UserSettings, error)
}
