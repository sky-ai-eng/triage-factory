package db

import (
	"context"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ErrTeamMemberNotFound is returned by TeamsStore.ChangeMemberRole /
// RemoveMember when no membership row matches (team_id, user_id) — so a stale
// or bogus user_id surfaces as a 404 rather than a silent no-op. The handlers
// gate team-admin (or self) before calling, so a zero-row result past that
// gate means the target simply isn't on the team. The team-tier twin of
// ErrOrgMemberNotFound.
var ErrTeamMemberNotFound = errors.New("db: team member not found")

// ErrLastTeamAdminGuard is returned by TeamsStore.ChangeMemberRole /
// RemoveMember when the write would leave the team with no 'admin' role. The
// authority is the tf.guard_team_admins statement trigger (Postgres baseline),
// which raises SQLSTATE 23514; the store translates that into this sentinel so
// the handler can answer 409 without importing the pg driver. The invariant is
// never re-implemented in Go — the DB is the single source of truth. Mirrors
// ErrLastOwnerGuard for the org tier.
var ErrLastTeamAdminGuard = errors.New("db: each team must retain at least one admin")

// ErrTeamMemberExists is returned by TeamsStore.AddMember when the user is
// already on the team (the memberships PK (user_id, team_id) collides). The
// member-picker only offers org members not already on the team, so this is
// the defensive backstop for a race; the handler maps it to a 409.
var ErrTeamMemberExists = errors.New("db: user is already a member of this team")

// TeamsStore owns the teams + team_settings tables — the membership
// unit inside an org and its sibling settings row. Most resources
// (tasks, runs, projects, prompts) carry a team_id FK; the request-
// handler sites that synthesize new rows pick the right team for the
// requesting org via GetDefaultForOrgSystem. Per-team settings (AI
// thresholds, default model, auto-delegate toggle, tracked Jira
// projects) live on team_settings and are read/written via the
// settings methods.
//
// # Pool split (Postgres)
//
//   - GetDefaultForOrgSystem, GetSettingsSystem, and
//     TeamIDsForUserInOrgSystem run on the admin pool. The boot-time
//     pollers/scorer/delegation spawner and the routing eventbus
//     subscriber have no JWT-claims context.
//   - GetSettings and UpdateSettings run on the app pool. The
//     team_settings_select / team_settings_update RLS policies gate
//     reads by team membership (via memberships) and writes by team
//     admin; the request-handler caller has set the JWT claims via
//     the TxRunner.
//   - The roster methods (TFAC-444) split like
//     OrgMembershipsStore: the membership read + the AddMember /
//     ChangeMemberRole / RemoveMember mutations run on the app pool
//     (memberships_select / _insert / _update / _delete RLS gate read
//     by org access and writes by team-admin-or-org-admin), with the
//     tf.guard_team_admins trigger the sole authority on the last-admin
//     invariant; ListMembers' cross-member GitHub/Jira identity
//     enrichment runs on the admin pool (the self-only identity RLS
//     can't express it, scoped back to the team by a membership join).
//
// SQLite collapses the pool split to one connection; the `...System`
// variants delegate to their non-System counterparts. The roster
// methods are multi-mode only (the team-roster handlers 404 in local);
// the SQLite impls are stubs returning ErrNotApplicableInLocal.
type TeamsStore interface {
	// GetDefaultForOrg returns the ID of the org's default team —
	// defined as the oldest team row by created_at. Same shape as
	// GetDefaultForOrgSystem but routes through the app pool in
	// Postgres so teams_select RLS (org_id = current_org_id() AND
	// user_has_org_access) fires under the caller's claims. Use
	// this from request handlers; use the System variant from
	// background services with no claims context.
	GetDefaultForOrg(ctx context.Context, orgID string) (string, error)

	// GetDefaultForOrgSystem returns the ID of the org's default
	// team — defined as the oldest team row by created_at. The
	// single-team-per-org assumption means there's only one row to
	// pick in practice; the ORDER BY is the tiebreaker for any
	// future fixture that seeds multiple teams (and pins behavior
	// at "the original team wins" rather than non-deterministic).
	//
	// Returns the empty string with a nil error if the org has no
	// teams. Callers treat that as a hard error — every org gets a
	// default team at create time (multi-mode via SKY-257 D14 org
	// provisioning; local mode via the baseline migration),
	// and a teamless org is a bootstrap bug.
	GetDefaultForOrgSystem(ctx context.Context, orgID string) (string, error)

	// GetSettings returns the team's settings row. On sql.ErrNoRows
	// it falls back to domain.DefaultTeamSettings() (matching the
	// schema DEFAULT clauses) so callers see a populated struct
	// rather than the Go zero value with empty model + zero thresholds
	// + auto_delegate=false. Row-missing is a test-fixture-only case;
	// production paths seed a row at team-create time. JiraProjects
	// is a denormalized fast path keyed `(team_id, project_key)`;
	// the per-project status rules live on JiraStatusRulesStore.
	// Postgres routes through the app pool (team_settings_select RLS
	// gates by team membership).
	GetSettings(ctx context.Context, teamID string) (domain.TeamSettings, error)

	// GetSettingsSystem mirrors GetSettings but routes through the
	// admin pool in Postgres for callers without a JWT-claims context
	// (poller manager, scorer, delegation spawner). SQLite collapses
	// to the same impl. Same defaults-on-ErrNoRows contract.
	GetSettingsSystem(ctx context.Context, teamID string) (domain.TeamSettings, error)

	// UpdateSettings upserts the team's settings row. JiraProjects is
	// persisted verbatim as the denormalized fast path; callers that
	// also want the per-project rules to stay in sync must call
	// JiraStatusRulesStore.ReplaceForTeam alongside (the existing
	// config.Save() flow does both). Postgres routes through the app
	// pool (team_settings_update RLS gates by team admin).
	UpdateSettings(ctx context.Context, teamID string, updates domain.TeamSettings) error

	// ListForUser returns the requesting user's teams in the org,
	// ordered oldest-first (the same created_at tiebreak the default-
	// team pick uses, so teams[0] is the org's default team). This is
	// the data source for the multi-team selectors: the frontend
	// renders a team control only when the count is ≥2.
	//
	// Postgres joins memberships under the caller's claims — teams_select
	// RLS alone returns *every* team in the org (it gates on org access,
	// not team membership), so the membership join is what narrows the
	// result to the teams the user actually belongs to. SQLite is N=1
	// (single synthetic user) and returns every team in the org, which
	// is the same set. Routes through the app pool in Postgres.
	ListForUser(ctx context.Context, orgID string) ([]domain.Team, error)

	// TeamIDsForUserInOrgSystem returns the ids of every team in orgID
	// that userID belongs to (memberships ⋈ teams WHERE teams.org_id =
	// orgID), ordered by team id. Claims-free admin-pool sibling of
	// ListForUser, which resolves the *requesting* user under RLS — this
	// takes an explicit, arbitrary userID for the author-/reviewer-
	// routing routers, which run in an eventbus subscriber goroutine
	// with no JWT claims. Empty slice when the user is in the org on no
	// teams, or not in the org at all.
	//
	// The team set comes from the team-level memberships table
	// (memberships(user_id, team_id, role)) joined to teams filtered by
	// teams.org_id = orgID — not org_memberships, which is org-level
	// ((user_id, org_id, role), no team_id). Scoping to orgID is what
	// filters out a same-login user who belongs to a different org on the
	// same host (legitimate on github.com, where many orgs share one
	// host).
	//
	// Admin pool / claims-free: system/router callers only; never
	// touches current_user_id(). A request-path consumer would need a
	// separate claims-scoped method with its own RLS story, and none is
	// needed today. SQLite collapses to one connection.
	TeamIDsForUserInOrgSystem(ctx context.Context, orgID, userID string) ([]string, error)

	// Create inserts a new team in the org and enrolls the creator as a
	// team admin in the same transaction, returning the new row. The
	// org-admin "add team" affordance is how a solo hosted user grows
	// past one team (at which point the count-gated selectors begin
	// rendering). Postgres routes through the app pool: teams_insert RLS
	// gates org-admin, and the sibling memberships insert is permitted
	// because the creator is an org admin (user_is_org_admin_via_team).
	// Callers must have already confirmed org-admin privilege; the RLS
	// check is the backstop, not the primary gate.
	Create(ctx context.Context, orgID, name, slug, creatorUserID string) (domain.Team, error)

	// ListMembers returns every member of teamID with their team role and
	// host-scoped identity readiness, ordered by display name then user id
	// for a stable roster — the team-tier sibling of
	// OrgMembershipsStore.ListWithIdentity. githubBaseURL / jiraBaseURL are
	// the org's configured hosts (raw, read from org_settings by the caller);
	// the impl resolves them to the host identities are keyed under
	// (EffectiveGitHubHost: an unset github_base_url resolves to github.com;
	// NormalizeJiraHost: an unset jira_base_url matches nothing). A member's
	// GitHubUsername / JiraAccountID is nil when they hold no binding on that
	// host. The roster reads under the app pool (memberships_select RLS — any
	// org member may read a team-in-org's roster); the identity enrichment
	// reads under the admin pool (the self-only identity RLS can't express a
	// cross-member read, scoped back to the team by a membership join).
	ListMembers(ctx context.Context, teamID, githubBaseURL, jiraBaseURL string) ([]domain.TeamMember, error)

	// AddMember enrolls userID on teamID with role ("admin" | "member" |
	// "viewer"). App pool: memberships_insert RLS gates the write to team
	// admins or org admins. Returns ErrTeamMemberExists when the user is
	// already on the team (the PK collides). Callers validate role against the
	// allowed set and confirm the target is an org member before calling.
	AddMember(ctx context.Context, teamID, userID, role string) error

	// ChangeMemberRole sets userID's team role on teamID. App pool:
	// memberships_update RLS gates the write to team admins or org admins.
	// Returns ErrLastTeamAdminGuard when the change would drop the team's last
	// admin (the guard trigger fires), and ErrTeamMemberNotFound when no row
	// matches. Callers validate role before calling so an invalid enum never
	// reaches the column.
	ChangeMemberRole(ctx context.Context, teamID, userID, role string) error

	// RemoveMember deletes userID's membership on teamID. App pool:
	// memberships_delete RLS permits the caller to remove themselves
	// (self-leave, any role) or, as a team/org admin, anyone. Returns
	// ErrLastTeamAdminGuard when removing the team's last admin (guard
	// trigger), and ErrTeamMemberNotFound when no row matches.
	RemoveMember(ctx context.Context, teamID, userID string) error
}
