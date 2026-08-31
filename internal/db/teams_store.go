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

// ErrTeamNotFound is returned by TeamsStore.Update when no team row matches
// the id under the caller's RLS scope — a cross-org id (invisible to the app
// pool) or a team deleted between the handler's VerifyTeamInOrg gate and the
// write. The handler maps it to a 404. The handler's front gate makes this a
// defensive backstop rather than the primary not-found path.
var ErrTeamNotFound = errors.New("db: team not found")

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
//   - The roster methods split like OrgMembershipsStore: the membership
//     read + the AddMember / ChangeMemberRole / RemoveMember mutations
//     run on the app pool (memberships_select / _insert / _update /
//     _delete RLS gate read by org access and writes by
//     team-admin-or-org-admin), with the tf.guard_team_admins trigger
//     the sole authority on the last-admin invariant; ListMembers'
//     cross-member GitHub/Jira identity enrichment runs on the admin
//     pool (the self-only identity RLS can't express it, scoped back to
//     the team by a membership join).
//
// SQLite collapses the pool split to one connection; the `...System`
// variants delegate to their non-System counterparts. ListMembers is
// implemented in both dialects — local mode reads its own roster, which
// is one member — while the roster WRITES are multi-mode only (the
// mutating handlers 404 in local) and their SQLite impls are stubs
// returning ErrNotApplicableInLocal.
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
	// default team at create time (multi-mode via org
	// provisioning; local mode via the baseline migration),
	// and a teamless org is a bootstrap bug.
	GetDefaultForOrgSystem(ctx context.Context, orgID string) (string, error)

	// GetSettings returns the team's settings row. On sql.ErrNoRows
	// it falls back to domain.DefaultTeamSettings() (matching the
	// schema DEFAULT clauses) so callers see a populated struct
	// rather than the Go zero value with empty model + zero thresholds
	// + auto_delegate=true. Row-missing is a test-fixture-only case;
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
	// pool (team_settings_update RLS gates by team admin). It writes
	// MaxDailyCostUSD too, but the team-settings handler reaches this via
	// read-modify-write and never sets that field from its request body, so a
	// team-admin save round-trips the org-admin-configured cap untouched — it is
	// *changed* only by SetDailyCostCapSystem.
	//
	// Returns the persisted settings, read off RETURNING on the write
	// statement itself rather than from a follow-up SELECT and projecting
	// GetSettings' column list and scanner.
	UpdateSettings(ctx context.Context, teamID string, updates domain.TeamSettings) (domain.TeamSettings, error)

	// SetDailyCostCapSystem upserts ONLY team_settings.max_daily_cost_usd for
	// teamID — the org-admin per-team daily LLM spend cap (TFAC-482, the team-
	// scoped sibling of OrgSettings.MaxDailyCostUSD). capUSD ≤ 0 clears the cap
	// (stored NULL). Admin pool in Postgres (BYPASSRLS): an org admin configuring
	// a team's cap may not be a member of that team, so the team-admin-gated
	// team_settings_update RLS would reject an app-pool write — the HTTP
	// RequireOrgAdminRole gate authorizes this System path. It touches only the
	// cap column (the other settings are never clobbered) and creates the row from
	// schema defaults when none exists. SQLite is N=1 / no RLS and writes directly.
	//
	// Returns the whole persisted settings row, which is the point: the write
	// touches one column and the row it lands in may have just been created
	// from schema defaults, so nothing the caller holds describes it. Two
	// admins racing on the same cap each get the value that actually landed
	// rather than an echo of their own request.
	SetDailyCostCapSystem(ctx context.Context, teamID string, capUSD float64) (domain.TeamSettings, error)

	// ListForUser returns one page of the requesting user's active teams in
	// the org plus the unpaged total, ordered oldest-first (the same
	// created_at tiebreak the default-team pick uses, with an id tiebreaker
	// so the order is total and the pages partition it). This is the data
	// source for the multi-team selectors: the frontend renders a team
	// control only when the count is ≥2. Archived teams (deleted_at IS NOT
	// NULL) are filtered out — the request-facing read filter that makes an
	// archived team vanish from every selector (TFAC-448); the
	// archive/restore lifecycle paths use the unfiltered ...System reads.
	//
	// Postgres joins memberships under the caller's claims — teams_select
	// RLS alone returns *every* team in the org (it gates on org access,
	// not team membership), so the membership join is what narrows the
	// result to the teams the user actually belongs to. SQLite is N=1
	// (single synthetic user) and returns every team in the org, which
	// is the same set. Routes through the app pool in Postgres.
	ListForUser(ctx context.Context, orgID string, opts ListOpts) ([]domain.Team, int, error)

	// Get returns a single team by id under the caller's claims, or (nil,
	// nil) when no row matches. Role carries the CALLER's membership role and
	// is empty when they are not on the team — teams_select RLS gates on org
	// access rather than membership, so an org admin reads a team they never
	// joined and gets an empty role rather than a 404. Description and
	// DeletedAt are populated: this is the canonical single read, and the
	// description column had no reader at all before it (only the PATCH
	// response echoed it back).
	//
	// App pool in Postgres, so a cross-org id is invisible and answers
	// not-found. SQLite is N=1 and reports the sole user as 'admin', matching
	// ListForUser.
	Get(ctx context.Context, orgID, teamID string) (*domain.Team, error)

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
	//
	// Archived teams (deleted_at IS NOT NULL) are excluded (TFAC-448): the
	// router routes new tasks to the returned teams, and an archived team has
	// been force-stopped + write-blocked, so it must not receive new work.
	TeamIDsForUserInOrgSystem(ctx context.Context, orgID, userID string) ([]string, error)

	// Create inserts a new team in the org and enrolls the creator as a
	// team admin in the same transaction, returning the new row. The
	// org-admin "add team" affordance is how a solo multi-mode user grows
	// past one team (at which point the count-gated selectors begin
	// rendering). Postgres routes through the app pool: teams_insert RLS
	// gates org-admin, and the sibling memberships insert is permitted
	// because the creator is an org admin (user_is_org_admin_via_team).
	// Callers must have already confirmed org-admin privilege; the RLS
	// check is the backstop, not the primary gate.
	Create(ctx context.Context, orgID, name, slug, creatorUserID string) (domain.Team, error)

	// Update renames teamID and/or rewrites its description, returning the
	// updated row. Each argument is optional (nil = leave that column
	// untouched, the partial-PATCH contract) and applied via COALESCE; a
	// non-nil empty-string description clears the blurb. The caller computes
	// slug from the new name (slugify, the same derivation Create uses) and
	// passes it alongside name so the two stay in sync — the body carries no
	// slug of its own. Returns ErrTeamNotFound when no row matches under RLS,
	// and surfaces the UNIQUE (org_id, slug) collision verbatim (the handler
	// maps it to 409, mirroring Create). Postgres routes through the app pool:
	// the teams_update RLS policy gates the write to team-admin-or-org-admin.
	Update(ctx context.Context, teamID string, name, slug, description *string) (domain.Team, error)

	// Archive soft-deletes teamID by stamping deleted_at = now(), but only when
	// the team is currently active (deleted_at IS NULL). Returns ErrTeamNotFound
	// when no active row matches under RLS — already archived, or invisible /
	// cross-org. The team's durable work (tasks, runs, memory) is never touched;
	// archive only sets the tombstone, and the force-stop cascade runs in the
	// handler. Postgres routes through the app pool: teams_update RLS gates the
	// write to team-admin-or-org-admin, and the handler additionally restricts to
	// org-admin (TFAC-448).
	//
	// Returns the archived row — carrying the deleted_at it stamped — so the
	// handler answers with the team the write produced rather than patching
	// the tombstone onto a copy it read beforehand. Team.Role is left empty:
	// it is the *caller's* membership role, which Get joins on and no column
	// of the teams row carries, so it is not part of what a write returns.
	Archive(ctx context.Context, teamID string) (domain.Team, error)

	// Restore clears teamID's deleted_at (back to NULL), but only when the team
	// is currently archived (deleted_at IS NOT NULL). Returns ErrTeamNotFound when
	// no archived row matches. Restore makes the team visible + writable again; it
	// deliberately does NOT resurrect the runs that archive
	// force-stopped — those stay terminal. App pool: teams_update RLS (org-admin
	// via the handler gate).
	//
	// Returns the restored row, with the same Role caveat as Archive.
	Restore(ctx context.Context, teamID string) (domain.Team, error)

	// GetSystem returns a single team by id WITHOUT the request-facing
	// deleted_at read filter (so an archived team resolves), or nil when no row
	// matches orgID/teamID. DeletedAt is populated. Admin pool: the archive /
	// restore / preview paths read the team's lifecycle state from a handler
	// whose caller is an org admin who may not be a member of the team, and the
	// in-flight reaping has no team-membership claim to lean on. orgID stays in
	// the WHERE clause as defense in depth.
	GetSystem(ctx context.Context, orgID, teamID string) (*domain.Team, error)

	// ListArchivedForOrgSystem returns one page of the org's archived teams
	// (deleted_at IS NOT NULL) plus the unpaged total, most-recently-archived
	// first with an id tiebreaker, with DeletedAt populated. Admin pool /
	// org-scoped: the org-admin "Archived teams" restore surface enumerates
	// them even for teams the admin never joined (the per-user membership join
	// ListForUser uses would hide those). Empty slice when the org has no
	// archived teams.
	ListArchivedForOrgSystem(ctx context.Context, orgID string, opts ListOpts) ([]domain.Team, int, error)

	// ListActiveForOrgSystem returns the org's ACTIVE teams (deleted_at IS NULL),
	// ordered by name, on the admin pool — the org-scoped sibling of
	// ListArchivedForOrgSystem. The TFAC-482 governance cap editor lists every team
	// (not just those with spend in the window) so an org admin can pre-cap an idle
	// team before any runaway, and crosses teams the admin may not be a member of —
	// the per-user-scoped ListForUser would hide those. Empty slice for a teamless
	// org (a bootstrap bug). DeletedAt is left nil (all rows are active).
	ListActiveForOrgSystem(ctx context.Context, orgID string) ([]domain.Team, error)

	// ListActiveCapsForOrgSystem returns one page of the org's active teams
	// with each one's configured per-team daily cost cap, ordered by name with
	// an id tiebreaker, plus the total. Admin pool, org-scoped: the governance
	// cap editor crosses teams its org admin may not belong to.
	//
	// The cap is joined, not looked up per team. The editor used to call
	// ListActiveForOrgSystem and then GetSettingsSystem once per row, so the
	// query count grew with the org's team count on every render of one panel
	// — and a page of that list would have been a page of teams with a full
	// scan of settings behind it. A missing team_settings row means the team
	// has never been configured, which is a nil cap (no cap), the same answer
	// the per-team read's ErrNoRows default gave.
	ListActiveCapsForOrgSystem(ctx context.Context, orgID string, opts ListOpts) ([]domain.TeamCap, int, error)

	// NamesForIDsSystem resolves id->name for exactly the given team IDs
	// (deduped; blanks ignored), on the admin pool — the narrow cross-team
	// disclosure read a caller needs when it already knows WHICH teams it
	// cares about (e.g. the Slack channel-tracking API's tracked_by, which
	// only needs names for the handful of teams that actually track at
	// least one channel) and would otherwise over-fetch by calling
	// ListActiveForOrgSystem and discarding most of it. An id that doesn't
	// resolve (unknown, or belongs to a different org) is simply absent
	// from the result rather than an error. Empty input returns an empty
	// map without a query.
	NamesForIDsSystem(ctx context.Context, orgID string, teamIDs []string) (map[string]string, error)

	// MemberIDsSystem returns the distinct, sorted ids of every user enrolled
	// on teamID — the websocket delivery scope for a team_knowledge_updated
	// event whose changed root is `private`, which is readable through this
	// team's gate and nowhere else. orgID is checked in the WHERE clause as
	// defense in depth so a team id from another tenant resolves to nobody.
	//
	// Every role is included, viewers too: a viewer reads the team's KB page
	// and must not be left looking at a stale listing. Archived teams are
	// included for the same reason repo recipients include them — archiving
	// force-stops a team's work without hiding anything it can already see.
	//
	// Admin pool in Postgres: the emitter is a claims-free notifier called at
	// the write site, and the answer must not vary with who happened to make
	// the change. Called fresh per emission, never cached, so a membership
	// change takes effect on the next event — a connection-captured audience
	// goes stale into exactly the disclosure this scoping prevents. Local mode
	// never calls it (N=1 broadcasts org-wide).
	MemberIDsSystem(ctx context.Context, orgID, teamID string) ([]string, error)

	// ListMembers returns one page of teamID's members with their team role
	// and host-scoped identity readiness, plus the unpaged total, ordered by
	// display name then user id for a stable roster — the team-tier sibling of
	// OrgMembershipsStore.ListWithIdentity. The user-id tiebreaker is what
	// makes offset paging total: display names collide freely, and a partial
	// order drops and repeats rows across pages. githubBaseURL / jiraBaseURL
	// are the org's configured hosts (raw, read from org_settings by the
	// caller); the impl resolves them to the host identities are keyed under
	// (EffectiveGitHubHost: an unset github_base_url resolves to github.com;
	// NormalizeJiraHost: an unset jira_base_url matches nothing). A member's
	// GitHubUsername / JiraAccountID is nil when they hold no binding on that
	// host.
	//
	// Postgres: the roster reads under the app pool (memberships_select RLS —
	// any org member may read a team-in-org's roster); the identity enrichment
	// reads under the admin pool (the self-only identity RLS can't express a
	// cross-member read, scoped back to the team by a membership join).
	// SQLite is N=1 — one implicit user, enrolled on the sole team by the
	// local tenant seed — so the same query shape answers with that single
	// synthetic member. Local mode runs the roster's own consumers (the
	// assignee picker, the predicate editor's variant choice), so this is a
	// read both dialects owe an answer to.
	ListMembers(ctx context.Context, teamID, githubBaseURL, jiraBaseURL string, opts ListOpts) ([]domain.TeamMember, int, error)

	// AddMember enrolls userID on teamID with role ("admin" | "member" |
	// "viewer"). App pool: memberships_insert RLS gates the write to team
	// admins or org admins. Returns ErrTeamMemberExists when the user is
	// already on the team (the PK collides). Callers validate role against the
	// allowed set and confirm the target is an org member before calling.
	//
	// Exempt from the returned-row rule: memberships is exposed here only
	// through ListMembers, an identity-enriched roster read that joins two
	// other tables per member — there is no plain row read whose column list
	// and scanner a RETURNING could share, and the enrichment is not something
	// an insert can produce. ErrTeamMemberExists already answers "did this
	// land".
	AddMember(ctx context.Context, teamID, userID, role string) error

	// ChangeMemberRole sets userID's team role on teamID and returns the prior
	// role (so the governance audit log can record the old→new transition). App
	// pool: memberships_update RLS gates the write to team admins or org admins.
	// Returns ErrLastTeamAdminGuard when the change would drop the team's last
	// admin (the guard trigger fires), and ErrTeamMemberNotFound when no row
	// matches. Callers validate role before calling so an invalid enum never
	// reaches the column.
	ChangeMemberRole(ctx context.Context, teamID, userID, role string) (oldRole string, err error)

	// RemoveMember deletes userID's membership on teamID. App pool:
	// memberships_delete RLS permits the caller to remove themselves
	// (self-leave, any role) or, as a team/org admin, anyone. Returns
	// ErrLastTeamAdminGuard when removing the team's last admin (guard
	// trigger), and ErrTeamMemberNotFound when no row matches.
	//
	// Exempt from the returned-row rule: it is a delete.
	RemoveMember(ctx context.Context, teamID, userID string) error
}
