package tracker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
)

// deadStatusServer models the instance a globally-deleted status leaves behind:
// JQL naming it is rejected outright, so the whole project's query fails rather
// than the one term. The workflow read is what tells the difference between
// that and any other search failure.
type deadStatusServer struct {
	*httptest.Server
	mu        sync.Mutex
	deadID    string
	jqls      []string
	workflow  []jiraclient.Status
	statusHit int
}

func newDeadStatusServer(t *testing.T, deadID string, workflow ...jiraclient.Status) *deadStatusServer {
	t.Helper()
	d := &deadStatusServer{deadID: deadID, workflow: workflow}
	d.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/statuses"):
			d.mu.Lock()
			d.statusHit++
			d.mu.Unlock()
			// Grouped by issue type upstream; one group is enough here.
			body, _ := json.Marshal([]map[string]any{{"statuses": d.workflow}})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)

		// Cloud posts to /search/jql, Data Center to /search.
		case strings.Contains(r.URL.Path, "/search"):
			raw, _ := io.ReadAll(r.Body)
			var payload struct {
				JQL string `json:"jql"`
			}
			_ = json.Unmarshal(raw, &payload)
			d.mu.Lock()
			d.jqls = append(d.jqls, payload.JQL)
			d.mu.Unlock()
			if strings.Contains(payload.JQL, d.deadID) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"errorMessages":["The value '`+d.deadID+`' does not exist for the field 'status'."]}`)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"issues":[{"key":"SKY-1","fields":{
				"summary":"Salvaged","status":{"id":"10001","name":"To Do"}}}]}`)

		default:
			t.Errorf("unexpected jira request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(d.Close)
	return d
}

func (d *deadStatusServer) sentJQLs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.jqls...)
}

func salvageRules() JiraRules {
	return JiraRules{{
		Key: "SKY",
		// 99999 was deleted in Jira; 10001 is still in the workflow.
		PickupMembers: []domain.JiraStatusRef{{ID: "10001", Name: "To Do"}, {ID: "99999", Name: "Retired"}},
		DoneMembers:   []domain.JiraStatusRef{{ID: "10100", Name: "Done"}},
	}}
}

// One dead status must cost the team that status, not the whole project. The
// rebuilt query drops it and the surviving statuses keep discovering work.
func TestDiscoverJira_SalvagesAQueryNamingADeadStatus(t *testing.T) {
	srv := newDeadStatusServer(t, "99999",
		jiraclient.Status{ID: "10001", Name: "To Do"},
		jiraclient.Status{ID: "10100", Name: "Done"})
	client := jiraclient.NewClient(jiraclient.CloudAPIToken(srv.URL, "me@example.com", "tok"))

	got, err := (&Tracker{}).discoverJira(context.Background(), client, srv.URL, salvageRules())
	if err != nil {
		t.Fatalf("discoverJira: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no issues discovered; one dead status disabled the whole project")
	}

	jqls := srv.sentJQLs()
	var retried string
	for _, j := range jqls {
		if strings.Contains(j, "assignee IS EMPTY") && !strings.Contains(j, "99999") {
			retried = j
		}
	}
	if retried == "" {
		t.Fatalf("no rebuilt pickup query was sent; got %q", jqls)
	}
	if !strings.Contains(retried, "10001") {
		t.Errorf("the rebuilt query %q dropped a status that is still live", retried)
	}
}

// The workflow read is the rare path, not a per-cycle cost: a healthy project
// never reaches it.
func TestDiscoverJira_HealthyProjectNeverReadsTheWorkflow(t *testing.T) {
	srv := newDeadStatusServer(t, "no-such-id",
		jiraclient.Status{ID: "10001", Name: "To Do"})
	client := jiraclient.NewClient(jiraclient.CloudAPIToken(srv.URL, "me@example.com", "tok"))

	if _, err := (&Tracker{}).discoverJira(context.Background(), client, srv.URL, salvageRules()); err != nil {
		t.Fatalf("discoverJira: %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.statusHit != 0 {
		t.Errorf("read the project workflow %d times with nothing failing", srv.statusHit)
	}
}

// A query whose members are all still live failed for some other reason.
// Rerunning it unchanged would just repeat the failure, so nothing is retried.
func TestDiscoverJira_NoRetryWhenEveryStatusIsLive(t *testing.T) {
	srv := newDeadStatusServer(t, "10001", // the live status itself is rejected
		jiraclient.Status{ID: "10001", Name: "To Do"},
		jiraclient.Status{ID: "99999", Name: "Retired"},
		jiraclient.Status{ID: "10100", Name: "Done"})
	client := jiraclient.NewClient(jiraclient.CloudAPIToken(srv.URL, "me@example.com", "tok"))

	if _, err := (&Tracker{}).discoverJira(context.Background(), client, srv.URL, salvageRules()); err != nil {
		t.Fatalf("discoverJira: %v", err)
	}
	pickups := 0
	for _, j := range srv.sentJQLs() {
		if strings.Contains(j, "assignee IS EMPTY") {
			pickups++
		}
	}
	if pickups != 1 {
		t.Errorf("pickup query ran %d times; a failure with no dead status must not be retried", pickups)
	}
}

// The assigned-to-me query EXCLUDES its status set, so narrowing it widens the
// result. That is unsound in a way the pickup query is not, and it is not only
// the all-deleted case: the filter drops every status missing from the
// project's workflow, while JQL only rejects one missing from the instance —
// so a status a workflow-scheme change retired, still holding finished
// tickets, would be dropped from the exclusion and hand those tickets back as
// new work. The query is left to fail instead.
func TestDiscoverJira_NeverSalvagesTheAssignedQuery(t *testing.T) {
	// Every Done member is rejected, and none of them is in the workflow the
	// salvage would filter against — the shape that would produce a query with
	// no terminal exclusion at all.
	srv := newDeadStatusServer(t, "10100",
		jiraclient.Status{ID: "10001", Name: "To Do"})
	client := jiraclient.NewClient(jiraclient.CloudAPIToken(srv.URL, "me@example.com", "tok"))

	if _, err := (&Tracker{}).discoverJira(context.Background(), client, srv.URL, salvageRules()); err != nil {
		t.Fatalf("discoverJira: %v", err)
	}
	for _, jql := range srv.sentJQLs() {
		if !strings.Contains(jql, "currentUser()") {
			continue
		}
		if !strings.Contains(jql, "status NOT IN") {
			t.Errorf("assigned query %q ran with no terminal exclusion; every completed ticket comes back as new work", jql)
		}
	}
}

// An inclusion set narrowed to nothing is an unfiltered query, not a narrower
// one. Every pickup member being gone must end the query, never widen it.
func TestDiscoverJira_NoSalvageWhenNothingSurvives(t *testing.T) {
	// The workflow shares no status with the pickup rule, so both members drop.
	srv := newDeadStatusServer(t, "99999",
		jiraclient.Status{ID: "20001", Name: "Fresh Start"},
		jiraclient.Status{ID: "10100", Name: "Done"})
	client := jiraclient.NewClient(jiraclient.CloudAPIToken(srv.URL, "me@example.com", "tok"))

	if _, err := (&Tracker{}).discoverJira(context.Background(), client, srv.URL, salvageRules()); err != nil {
		t.Fatalf("discoverJira: %v", err)
	}
	for _, jql := range srv.sentJQLs() {
		if strings.Contains(jql, "assignee IS EMPTY") && !strings.Contains(jql, "status IN") {
			t.Errorf("pickup query %q ran with no status filter; every unassigned ticket in the project becomes work", jql)
		}
	}
}
