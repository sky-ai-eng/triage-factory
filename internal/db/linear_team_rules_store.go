package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// LinearTeamRulesStore owns the linear_team_rules table: one row per (team_id,
// linear_team_id) carrying the team's pickup / in_progress / done rules for one
// Linear team. The Linear sibling of JiraStatusRulesStore, with a Linear team
// as the tracked unit where Jira has a project.
//
// A row is the team's commitment to watch the Linear team, armed or not (see
// domain.LinearTeamRules). team_settings.linear_teams carries the display
// order; this table is the truth for membership.
//
// # Pool split (Postgres)
//
//   - ListForTeam and ReplaceForTeam run on the app pool. The linear_rules_*
//     RLS policies gate reads by team membership and writes by team admin,
//     under the claims the request handler's TxRunner set.
//   - ListForTeamSystem, ListForOrgSystem and TracksTeamSystem run on the
//     admin pool, for the poller and the router, which carry no claims.
//
// SQLite collapses the split to one connection.
//
// Every List* method populates domain.LinearTeamRules.TeamID, so the poller's
// per-Linear-team merge and the router's team gate can attribute each row to
// its team.
//
// # Workspace
//
// A row names the Linear team and workflow state ids of the workspace it was
// saved under, and no other workspace has those ids. Every method therefore
// takes the workspace id — the org's current linear_workspace_id, through
// domain.EntityScope — and sees only that workspace's rows: after the org's
// credential moves to another workspace, the old rows stop applying without
// being deleted, and binding the old workspace again brings them back. An
// empty workspace id (no Linear credential bound) reads as no rows. This is
// the tracked-set rule for a provider namespace change: forward-only, and
// nothing a save did not name is destroyed.
type LinearTeamRulesStore interface {
	// ListForTeam returns the team's rows in workspaceID ordered by
	// linear_team_id. An empty slice with a nil error when the team tracks no
	// Linear team there.
	ListForTeam(ctx context.Context, teamID, workspaceID string) ([]domain.LinearTeamRules, error)

	// ListForTeamSystem is ListForTeam on the admin pool.
	ListForTeamSystem(ctx context.Context, teamID, workspaceID string) ([]domain.LinearTeamRules, error)

	// ListForOrgSystem returns every team's rows in workspaceID across the
	// org, ordered by (linear_team_id, team_id): the union the poller merges
	// per Linear team. Admin pool; the org scope rides the teams join, since
	// the table carries no org_id of its own.
	ListForOrgSystem(ctx context.Context, orgID, workspaceID string) ([]domain.LinearTeamRules, error)

	// TracksTeamSystem reports whether the team has a row for linearTeamID in
	// workspaceID — the router's team gate. Admin pool. Mirrors
	// JiraStatusRulesStore.TracksProjectSystem.
	TracksTeamSystem(ctx context.Context, teamID, workspaceID, linearTeamID string) (bool, error)

	// ReplaceForTeam makes rules the team's whole set in workspaceID in one
	// transaction: the team's rows in that workspace whose linear_team_id is
	// absent from rules are deleted, the rest are upserted and stamped with
	// the workspace. Rows the team holds in any other workspace are left as
	// they are. An empty rules deletes every row of the team in the
	// workspace; an entry with an empty LinearTeamID is an error, so a
	// malformed input cannot read as "clear everything", and so is an empty
	// workspaceID, since a row saved under no workspace could never be read
	// back. App pool.
	//
	// It returns the team's stored set in ListForTeam's order, read inside the
	// same transaction. That is the bulk-write form of the returned-row rule:
	// the write reconciles a whole set rather than one row, so there is no
	// single RETURNING to hand back, and what it answers is the set as stored
	// rather than the input, which carries no TeamID.
	ReplaceForTeam(ctx context.Context, teamID, workspaceID string, rules []domain.LinearTeamRules) ([]domain.LinearTeamRules, error)
}
