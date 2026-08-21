package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/zalando/go-keyring"
)

// The Jira ids the write gate is exercised against. Statuses are addressed by
// id everywhere on this surface — the id is what a workflow references, so it
// is what the rules store and what the PUT accepts — and these are the three a
// fixture project's workflow offers.
const (
	statusToDoID       = "10000"
	statusInProgressID = "10001"
	statusDoneID       = "10002"
	statusUnknownID    = "99999"
)

var jiraFixtureStatuses = []fakeJiraStatus{
	{ID: statusToDoID, Name: "To Do"},
	{ID: statusInProgressID, Name: "In Progress"},
	{ID: statusDoneID, Name: "Done"},
}

type fakeJiraStatus struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// jiraCatalogFake stands in for a Data Center instance's two read endpoints:
// /project, which serves the whole project catalog in one unpaged, unfiltered
// response, and /project/{key}/statuses, which serves one project's workflow
// statuses grouped by issue type.
//
// It is the right fixture for every reader under test — the picker's list
// route and both halves of the PUT's write gate — because each page or lookup
// they produce is windowed and filtered from these answers, so a paging,
// filtering or resolution bug shows up as wrong rows rather than as a stub that
// ran out of scripted responses.
type jiraCatalogFake struct {
	URL string

	mu       sync.Mutex
	keys     []string
	statuses map[string][]fakeJiraStatus
	failing  bool
	calls    int
}

// newJiraCatalogFake starts a fake serving the given project keys, each with
// the fixture workflow.
func newJiraCatalogFake(t *testing.T, keys ...string) *jiraCatalogFake {
	t.Helper()
	f := &jiraCatalogFake{keys: keys, statuses: map[string][]fakeJiraStatus{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls++
		failing, keys := f.failing, append([]string(nil), f.keys...)
		project, isStatuses := projectFromStatusesPath(r.URL.Path)
		statuses, custom := f.statuses[project]
		f.mu.Unlock()

		if failing {
			// 401 rather than a 5xx: an idempotent GET retries a 5xx three
			// times with backoff, and the behaviour under test is what the
			// handler does with an upstream that failed, not how the client
			// gets there.
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"errorMessages":["Client must be authenticated"]}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if isStatuses {
			if !custom {
				statuses = jiraFixtureStatuses
			}
			// Jira answers this one as a list of issue types, each carrying the
			// statuses its workflow offers; the client unions and dedupes them.
			body, _ := json.Marshal([]map[string]any{{"name": "Task", "statuses": statuses}})
			_, _ = w.Write(body)
			return
		}
		rows := make([]string, len(keys))
		for i, k := range keys {
			rows[i] = `{"key": "` + k + `", "name": "` + k + ` Project"}`
		}
		_, _ = io.WriteString(w, "["+strings.Join(rows, ",")+"]")
	}))
	t.Cleanup(srv.Close)
	f.URL = srv.URL
	return f
}

// projectFromStatusesPath recognizes /rest/api/{v}/project/{key}/statuses.
func projectFromStatusesPath(path string) (string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 6 && parts[3] == "project" && parts[5] == "statuses" {
		return parts[4], true
	}
	return "", false
}

// Calls is how many times Jira was reached — the assertion behind "a write that
// changes nothing about Jira asks Jira nothing at all".
func (f *jiraCatalogFake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// SetKeys replaces the catalog, standing in for a project created or deleted in
// Jira after TF stored it.
func (f *jiraCatalogFake) SetKeys(keys ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = keys
}

// SetStatuses replaces one project's workflow — a status retired or renamed
// upstream after a team armed its rules.
func (f *jiraCatalogFake) SetStatuses(project string, statuses ...fakeJiraStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses[project] = statuses
}

// SetFailing makes every later read fail, standing in for an unreachable Jira.
func (f *jiraCatalogFake) SetFailing(failing bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failing = failing
}

// newServerWithJiraCatalog is a test server whose org holds a Data Center Jira
// credential pointed at a fake catalog carrying the given project keys.
func newServerWithJiraCatalog(t *testing.T, keys ...string) (*Server, *jiraCatalogFake) {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	fake := newJiraCatalogFake(t, keys...)
	s := newTestServer(t)
	if err := integrations.Save(t.Context(), s.secrets, runmode.LocalDefaultOrgID,
		auth.Credentials{JiraURL: fake.URL, JiraPAT: "org-pat"}); err != nil {
		t.Fatalf("seed jira creds: %v", err)
	}
	return s, fake
}

// jiraRef builds one status ref the way a rule armed through the API carries
// it: the id, which is the identity, plus the display name resolved for it. The
// id is derived from the name so a test can name a status once and get the same
// ref wherever it appears — these are for rules seeded straight into the store,
// which never pass through the resolution the handler does.
func jiraRef(name string) domain.JiraStatusRef {
	return domain.JiraStatusRef{ID: "st-" + name, Name: name}
}

func jiraRefs(names ...string) []domain.JiraStatusRef {
	refs := make([]domain.JiraStatusRef, 0, len(names))
	for _, n := range names {
		refs = append(refs, jiraRef(n))
	}
	return refs
}
