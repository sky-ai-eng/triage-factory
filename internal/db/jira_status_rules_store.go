package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// JiraStatusRulesStore owns the jira_project_status_rules table —
// one row per (team_id, project_key) carrying the team's per-project
// pickup/in_progress/done status configuration. Separate from
// TeamsStore because the table is multi-row per team with bulk-replace
// semantics (config.Save's clone-then-prune flow); folding it into
// the single-row TeamSettings struct would muddy the per-scope read
// shape.
//
// # Pool split (Postgres)
//
//   - ListForTeam, ReplaceForTeam run on the app pool. The
//     jira_rules_select / jira_rules_insert / jira_rules_update /
//     jira_rules_delete RLS policies gate reads by team membership
//     and writes by team admin; the request-handler caller has set
//     the JWT claims via the TxRunner.
//   - ListForTeamSystem, ListForOrgSystem, TracksProjectSystem run on
//     the admin pool. The poller manager / scorer / router gate read
//     rules at boot/poll-tick without a JWT-claims context.
//
// SQLite collapses the pool split to one connection; the `...System`
// variant delegates to its non-System counterpart.
//
// Every List* method populates domain.JiraProjectStatusRules.TeamID (the
// PK's first column) so the poller's per-project merge and the router's
// team↔project gate can attribute each row to its team.
type JiraStatusRulesStore interface {
	// ListForTeam returns the team's per-project rules in
	// project_key ascending order. Empty slice with nil error when
	// the team has no rules configured. Postgres routes through the
	// app pool.
	ListForTeam(ctx context.Context, teamID string) ([]domain.JiraProjectStatusRules, error)

	// ListForTeamSystem mirrors ListForTeam but routes through the
	// admin pool in Postgres for callers without a JWT-claims context.
	ListForTeamSystem(ctx context.Context, teamID string) ([]domain.JiraProjectStatusRules, error)

	// ListForOrgSystem returns every team's per-project rules across the
	// org — the UNION the poller discovers, so a non-default team's Jira
	// config is polled too. One row per (team_id, project_key); the
	// caller (toTrackerJiraRules) merges members per project_key. Ordered
	// by (project_key, team_id) for a deterministic merge. Admin pool in
	// Postgres: the union spans teams via the teams.org_id join
	// (jira_project_status_rules has no org_id column). Mirrors
	// TeamGitHubReposStore.ListForOrgSystem.
	ListForOrgSystem(ctx context.Context, orgID string) ([]domain.JiraProjectStatusRules, error)

	// TracksProjectSystem reports whether the team has a
	// jira_project_status_rules row for projectKey. This is the router
	// team↔project gate lookup. Admin pool in Postgres: the router
	// goroutine has no JWT claims. Mirrors
	// TeamGitHubReposStore.TracksRepoSystem.
	TracksProjectSystem(ctx context.Context, teamID, projectKey string) (bool, error)

	// ReplaceForTeam upserts one row per entry in rules and deletes
	// rows whose project_key is no longer in the input — matches the
	// bulk-replace semantics of the existing config.Save() flow. The
	// whole replace runs inside a single transaction so the table
	// can't observe a partial mid-sync state. Passing an empty rules
	// slice deletes every row for the team. Postgres routes through
	// the app pool (jira_rules_insert / _update / _delete RLS gate
	// writes by team admin).
	ReplaceForTeam(ctx context.Context, teamID string, rules []domain.JiraProjectStatusRules) error
}
