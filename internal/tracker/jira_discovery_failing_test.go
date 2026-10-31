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
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
	"github.com/sky-ai-eng/triage-factory/internal/notify/notifytest"
)

// projectAnswer is how discoveryServer answers one project's searches.
type projectAnswer int

const (
	answerOK projectAnswer = iota
	answerRejected
	answerUnavailable
)

// discoveryServer answers each project's discovery searches by the project id
// the JQL names, and serves a workflow holding every status the rules below
// name, so a rejection is never salvaged.
type discoveryServer struct {
	*httptest.Server
	mu      sync.Mutex
	answers map[string]projectAnswer
}

func newDiscoveryServer(t *testing.T) *discoveryServer {
	t.Helper()
	jiraclient.SetRetryBackoffForTest(t, time.Millisecond)
	d := &discoveryServer{answers: map[string]projectAnswer{}}
	d.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/statuses") {
			body, _ := json.Marshal([]map[string]any{{"statuses": []jiraclient.Status{
				{ID: "10001", Name: "To Do"}, {ID: "10100", Name: "Done"},
			}}})
			_, _ = w.Write(body)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var payload struct {
			JQL string `json:"jql"`
		}
		_ = json.Unmarshal(raw, &payload)
		d.mu.Lock()
		defer d.mu.Unlock()
		answer := answerOK
		for id, a := range d.answers {
			if strings.HasPrefix(payload.JQL, "project = "+id+" ") {
				answer = a
			}
		}
		switch answer {
		case answerRejected:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"errorMessages":["The value '10001' does not exist for the field 'status'."]}`)
		case answerUnavailable:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"errorMessages":["Service Unavailable"]}`)
		default:
			_, _ = io.WriteString(w, `{"issues":[]}`)
		}
	}))
	t.Cleanup(d.Close)
	return d
}

func (d *discoveryServer) answer(projectID string, a projectAnswer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.answers[projectID] = a
}

// discoveryRules is two armed projects, SKY (Jira id 1001) and OPS (1002).
func discoveryRules() JiraRules {
	rule := func(row, key, id string) JiraProjectRules {
		return JiraProjectRules{
			JiraProjectID: row, Key: key, ProjectID: id,
			PickupMembers: []domain.JiraStatusRef{{ID: "10001", Name: "To Do"}},
			DoneMembers:   []domain.JiraStatusRef{{ID: "10100", Name: "Done"}},
		}
	}
	return JiraRules{rule("jp-sky", "SKY", "1001"), rule("jp-ops", "OPS", "1002")}
}

func discover(t *testing.T, tr *Tracker, srv *discoveryServer) {
	t.Helper()
	client := jiraclient.NewClient(jiraclient.CloudAPIToken(srv.URL, "me@example.com", "tok"))
	if _, err := tr.discoverJira(context.Background(), client, srv.URL, discoveryRules()); err != nil {
		t.Fatalf("discoverJira: %v", err)
	}
}

// A project whose query Jira keeps rejecting is raised once the debounce is
// reached, for that project alone, and resolves on the first cycle Jira
// answers it.
func TestDiscoverJira_RaisesARejectedProjectAfterTheDebounce(t *testing.T) {
	rec := notifytest.Record(t)
	const org = "org-discovery-raise"
	srv := newDiscoveryServer(t)
	srv.answer("1001", answerRejected)
	tr := &Tracker{orgID: org}

	for range jiraDiscoveryDebounce - 1 {
		discover(t, tr, srv)
	}
	rec.Inactive(t, org, kinds.JiraDiscoveryFailing)

	discover(t, tr, srv)
	rec.Active(t, org, kinds.JiraDiscoveryFailing, "jp-sky")
	open := rec.Open(org, kinds.JiraDiscoveryFailing)
	if len(open) != 1 || open[0].Params != (kinds.JiraProject{JiraProjectID: "jp-sky", ProjectKey: "SKY"}) {
		t.Fatalf("open = %+v; want SKY alone, named by its key", open)
	}

	srv.answer("1001", answerOK)
	discover(t, tr, srv)
	rec.Inactive(t, org, kinds.JiraDiscoveryFailing)
}

// A query the salvage rebuilt keeps the project producing work, so however
// often it happens it raises nothing.
func TestDiscoverJira_ASalvagedQueryRaisesNothing(t *testing.T) {
	rec := notifytest.Record(t)
	const org = "org-discovery-salvaged"
	srv := newDeadStatusServer(t, "99999",
		jiraclient.Status{ID: "10001", Name: "To Do"},
		jiraclient.Status{ID: "10100", Name: "Done"})
	client := jiraclient.NewClient(jiraclient.CloudAPIToken(srv.URL, "me@example.com", "tok"))
	rules := salvageRules()
	rules[0].JiraProjectID = "jp-sky"
	tr := &Tracker{orgID: org}

	for range jiraDiscoveryDebounce + 1 {
		if _, err := tr.discoverJira(context.Background(), client, srv.URL, rules); err != nil {
			t.Fatalf("discoverJira: %v", err)
		}
	}
	rec.Inactive(t, org, kinds.JiraDiscoveryFailing)
}

// A project Jira did not answer is no evidence either way: its open occurrence
// is neither bumped nor resolved, and its count neither grows nor restarts.
func TestDiscoverJira_AnUnansweredProjectIsKept(t *testing.T) {
	rec := notifytest.Record(t)
	const org = "org-discovery-unanswered"
	srv := newDiscoveryServer(t)
	srv.answer("1001", answerRejected)
	tr := &Tracker{orgID: org}
	for range jiraDiscoveryDebounce {
		discover(t, tr, srv)
	}
	rec.Active(t, org, kinds.JiraDiscoveryFailing, "jp-sky")

	// OPS answers, so the cycle reached Jira, but SKY's queries did not.
	srv.answer("1001", answerUnavailable)
	discover(t, tr, srv)
	if open := rec.Open(org, kinds.JiraDiscoveryFailing); len(open) != 1 || open[0].Seen != 1 {
		t.Fatalf("open after an unanswered cycle = %+v; want SKY kept as it was", open)
	}

	// Still rejected: the streak the unanswered cycle left alone continues.
	srv.answer("1001", answerRejected)
	discover(t, tr, srv)
	if open := rec.Open(org, kinds.JiraDiscoveryFailing); len(open) != 1 || open[0].Seen != 2 {
		t.Fatalf("open after a further rejection = %+v; want SKY bumped", open)
	}
}

// A new process starts every count at zero. A project still failing must not
// resolve while its count climbs back to the debounce.
func TestDiscoverJira_ARestartedCountKeepsAnOpenOccurrence(t *testing.T) {
	rec := notifytest.Record(t)
	const org = "org-discovery-restart"
	srv := newDiscoveryServer(t)
	srv.answer("1001", answerRejected)
	before := &Tracker{orgID: org}
	for range jiraDiscoveryDebounce {
		discover(t, before, srv)
	}

	after := &Tracker{orgID: org}
	for range jiraDiscoveryDebounce - 1 {
		discover(t, after, srv)
		if open := rec.Open(org, kinds.JiraDiscoveryFailing); len(open) != 1 || open[0].Seen != 1 {
			t.Fatalf("open while the new count climbs = %+v; want SKY kept as it was", open)
		}
	}
	discover(t, after, srv)
	if open := rec.Open(org, kinds.JiraDiscoveryFailing); len(open) != 1 || open[0].Seen != 2 {
		t.Fatalf("open once the new count reaches the debounce = %+v; want SKY bumped", open)
	}
}

// Shared counts outlive the tracker that wrote them, which is how the poller's
// one-cycle trackers count across cycles.
func TestDiscoverJira_SharedCountsSpanTrackers(t *testing.T) {
	rec := notifytest.Record(t)
	const org = "org-discovery-shared"
	srv := newDiscoveryServer(t)
	srv.answer("1002", answerRejected)
	var shared JiraDiscoveryStreaks
	for range jiraDiscoveryDebounce {
		discover(t, (&Tracker{orgID: org}).WithJiraDiscoveryStreaks(&shared), srv)
	}
	rec.Active(t, org, kinds.JiraDiscoveryFailing, "jp-ops")

	// The org's last armed project is untracked: nothing can be failing, and
	// the count starts over.
	(&Tracker{orgID: org}).WithJiraDiscoveryStreaks(&shared).NoJiraDiscovery(context.Background())
	rec.Inactive(t, org, kinds.JiraDiscoveryFailing)
	discover(t, (&Tracker{orgID: org}).WithJiraDiscoveryStreaks(&shared), srv)
	rec.Inactive(t, org, kinds.JiraDiscoveryFailing)

	// A count one short of the debounce, then Reset: the next rejection is
	// the first of a new run, not the one that completes the old.
	for range jiraDiscoveryDebounce - 2 {
		discover(t, (&Tracker{orgID: org}).WithJiraDiscoveryStreaks(&shared), srv)
	}
	shared.Reset()
	discover(t, (&Tracker{orgID: org}).WithJiraDiscoveryStreaks(&shared), srv)
	rec.Inactive(t, org, kinds.JiraDiscoveryFailing)
}
