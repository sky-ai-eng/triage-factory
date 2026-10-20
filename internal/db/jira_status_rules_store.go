package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// JiraStatusRulesStore owns the jira_project_status_rules table —
// one row per (team_id, jira_project_id) carrying the team's per-project
// pickup/in_progress/in_review/done status configuration. Separate from
// TeamsStore because the table is multi-row per team with bulk-replace
// semantics (config.Save's clone-then-prune flow); folding it into
// the single-row TeamSettings struct would muddy the per-scope read
// shape.
//
// A row references its project by the jira_projects registry row's id, so a
// project key rename changes nothing here. Every read joins the registry and
// hands back the project's key as the registry holds it now
// (domain.JiraProjectStatusRules.ProjectKey), which is how a reader that looks
// a rule up by key still finds it after a rename.
//
// # Site
//
// A registry row is one project on one Jira site, and no other site has its
// project or status ids. Every method therefore takes the site — the org's
// current one, domain.EntityScope("jira", settings) — and sees only rules
// whose project is on it: after the org moves to another Jira site, the old
// rules stop applying without being deleted, and moving back brings them
// back. An empty site (no Jira site configured) reads as no rows. This is the
// tracked-set rule for a provider namespace change, as for Linear workspaces:
// forward-only, and nothing a save did not name is destroyed.
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
//
// The in_review rule is optional and, unlike the other three, feeds nothing
// that decides what TF polls or how a ticket is classified — see
// domain.JiraProjectStatusRules. A row whose in_review columns are empty is a
// complete configuration, which is why every read here can hand one back.
type JiraStatusRulesStore interface {
	// ListForTeam returns the team's per-project rules on site in
	// project key order. Empty slice with nil error when the team has
	// no rules configured there. Postgres routes through the app pool.
	ListForTeam(ctx context.Context, teamID, site string) ([]domain.JiraProjectStatusRules, error)

	// ListForTeamSystem mirrors ListForTeam but routes through the
	// admin pool in Postgres for callers without a JWT-claims context.
	ListForTeamSystem(ctx context.Context, teamID, site string) ([]domain.JiraProjectStatusRules, error)

	// ListForOrgSystem returns every team's per-project rules on site across
	// the org — the UNION the poller discovers, so a non-default team's Jira
	// config is polled too. One row per (team_id, jira_project_id); the
	// caller (toTrackerJiraRules) merges members per project. Ordered by
	// (project key, jira_project_id, team_id) for a deterministic merge.
	// Admin pool in Postgres: the union spans teams.
	ListForOrgSystem(ctx context.Context, orgID, site string) ([]domain.JiraProjectStatusRules, error)

	// TracksProjectSystem reports whether the team has a rule for the project
	// whose key on site is projectKey, matched against the registry's current
	// key. This is the router team↔project gate lookup. Admin pool in
	// Postgres: the router goroutine has no JWT claims. Mirrors
	// TeamGitHubReposStore.TracksRepoSystem.
	TracksProjectSystem(ctx context.Context, teamID, site, projectKey string) (bool, error)

	// ReplaceForTeam makes rules the team's whole set on site in one
	// transaction: each entry is upserted under its JiraProjectID, and the
	// team's rules for projects on site that the input no longer names are
	// deleted. Rules the team holds for projects on another site are left as
	// they are. Passing an empty rules slice deletes every rule of the team on
	// site. Postgres routes through the app pool (jira_rules_insert / _update
	// / _delete RLS gate writes by team admin).
	//
	// An entry with an empty JiraProjectID, two entries naming one project, an
	// entry whose registry row is not on site in the team's org, and an empty
	// site are all errors, so a malformed input cannot read as "clear
	// everything" and a rule can never point at a project no read would show.
	//
	// Exempt from the returned-row rule: it reconciles a team's whole rule set
	// in one transaction, so there is no single row a return value could name.
	// What it wrote is read back through the store's list read.
	ReplaceForTeam(ctx context.Context, teamID, site string, rules []domain.JiraProjectStatusRules) error
}

// ValidateJiraStatusRulesInput is ReplaceForTeam's input check, shared by both
// dialects.
func ValidateJiraStatusRulesInput(site string, rules []domain.JiraProjectStatusRules) error {
	if site == "" {
		return errors.New("ReplaceForTeam: no Jira site to save rules under")
	}
	seen := make(map[string]bool, len(rules))
	for i, r := range rules {
		if r.JiraProjectID == "" {
			return fmt.Errorf("ReplaceForTeam: rules[%d] has empty JiraProjectID", i)
		}
		if seen[r.JiraProjectID] {
			return fmt.Errorf("ReplaceForTeam: rules[%d] names jira project %s twice", i, r.JiraProjectID)
		}
		seen[r.JiraProjectID] = true
	}
	return nil
}

// ErrJiraRuleProjectNotOnSite is returned by ReplaceForTeam when a rule names a
// registry row that is not on the site being saved, or not in the team's org.
var ErrJiraRuleProjectNotOnSite = errors.New("jira project is not a registry row on this site")
