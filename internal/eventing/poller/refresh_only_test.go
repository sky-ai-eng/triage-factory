package poller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The refresh-only cycle: a source configured with nothing armed has nothing
// to discover, but the entities TF holds on its site or workspace are still
// read by provider id, so one whose issue moved is renamed onto the key the
// provider answers with. In multi mode that read is the only thing that
// renames an entity a run touched outside every tracked set.

// spanDisposition is the disposition attribute of the last ended span named
// name, "" when it carries none.
func spanDisposition(t *testing.T, recorder *tracetest.SpanRecorder, name string) string {
	t.Helper()
	ended := recorder.Ended()
	for i := len(ended) - 1; i >= 0; i-- {
		if ended[i].Name() != name {
			continue
		}
		for _, a := range ended[i].Attributes() {
			if a.Key == "disposition" {
				return a.Value.AsString()
			}
		}
		return ""
	}
	t.Fatalf("span %q was never recorded", name)
	return ""
}

// failingJiraRulesStore fails the org union read.
type failingJiraRulesStore struct{ dbpkg.JiraStatusRulesStore }

func (failingJiraRulesStore) ListForOrgSystem(context.Context, string, string) ([]domain.JiraProjectStatusRules, error) {
	return nil, errors.New("rules unreadable")
}

// TestRunJiraCycleForOrg_NothingArmedRefreshesFollowedEntities: an issue whose
// project's key was renamed, held by an entity no armed project covers, is
// read by its id and its entity renamed onto the new key, both when the org
// has no rules and when its only rule watches the project without arming it.
// The cycle sends no discovery query, retires a previous site's row exactly
// once, and completes as a poll.
func TestRunJiraCycleForOrg_NothingArmedRefreshesFollowedEntities(t *testing.T) {
	for _, tc := range []struct {
		name    string
		watched bool
	}{
		{"no rules", false},
		{"only a watched project", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := recordSpans(t)
			ctx := context.Background()
			var (
				mu       sync.Mutex
				searches []string
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/project/100"):
					_, _ = w.Write([]byte(`{"id":"100","key":"CORE","name":"Core"}`))
				case strings.HasSuffix(r.URL.Path, "/search"):
					var body struct {
						JQL string `json:"jql"`
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					mu.Lock()
					searches = append(searches, body.JQL)
					mu.Unlock()
					if body.JQL != "id IN (10001)" {
						t.Errorf("unexpected search %q: only the followed entity is read", body.JQL)
						_, _ = w.Write([]byte(`{"issues":[],"total":0}`))
						return
					}
					_, _ = w.Write([]byte(`{"issues":[{"id":"10001","key":"CORE-1","fields":{` +
						`"summary":"Fix","status":{"id":"1","name":"To Do"},` +
						`"project":{"id":"100","key":"CORE","name":"Core"},` +
						`"updated":"2026-10-01T10:00:00.000+0000"}}],"total":1}`))
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
			followed, _, err := stores.Entities.FindOrCreateJiraSystem(ctx, org, site, "ENG-1", "10001", domain.JiraProjectRef{ID: "100", Key: "ENG"}, "Fix", "")
			if err != nil {
				t.Fatal(err)
			}
			stale, _, err := stores.Entities.FindOrCreateJiraSystem(ctx, org, "https://old.example.com", "ENG-2", "10002", domain.JiraProjectRef{ID: "100", Key: "ENG"}, "Old", "")
			if err != nil {
				t.Fatal(err)
			}
			if tc.watched {
				watched := domain.JiraProjectStatusRules{TeamID: team, ProjectKey: "ENG", JiraProjectID: followed.JiraProjectID}
				if err := stores.JiraStatusRules.ReplaceForTeam(ctx, team, site, []domain.JiraProjectStatusRules{watched}); err != nil {
					t.Fatalf("ReplaceForTeam: %v", err)
				}
			}

			published := &capturingPublisher{}
			m := &Manager{
				database: database, pub: published,
				tasks: stores.Tasks, entities: stores.Entities, repos: stores.Repos, eventQueue: stores.EventQueue, mergeability: stores.PRMergeability,
				orgs: stores.Orgs, users: stores.Users, secrets: jiraSecrets{},
				jiraRules: stores.JiraStatusRules, jiraProjects: stores.JiraProjects,
				EventSources: stores.OrgEventSources,
			}
			client := jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat"))
			m.runJiraCycleForOrg(ctx, stubJiraResolver{client: client}, org, time.Now())

			if got := spanOutcome(t, recorder, "poll.jira.org"); got != "" {
				t.Errorf("span outcome = %q, want none: the cycle polled", got)
			}
			if got := spanDisposition(t, recorder, "poll.jira.org"); got != "refresh_only" {
				t.Errorf("span disposition = %q, want refresh_only", got)
			}
			after, err := stores.Entities.GetByExternalIDSystem(ctx, org, "jira", site, "10001")
			if err != nil || after == nil {
				t.Fatalf("GetByExternalIDSystem = %+v, %v", after, err)
			}
			if after.ID != followed.ID || after.SourceID != "CORE-1" || after.State != "active" {
				t.Errorf("entity after the cycle = %s %q %s, want %s renamed to CORE-1 and still active", after.ID, after.SourceID, after.State, followed.ID)
			}
			if !strings.HasSuffix(after.URL, "/browse/CORE-1") {
				t.Errorf("entity url = %q, want the new key's link", after.URL)
			}
			mu.Lock()
			if len(searches) != 1 {
				t.Errorf("searches = %q, want only the read of the followed entity", searches)
			}
			mu.Unlock()
			if evts := published.ofType(domain.EventJiraIssueUnreachable); len(evts) != 1 || evts[0].EntityID == nil || *evts[0].EntityID != stale.ID {
				t.Errorf("unreachable events = %+v, want one, for the previous site's row", evts)
			}
			if evts := published.ofType(domain.EventSystemPollCompleted); len(evts) != 1 {
				t.Errorf("poll-complete events = %d, want 1", len(evts))
			}
		})
	}
}

// TestRunJiraCycleForOrg_UnreadableRulesRefreshNothing: a cycle that cannot
// read the rules does not know which projects are armed, so it refreshes
// nothing rather than read tracked issues as if none were. The resolver is
// nil, so reaching it would panic.
func TestRunJiraCycleForOrg_UnreadableRulesRefreshNothing(t *testing.T) {
	recorder := recordSpans(t)
	ctx := context.Background()
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	const site = "https://jira.example.com"
	if _, err := stores.Orgs.SetSourceBaseURL(ctx, org, "jira", site); err != nil {
		t.Fatalf("set jira site: %v", err)
	}
	if _, _, err := stores.Entities.FindOrCreateJiraSystem(ctx, org, site, "ENG-1", "10001", domain.JiraProjectRef{ID: "100", Key: "ENG"}, "Fix", ""); err != nil {
		t.Fatal(err)
	}
	published := &capturingPublisher{}
	m := &Manager{
		database: database, pub: published,
		tasks: stores.Tasks, entities: stores.Entities, repos: stores.Repos, eventQueue: stores.EventQueue, mergeability: stores.PRMergeability,
		orgs: stores.Orgs, users: stores.Users, secrets: jiraSecrets{},
		jiraRules: failingJiraRulesStore{}, jiraProjects: stores.JiraProjects,
		EventSources: stores.OrgEventSources,
	}
	m.runJiraCycleForOrg(ctx, nil, org, time.Now())
	if got := spanOutcome(t, recorder, "poll.jira.org"); got != "unconfigured" {
		t.Errorf("span outcome = %q, want unconfigured", got)
	}
	if evts := published.ofType(domain.EventSystemPollCompleted); len(evts) != 0 {
		t.Errorf("poll-complete events = %d, want none", len(evts))
	}
}

// linearIssueNode is an issue node as Linear's batch read answers it.
func linearIssueNode(id, identifier, teamID, teamKey string) map[string]any {
	return map[string]any{
		"id": id, "identifier": identifier, "title": "Fix",
		"url":       "https://linear.app/acme/issue/" + identifier,
		"createdAt": "2026-10-01T09:00:00.000Z", "updatedAt": "2026-10-01T10:00:00.000Z",
		"state":    map[string]any{"id": "st-todo", "name": "Todo", "type": "unstarted", "position": 1},
		"team":     map[string]any{"id": teamID, "key": teamKey, "name": "Core"},
		"labels":   map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}},
		"comments": map[string]any{"nodes": []any{}},
		"children": map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}},
	}
}

// TestRunLinearCycleForOrg_NothingArmedRefreshesFollowedEntities is the
// Linear twin of the Jira test: an issue whose team's key was renamed, held by
// an entity no armed team covers, is read by its UUID and renamed, whether the
// org has no rules or only an unarmed one. The cycle discovers nothing,
// retires a previous workspace's row exactly once, and completes as a poll.
func TestRunLinearCycleForOrg_NothingArmedRefreshesFollowedEntities(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rules []domain.LinearTeamRules
	}{
		{"no rules", nil},
		{"only an unarmed team", []domain.LinearTeamRules{{TeamID: "team-a", LinearTeamID: "team-eng", LinearTeamKey: "ENG"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := recordSpans(t)
			ctx := context.Background()
			org := runmode.LocalDefaultOrgID
			database := newMigratedSQLiteForPoller(t)
			bindLinearWorkspace(t, database)
			stores := sqlitestore.New(database)
			followed, _, err := stores.Entities.FindOrCreateLinearSystem(ctx, org, linearTestWorkspace, "ENG-1", "uuid-1", "team-eng", "Fix", "")
			if err != nil {
				t.Fatal(err)
			}
			stale, _, err := stores.Entities.FindOrCreateLinearSystem(ctx, org, "ws-old", "ENG-2", "uuid-2", "team-eng", "Old", "")
			if err != nil {
				t.Fatal(err)
			}
			stub := &linearStub{issues: []map[string]any{linearIssueNode("uuid-1", "CORE-1", "team-eng", "CORE")}}
			srv := stub.serve(t)
			published := &capturingPublisher{}
			m := &Manager{
				database: database, pub: published,
				tasks: stores.Tasks, entities: stores.Entities, repos: stores.Repos, eventQueue: stores.EventQueue, mergeability: stores.PRMergeability,
				orgs: stores.Orgs, users: stores.Users,
				secrets:        linearSecrets{key: "lin_api_test"},
				linearRules:    linearRulesStore{rules: tc.rules},
				linearResolver: stubLinearResolver{client: linear.NewClient(linear.ProxyPlaceholder(srv.URL, "placeholder"))},
				EventSources:   stores.OrgEventSources,
			}
			m.runLinearCycleForOrg(ctx, org, time.Now())

			if got := spanOutcome(t, recorder, "poll.linear.org"); got != "" {
				t.Errorf("span outcome = %q, want none: the cycle polled", got)
			}
			if got := spanDisposition(t, recorder, "poll.linear.org"); got != "refresh_only" {
				t.Errorf("span disposition = %q, want refresh_only", got)
			}
			after, err := stores.Entities.GetByExternalIDSystem(ctx, org, "linear", linearTestWorkspace, "uuid-1")
			if err != nil || after == nil {
				t.Fatalf("GetByExternalIDSystem = %+v, %v", after, err)
			}
			if after.ID != followed.ID || after.SourceID != "CORE-1" || after.State != "active" {
				t.Errorf("entity after the cycle = %s %q %s, want %s renamed to CORE-1 and still active", after.ID, after.SourceID, after.State, followed.ID)
			}
			if got := stub.requests.Load(); got != 1 {
				t.Errorf("linear requests = %d, want only the read of the followed entity", got)
			}
			if evts := published.ofType(domain.EventLinearIssueUnreachable); len(evts) != 1 || evts[0].EntityID == nil || *evts[0].EntityID != stale.ID {
				t.Errorf("unreachable events = %+v, want one, for the previous workspace's row", evts)
			}
			if evts := published.ofType(domain.EventSystemPollCompleted); len(evts) != 1 {
				t.Errorf("poll-complete events = %d, want 1", len(evts))
			}
		})
	}
}

// TestRunLinearCycleForOrg_UnreadableRulesRefreshNothing is Jira's: a cycle
// that cannot read the rules refreshes nothing. The resolver is nil, so
// reaching it would panic.
func TestRunLinearCycleForOrg_UnreadableRulesRefreshNothing(t *testing.T) {
	recorder := recordSpans(t)
	ctx := context.Background()
	database := newMigratedSQLiteForPoller(t)
	bindLinearWorkspace(t, database)
	stores := sqlitestore.New(database)
	if _, _, err := stores.Entities.FindOrCreateLinearSystem(ctx, runmode.LocalDefaultOrgID, linearTestWorkspace, "ENG-1", "uuid-1", "team-eng", "Fix", ""); err != nil {
		t.Fatal(err)
	}
	published := &capturingPublisher{}
	m := &Manager{
		database: database, pub: published,
		tasks: stores.Tasks, entities: stores.Entities, repos: stores.Repos, eventQueue: stores.EventQueue, mergeability: stores.PRMergeability,
		orgs: stores.Orgs, users: stores.Users, secrets: linearSecrets{key: "lin_api_test"},
		linearRules:  failingLinearRulesStore{},
		EventSources: stores.OrgEventSources,
	}
	m.runLinearCycleForOrg(ctx, runmode.LocalDefaultOrgID, time.Now())
	if got := spanOutcome(t, recorder, "poll.linear.org"); got != "unconfigured" {
		t.Errorf("span outcome = %q, want unconfigured", got)
	}
	if evts := published.ofType(domain.EventSystemPollCompleted); len(evts) != 0 {
		t.Errorf("poll-complete events = %d, want none", len(evts))
	}
}
