package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

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
//   - GetDefaultForOrgSystem and GetSettingsSystem run on the admin
//     pool. The boot-time pollers/scorer/delegation spawner have no
//     JWT-claims context.
//   - GetSettings and UpdateSettings run on the app pool. The
//     team_settings_select / team_settings_update RLS policies gate
//     reads by team membership (via memberships) and writes by team
//     admin; the request-handler caller has set the JWT claims via
//     the TxRunner.
//
// SQLite collapses the pool split to one connection; the `...System`
// variants delegate to their non-System counterparts.
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
	// provisioning; local mode via the v1.11.0 baseline migration),
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
}
