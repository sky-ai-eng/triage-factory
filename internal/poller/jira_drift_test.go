package poller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
	"github.com/sky-ai-eng/triage-factory/internal/notify/notifytest"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestRunJiraCycleForOrg_ChecksDriftHourly: a cycle checks the org's status
// rules and Jira task rules against Jira and raises what names a value Jira no
// longer has, and the cycles after it within the hour ask Jira nothing more
// for it.
func TestRunJiraCycleForOrg_ChecksDriftHourly(t *testing.T) {
	rec := notifytest.Record(t)
	ctx := context.Background()
	var statusReads, priorityReads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/project/10000/statuses"):
			statusReads.Add(1)
			// "Ready" (id 2) has left the workflow.
			_, _ = w.Write([]byte(`[{"statuses":[{"id":"1","name":"To Do"},{"id":"3","name":"In Progress"},{"id":"5","name":"Done"}]}]`))
		case strings.HasSuffix(r.URL.Path, "/project/10000"):
			_, _ = w.Write([]byte(`{"id":"10000","key":"SKY","name":"Sky"}`))
		case strings.HasSuffix(r.URL.Path, "/priority"):
			priorityReads.Add(1)
			_, _ = w.Write([]byte(`[{"id":"1","name":"Highest"}]`))
		case strings.HasSuffix(r.URL.Path, "/search"):
			_, _ = w.Write([]byte(`{"issues":[],"total":0}`))
		default:
			t.Errorf("unexpected jira request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	org, team := runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID
	if _, err := stores.Orgs.SetSourceBaseURL(ctx, org, "jira", srv.URL); err != nil {
		t.Fatalf("set jira site: %v", err)
	}
	orgSet, err := stores.Orgs.GetSettingsSystem(ctx, org)
	if err != nil {
		t.Fatalf("GetSettingsSystem: %v", err)
	}
	site := domain.EntityScope("jira", orgSet)
	project, err := stores.JiraProjects.FindOrCreate(ctx, org, site, "10000", "SKY", "Sky")
	if err != nil {
		t.Fatalf("FindOrCreate: %v", err)
	}
	ref := func(id, name string) domain.JiraStatusRef { return domain.JiraStatusRef{ID: id, Name: name} }
	rule := domain.JiraProjectStatusRules{
		JiraProjectID:     project.ID,
		PickupMembers:     []domain.JiraStatusRef{ref("1", "To Do"), ref("2", "Ready")},
		InProgressMembers: []domain.JiraStatusRef{ref("3", "In Progress")}, InProgressCanonical: ref("3", "In Progress"),
		DoneMembers: []domain.JiraStatusRef{ref("5", "Done")}, DoneCanonical: ref("5", "Done"),
	}
	if err := stores.JiraStatusRules.ReplaceForTeam(ctx, team, site, []domain.JiraProjectStatusRules{rule}); err != nil {
		t.Fatalf("ReplaceForTeam: %v", err)
	}
	predicate := `{"priority_id_in":["9"]}`
	priority, order := 0.5, 0
	handler, err := stores.EventHandlers.Create(ctx, org, team, domain.EventHandler{
		ID: "00000000-0000-0000-0000-0000000000aa", Kind: domain.EventHandlerKindRule,
		EventType: domain.EventJiraIssueAssigned, Name: "Urgent work", Enabled: true,
		ScopePredicateJSON: &predicate, DefaultPriority: &priority, SortOrder: &order,
	})
	if err != nil {
		t.Fatalf("create handler: %v", err)
	}

	m := &Manager{
		database: database, pub: &capturingPublisher{},
		tasks: stores.Tasks, entities: stores.Entities, repos: stores.Repos, eventQueue: stores.EventQueue,
		orgs: stores.Orgs, users: stores.Users, secrets: jiraSecrets{},
		jiraRules: stores.JiraStatusRules, jiraProjects: stores.JiraProjects,
		EventSources: stores.OrgEventSources, EventHandlers: stores.EventHandlers,
	}
	resolver := stubJiraResolver{client: jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat"))}
	start := time.Now()
	m.runJiraCycleForOrg(ctx, resolver, org, start)

	rec.Active(t, org, kinds.JiraStatusDrift, team+":"+project.ID)
	if open := rec.Open(org, kinds.JiraStatusDrift); len(open) == 1 {
		got := open[0].Params.(kinds.JiraStatusRule)
		if got.ProjectKey != "SKY" || len(got.Statuses) != 1 || got.Statuses[0] != "Ready" || got.Stopped {
			t.Errorf("status drift params = %+v, want Ready gone and the rules still working", got)
		}
	}
	rec.Active(t, org, kinds.JiraPredicateDrift, handler.ID)

	m.runJiraCycleForOrg(ctx, resolver, org, start.Add(5*time.Minute))
	if n, p := statusReads.Load(), priorityReads.Load(); n != 1 || p != 1 {
		t.Errorf("after a second cycle within the hour: %d workflow and %d priority reads, want 1 each", n, p)
	}

	m.runJiraCycleForOrg(ctx, resolver, org, start.Add(jiraDriftInterval+time.Minute))
	if n := statusReads.Load(); n != 2 {
		t.Errorf("after the hour: %d workflow reads, want the check to run again", n)
	}
}

// TestRunJiraCycleForOrg_NothingTrackedResolvesDrift: once no team tracks a
// project on the org's site, nothing can have drifted, and what was open
// resolves without asking Jira. The resolver is nil, so a cycle that reached
// Jira would panic.
func TestRunJiraCycleForOrg_NothingTrackedResolvesDrift(t *testing.T) {
	rec := notifytest.Record(t)
	ctx, org := context.Background(), runmode.LocalDefaultOrgID
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	if _, err := stores.Orgs.SetSourceBaseURL(ctx, org, "jira", "https://jira.example.com"); err != nil {
		t.Fatal(err)
	}
	kinds.JiraStatusDrift.Set(ctx, org, kinds.JiraStatusRule{TeamID: "team", JiraProjectID: "jp-sky"}, true)
	kinds.JiraPredicateDrift.Set(ctx, org, kinds.JiraHandlerFilter{HandlerID: "handler"}, true)
	m := &Manager{
		database: database, pub: &capturingPublisher{},
		tasks: stores.Tasks, entities: stores.Entities, repos: stores.Repos, eventQueue: stores.EventQueue,
		orgs: stores.Orgs, users: stores.Users, secrets: jiraSecrets{},
		jiraRules: stores.JiraStatusRules, jiraProjects: stores.JiraProjects,
		EventSources: stores.OrgEventSources, EventHandlers: stores.EventHandlers,
	}
	m.runJiraCycleForOrg(ctx, nil, org, time.Now())
	rec.Inactive(t, org, kinds.JiraStatusDrift)
	rec.Inactive(t, org, kinds.JiraPredicateDrift)
}
