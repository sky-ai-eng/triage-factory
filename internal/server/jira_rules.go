package server

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// lookupJiraRuleForTask resolves the per-team Jira status rule that
// governs the given task's source project on site (the org's Jira site): the
// rule for the registry row the task's entity references
// (entities.jira_project_id), which a project key rename does not move.
// Reads task.TeamID directly — not the org's default team — so multi-team
// orgs route each task to the right rule set. Returns nil when the task is
// not Jira-backed, has no TeamID stamped, or the team has no rule
// configured for the ticket's project.
//
// Runs inside the caller's WithTx so the rule lookup goes through
// the app-pool ListForTeam (jira_rules_select RLS gates by team
// membership) — keeps request-handler reads RLS-consistent.
func lookupJiraRuleForTask(ctx context.Context, tx db.TxStores, orgID, site string, task *domain.Task) *domain.JiraProjectStatusRules {
	if task == nil || task.EntitySource != "jira" || task.TeamID == nil || *task.TeamID == "" {
		return nil
	}
	entity, err := tx.Entities.Get(ctx, orgID, task.EntityID)
	if err != nil {
		jiraRuleLog.Warn("read entity for rule lookup failed", "entity", task.EntityID, "error", err)
		return nil
	}
	if entity == nil || entity.JiraProjectID == "" {
		return nil
	}
	rules, err := tx.JiraStatusRules.ListForTeam(ctx, *task.TeamID, site)
	if err != nil {
		jiraRuleLog.Warn("list rules for team failed", "team", *task.TeamID, "error", err)
		return nil
	}
	return domain.RuleForJiraProject(rules, entity.JiraProjectID)
}
