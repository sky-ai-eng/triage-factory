package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// init registers jiraProjectInFaults for every Jira event type whose predicate
// has a jira_project_id_in field. The events package registers its schemas in
// its own init, which runs before this one because this package imports it.
func init() {
	for eventType, schema := range events.All() {
		if !strings.HasPrefix(eventType, "jira:") {
			continue
		}
		for _, field := range schema.Fields {
			if field.Name == "jira_project_id_in" {
				RegisterPredicateReferenceCheck(eventType, jiraProjectInFaults)
				break
			}
		}
	}
}

// jiraProjectInFaults is the PredicateReferenceCheck for Jira event types:
// every jira_project_id_in entry the predicate adds, and every
// old_jira_project_id_in entry on key_changed, must be the registry id of a
// Jira project the handler's team tracks on the org's current Jira site. That
// is the set the predicate editor offers, and for every event but key_changed
// the only set that can match, because the router delivers a team's handlers
// only events from projects it tracks. A project key, the value a caller would
// naturally reach for, is refused here rather than stored as a filter that
// never fires.
//
// key_changed reaches a team that tracks either of its two projects, so one of
// its filters could name a project this team does not track and still match.
// It is held to the tracked set all the same, as Linear's identifier_changed
// is, so every id a Jira filter accepts is one the editor offers.
func jiraProjectInFaults(ctx context.Context, tx db.TxStores, orgID, teamID string, predicate, prior *string) ([]string, error) {
	return trackedIDFaults(predicate, prior, []string{"jira_project_id_in", "old_jira_project_id_in"},
		"is not the registry id of a Jira project this team tracks "+
			"(it takes the id of an entry in jira_projects on GET /api/teams/{team_id}/settings, not the project key)",
		func() (map[string]bool, error) {
			settings, err := tx.Orgs.GetSettings(ctx, orgID)
			if err != nil {
				return nil, fmt.Errorf("read org jira site: %w", err)
			}
			tracked := map[string]bool{}
			site := domain.EntityScope("jira", settings)
			if site == "" {
				return tracked, nil
			}
			rules, err := tx.JiraStatusRules.ListForTeam(ctx, teamID, site)
			if err != nil {
				return nil, fmt.Errorf("read team tracked jira projects: %w", err)
			}
			for _, r := range rules {
				tracked[r.JiraProjectID] = true
			}
			return tracked, nil
		})
}
