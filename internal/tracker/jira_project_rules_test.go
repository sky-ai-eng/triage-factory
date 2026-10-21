package tracker

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// registeredRules registers each rule's project on site, as the team settings
// write does before a rule can name it, and returns the rules carrying their
// registry rows. A rule with no ProjectID is registered under "id-<key>", the
// Jira project id dbseed.JiraIssueProject gives an issue keyed in the project,
// so a fixture issue created in it and the rule meet on one row.
func registeredRules(t *testing.T, projects db.JiraProjectStore, site string, rules JiraRules) JiraRules {
	t.Helper()
	out := make(JiraRules, len(rules))
	for i, r := range rules {
		externalID := r.ProjectID
		if externalID == "" {
			externalID = "id-" + r.Key
		}
		p, err := projects.FindOrCreateSystem(context.Background(), runmode.LocalDefaultOrgID, site, externalID, r.Key, "")
		if err != nil {
			t.Fatalf("register jira project %s: %v", r.Key, err)
		}
		r.JiraProjectID = p.ID
		out[i] = r
	}
	return out
}
