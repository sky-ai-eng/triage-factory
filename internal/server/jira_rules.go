package server

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// jiraTaskTarget is the ticket behind a Jira-backed task, as a Jira write on
// it needs it.
type jiraTaskTarget struct {
	// IssueRef is what the REST calls name the issue by: its id
	// (domain.Entity.JiraIssueRef), or the task's key when the entity could
	// not be read, which Jira still resolves.
	IssueRef string
	// Rule is the per-team Jira status rule that governs the ticket's project
	// on the org's site; nil when the task has no TeamID stamped or the team
	// has no rule configured for the project.
	Rule *domain.JiraProjectStatusRules
}

// lookupJiraTaskTarget resolves the ticket behind a Jira-backed task on site
// (the org's Jira site): the ref a Jira call addresses its issue by, and the
// rule for the registry row the task's entity references
// (entities.jira_project_id), which a project key rename does not move. Reads
// task.TeamID directly — not the org's default team — so multi-team orgs route
// each task to the right rule set. Returns the zero target when the task is
// not Jira-backed.
//
// Runs inside the caller's WithTx so the rule lookup goes through
// the app-pool ListForTeam (jira_rules_select RLS gates by team
// membership) — keeps request-handler reads RLS-consistent.
func lookupJiraTaskTarget(ctx context.Context, tx db.TxStores, orgID, site string, task *domain.Task) jiraTaskTarget {
	if task == nil || task.EntitySource != "jira" {
		return jiraTaskTarget{}
	}
	target := jiraTaskTarget{IssueRef: task.EntitySourceID}
	entity, err := tx.Entities.Get(ctx, orgID, task.EntityID)
	if err != nil {
		jiraRuleLog.Warn("read entity for rule lookup failed", "entity", task.EntityID, "error", err)
		return target
	}
	if entity == nil {
		return target
	}
	target.IssueRef = entity.JiraIssueRef()
	if task.TeamID == nil || *task.TeamID == "" || entity.JiraProjectID == "" {
		return target
	}
	rules, err := tx.JiraStatusRules.ListForTeam(ctx, *task.TeamID, site)
	if err != nil {
		jiraRuleLog.Warn("list rules for team failed", "team", *task.TeamID, "error", err)
		return target
	}
	target.Rule = domain.RuleForJiraProject(rules, entity.JiraProjectID)
	return target
}
