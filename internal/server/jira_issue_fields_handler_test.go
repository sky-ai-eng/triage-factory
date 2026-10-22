package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func jiraPrioritiesPath() string { return jiraOrgPath(runmode.LocalDefaultOrgID) + "/priorities" }

func jiraIssueTypesListPath(key string) string { return jiraProjectPath(key) + "/issue-types/list" }

// readTwoPages lists path two items at a time and returns both pages' items
// and the first page's token, failing unless the list ends on the second page
// and serves a null total_count.
func readTwoPages[T any](t *testing.T, s *Server, path string) ([]T, string) {
	t.Helper()
	first := decodeList[T](t, doJSON(t, s, http.MethodPost, path, map[string]any{"page_size": 2}))
	second := decodeList[T](t, doJSON(t, s, http.MethodPost, path, map[string]any{"page_size": 2, "page_token": first.NextPageToken}))
	if first.NextPageToken == "" || second.NextPageToken != "" || first.TotalCount != nil || second.TotalCount != nil {
		t.Fatalf("pages = %+v then %+v, want two pages and a null total", first, second)
	}
	return append(first.Items, second.Items...), first.NextPageToken
}

// The instance's priorities page in Jira's order, each under the id a
// predicate names it by. One priority is read by that id; a name never
// reaches Jira.
func TestJiraPriorities(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY")
	got, _ := readTwoPages[jiraPriorityJSON](t, s, jiraPrioritiesPath()+"/list")
	if want := []jiraPriorityJSON{{ID: "1", Name: "Highest"}, {ID: "2", Name: "High"}, {ID: "3", Name: "Medium"}}; !slices.Equal(got, want) {
		t.Errorf("priorities = %+v, want %+v", got, want)
	}
	assertOneFault(t, doJSON(t, s, http.MethodPost, jiraPrioritiesPath()+"/list", map[string]any{"page_size": 0}),
		http.StatusBadRequest, "OUT_OF_RANGE", "page_size")
	assertOneFault(t, doJSON(t, s, http.MethodPost, jiraPrioritiesPath()+"/list", map[string]any{"q": "High"}),
		http.StatusBadRequest, "UNKNOWN_FIELD", "q")

	rec := doJSON(t, s, http.MethodGet, jiraPrioritiesPath()+"/2", nil)
	var one jiraPriorityJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &one)
	if rec.Code != http.StatusOK || one != (jiraPriorityJSON{ID: "2", Name: "High"}) {
		t.Errorf("GET priority = %d %+v, want 200 High", rec.Code, one)
	}
	assertOneFault(t, doJSON(t, s, http.MethodGet, jiraPrioritiesPath()+"/99", nil), http.StatusNotFound, "NOT_FOUND", "")
	before := fake.Calls()
	assertOneFault(t, doJSON(t, s, http.MethodGet, jiraPrioritiesPath()+"/High", nil), http.StatusNotFound, "NOT_FOUND", "")
	if fake.Calls() != before {
		t.Error("a priority name reached Jira")
	}
}

// A project's issue types page by name, under a token bound to the project. A
// project Jira cannot show is a 404, as is an issue type of another project at
// this project's address.
func TestJiraIssueTypes(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY", "OPS")
	fake.SetIssueTypes("SKY",
		fakeJiraStatus{ID: "10001", Name: "Task"}, fakeJiraStatus{ID: "10004", Name: "Bug"}, fakeJiraStatus{ID: "10002", Name: "Sub-task"})
	fake.SetIssueTypes("OPS", fakeJiraStatus{ID: "10100", Name: "Incident"})

	got, token := readTwoPages[jiraIssueTypeJSON](t, s, jiraIssueTypesListPath("SKY"))
	if want := []jiraIssueTypeJSON{{ID: "10004", Name: "Bug"}, {ID: "10002", Name: "Sub-task"}, {ID: "10001", Name: "Task"}}; !slices.Equal(got, want) {
		t.Errorf("issue types = %+v, want %+v", got, want)
	}
	assertOneFault(t, doJSON(t, s, http.MethodPost, jiraIssueTypesListPath("OPS"), map[string]any{"page_size": 2, "page_token": token}),
		http.StatusBadRequest, "INVALID_PARAM", "page_token")
	fake.Hide("GONE")
	assertOneFault(t, doJSON(t, s, http.MethodPost, jiraIssueTypesListPath("GONE"), map[string]any{}),
		http.StatusNotFound, "NOT_FOUND", "")
	assertOneFault(t, doJSON(t, s, http.MethodPost, jiraProjectSegmentPath("SKY")+"/issue-types/list", map[string]any{}),
		http.StatusNotFound, "NOT_FOUND", "")

	rec := doJSON(t, s, http.MethodGet, jiraProjectPath("OPS")+"/issue-types/10100", nil)
	var one jiraIssueTypeJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &one)
	if rec.Code != http.StatusOK || one != (jiraIssueTypeJSON{ID: "10100", Name: "Incident"}) {
		t.Errorf("GET issue type = %d %+v, want 200 Incident", rec.Code, one)
	}
	for _, path := range []string{"/issue-types/10100", "/issue-types/Bug"} {
		assertOneFault(t, doJSON(t, s, http.MethodGet, jiraProjectPath("SKY")+path, nil), http.StatusNotFound, "NOT_FOUND", "")
	}
}
