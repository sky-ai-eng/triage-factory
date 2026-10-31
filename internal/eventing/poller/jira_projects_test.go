package poller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// stubJiraResolver hands the cycle one prebuilt service client.
type stubJiraResolver struct{ client *jiraclient.Client }

func (r stubJiraResolver) ForSystem(context.Context, string) (*jiraclient.Client, error) {
	return r.client, nil
}

func (r stubJiraResolver) ForUser(context.Context, string, string) (*jiraclient.Client, error) {
	return r.client, nil
}

// TestRunJiraCycleForOrg_RefreshesARenamedProjectBeforeDiscovery: a project
// whose key was renamed in Jira since the team tracked it. The cycle reads the
// project by its Jira id before discovery, moves the registry row onto the new
// key, and asks every discovery query by the id — so the team's rule, the
// router gate and the tracker's per-key lookups all answer under the key the
// project's issues now carry, and nothing ever queried the old key.
func TestRunJiraCycleForOrg_RefreshesARenamedProjectBeforeDiscovery(t *testing.T) {
	ctx := context.Background()
	var (
		mu   sync.Mutex
		jqls []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/project/10000"):
			_, _ = w.Write([]byte(`{"id":"10000","key":"NEW","name":"Renamed"}`))
		case strings.HasSuffix(r.URL.Path, "/search"):
			var body struct {
				JQL string `json:"jql"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			jqls = append(jqls, body.JQL)
			mu.Unlock()
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
	project, err := stores.JiraProjects.FindOrCreate(ctx, org, site, "10000", "OLD", "Old")
	if err != nil {
		t.Fatalf("FindOrCreate: %v", err)
	}
	rule := armedRule(team, "OLD", []string{"To Do"}, []string{"Done"})
	rule.JiraProjectID = project.ID
	if err := stores.JiraStatusRules.ReplaceForTeam(ctx, team, site, []domain.JiraProjectStatusRules{rule}); err != nil {
		t.Fatalf("ReplaceForTeam: %v", err)
	}

	m := &Manager{
		database: database, pub: &capturingPublisher{},
		tasks: stores.Tasks, entities: stores.Entities, repos: stores.Repos, eventQueue: stores.EventQueue,
		orgs: stores.Orgs, users: stores.Users, secrets: jiraSecrets{},
		jiraRules: stores.JiraStatusRules, jiraProjects: stores.JiraProjects,
		EventSources: stores.OrgEventSources,
	}
	client := jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat"))
	m.runJiraCycleForOrg(ctx, stubJiraResolver{client: client}, org, time.Now())

	got, err := stores.JiraProjects.GetSystem(ctx, org, project.ID)
	if err != nil || got == nil {
		t.Fatalf("GetSystem = %+v, %v", got, err)
	}
	if got.Key != "NEW" || got.Name != "Renamed" || got.ExternalID != "10000" {
		t.Errorf("registry row after the cycle = %+v, want it renamed to NEW", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(jqls) == 0 {
		t.Fatal("the cycle sent no discovery query")
	}
	for _, jql := range jqls {
		if !strings.HasPrefix(jql, "project = 10000 ") || strings.Contains(jql, "OLD") {
			t.Errorf("discovery JQL = %q, want it to name the project by id", jql)
		}
	}

	rules, err := stores.JiraStatusRules.ListForTeamSystem(ctx, team, site)
	if err != nil {
		t.Fatalf("ListForTeamSystem: %v", err)
	}
	if len(rules) != 1 || rules[0].JiraProjectID != project.ID || rules[0].ProjectKey != "NEW" {
		t.Errorf("team rules after the cycle = %+v, want the same rule under NEW", rules)
	}
	if tracks, err := stores.JiraStatusRules.TracksJiraProjectSystem(ctx, team, site, project.ID); err != nil || !tracks {
		t.Errorf("TracksJiraProjectSystem = %v, %v; want the team to track the renamed project", tracks, err)
	}
}

// TestRunJiraCycleForOrg_ResolvesAnUnresolvedProjectByKey: a registry row the
// migration could not find a Jira id for is read by its key, and the answer
// resolves it before the cycle's discovery asks by the id.
func TestRunJiraCycleForOrg_ResolvesAnUnresolvedProjectByKey(t *testing.T) {
	ctx := context.Background()
	var (
		mu   sync.Mutex
		jqls []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/project/OPS"):
			_, _ = w.Write([]byte(`{"id":"20000","key":"OPS","name":"Operations"}`))
		case strings.HasSuffix(r.URL.Path, "/search"):
			var body struct {
				JQL string `json:"jql"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			jqls = append(jqls, body.JQL)
			mu.Unlock()
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
	const id = "00000000-0000-4000-8000-0000000000c1"
	if _, err := database.Exec(
		`INSERT INTO jira_projects (id, org_id, site, project_key) VALUES (?, ?, ?, 'OPS')`, id, org, site,
	); err != nil {
		t.Fatalf("seed unresolved row: %v", err)
	}
	rule := armedRule(team, "OPS", []string{"To Do"}, []string{"Done"})
	rule.JiraProjectID = id
	if err := stores.JiraStatusRules.ReplaceForTeam(ctx, team, site, []domain.JiraProjectStatusRules{rule}); err != nil {
		t.Fatalf("ReplaceForTeam: %v", err)
	}

	m := &Manager{
		database: database, pub: &capturingPublisher{},
		tasks: stores.Tasks, entities: stores.Entities, repos: stores.Repos, eventQueue: stores.EventQueue,
		orgs: stores.Orgs, users: stores.Users, secrets: jiraSecrets{},
		jiraRules: stores.JiraStatusRules, jiraProjects: stores.JiraProjects,
		EventSources: stores.OrgEventSources,
	}
	client := jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat"))
	m.runJiraCycleForOrg(ctx, stubJiraResolver{client: client}, org, time.Now())

	got, err := stores.JiraProjects.GetSystem(ctx, org, id)
	if err != nil || got == nil {
		t.Fatalf("GetSystem = %+v, %v", got, err)
	}
	if got.ExternalID != "20000" || got.Name != "Operations" {
		t.Errorf("registry row after the cycle = %+v, want it resolved to 20000", got)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, jql := range jqls {
		if !strings.HasPrefix(jql, "project = 20000 ") {
			t.Errorf("discovery JQL = %q, want it to name the resolved project by id", jql)
		}
	}
}

// TestRunJiraCycleForOrg_RefreshesAWatchedProject: a project a team watches
// without mapping any statuses has nothing for discovery to ask, but its key is
// still what the team's settings show. The cycle reads it by id and renames its
// registry row, and sends no discovery query.
func TestRunJiraCycleForOrg_RefreshesAWatchedProject(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/project/10000") {
			_, _ = w.Write([]byte(`{"id":"10000","key":"NEW","name":"Renamed"}`))
			return
		}
		t.Errorf("unexpected jira request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
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
	project, err := stores.JiraProjects.FindOrCreate(ctx, org, site, "10000", "OLD", "Old")
	if err != nil {
		t.Fatalf("FindOrCreate: %v", err)
	}
	watched := domain.JiraProjectStatusRules{TeamID: team, ProjectKey: "OLD", JiraProjectID: project.ID}
	if err := stores.JiraStatusRules.ReplaceForTeam(ctx, team, site, []domain.JiraProjectStatusRules{watched}); err != nil {
		t.Fatalf("ReplaceForTeam: %v", err)
	}

	m := &Manager{
		database: database, pub: &capturingPublisher{},
		tasks: stores.Tasks, entities: stores.Entities, repos: stores.Repos, eventQueue: stores.EventQueue,
		orgs: stores.Orgs, users: stores.Users, secrets: jiraSecrets{},
		jiraRules: stores.JiraStatusRules, jiraProjects: stores.JiraProjects,
		EventSources: stores.OrgEventSources,
	}
	client := jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat"))
	m.runJiraCycleForOrg(ctx, stubJiraResolver{client: client}, org, time.Now())

	got, err := stores.JiraProjects.GetSystem(ctx, org, project.ID)
	if err != nil || got == nil {
		t.Fatalf("GetSystem = %+v, %v", got, err)
	}
	if got.Key != "NEW" || got.Name != "Renamed" {
		t.Errorf("registry row after the cycle = %+v, want the watched project renamed to NEW", got)
	}
}
