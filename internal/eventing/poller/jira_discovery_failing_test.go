package poller

import (
	"context"
	"errors"
	"testing"
	"time"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
	"github.com/sky-ai-eng/triage-factory/internal/notify/notifytest"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// unreadableJiraRules fails the org union read the cycle polls from.
type unreadableJiraRules struct{ dbpkg.JiraStatusRulesStore }

func (unreadableJiraRules) ListForOrgSystem(context.Context, string, string) ([]domain.JiraProjectStatusRules, error) {
	return nil, errors.New("rules unreadable")
}

// TestRunJiraCycleForOrg_NothingArmedResolvesDiscoveryFailing: an org whose
// rules arm no project has no discovery to fail, so an occurrence left open
// from when one was armed resolves — but only from rules the cycle read, since
// a failed read says nothing about them. The resolver is nil, so a cycle that
// reached Jira would panic.
func TestRunJiraCycleForOrg_NothingArmedResolvesDiscoveryFailing(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	for _, c := range []struct {
		name     string
		rules    func(dbpkg.JiraStatusRulesStore) dbpkg.JiraStatusRulesStore
		resolved bool
	}{
		{"rules read", func(s dbpkg.JiraStatusRulesStore) dbpkg.JiraStatusRulesStore { return s }, true},
		{"rules unreadable", func(s dbpkg.JiraStatusRulesStore) dbpkg.JiraStatusRulesStore { return unreadableJiraRules{s} }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := notifytest.Record(t)
			database := newMigratedSQLiteForPoller(t)
			stores := sqlitestore.New(database)
			if _, err := stores.Orgs.SetSourceBaseURL(context.Background(), org, "jira", "https://jira.example.com"); err != nil {
				t.Fatal(err)
			}
			kinds.JiraDiscoveryFailing.Set(context.Background(), org, kinds.JiraProject{JiraProjectID: "jp-sky", ProjectKey: "SKY"}, true)
			m := &Manager{
				database: database, pub: &capturingPublisher{},
				tasks: stores.Tasks, entities: stores.Entities, repos: stores.Repos, eventQueue: stores.EventQueue, mergeability: stores.PRMergeability,
				orgs: stores.Orgs, users: stores.Users, secrets: jiraSecrets{},
				jiraRules:    c.rules(stores.JiraStatusRules),
				jiraProjects: stores.JiraProjects,
				EventSources: stores.OrgEventSources,
			}
			m.runJiraCycleForOrg(context.Background(), nil, org, time.Now())
			if c.resolved {
				rec.Inactive(t, org, kinds.JiraDiscoveryFailing)
			} else {
				rec.Active(t, org, kinds.JiraDiscoveryFailing, "jp-sky")
			}
		})
	}
}
