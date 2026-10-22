package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func jiraPrioritiesListPath() string {
	return jiraOrgPath(runmode.LocalDefaultOrgID) + "/priorities/list"
}

func jiraIssueTypesListPath(key string) string { return jiraProjectPath(key) + "/issue-types/list" }

// TestJiraPrioritiesList_PagesTheInstancesPriorities walks the priorities a
// page at a time, in Jira's order, each carrying the id a predicate names it
// by; total_count is null.
func TestJiraPrioritiesList_PagesTheInstancesPriorities(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY")
	var got []jiraPriorityJSON
	token := ""
	for pages := 0; ; pages++ {
		if pages > len(jiraFixturePriorities) {
			t.Fatal("the priorities list never stopped minting tokens")
		}
		body := map[string]any{"page_size": 2}
		if token != "" {
			body["page_token"] = token
		}
		page := decodeList[jiraPriorityJSON](t, doJSON(t, s, http.MethodPost, jiraPrioritiesListPath(), body))
		if page.TotalCount != nil {
			t.Errorf("total_count = %d, want null", *page.TotalCount)
		}
		got = append(got, page.Items...)
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	want := []jiraPriorityJSON{{ID: "1", Name: "Highest"}, {ID: "2", Name: "High"}, {ID: "3", Name: "Medium"}}
	if !slices.Equal(got, want) {
		t.Errorf("priorities = %+v, want %+v", got, want)
	}

	assertOneFault(t, doJSON(t, s, http.MethodPost, jiraPrioritiesListPath(), map[string]any{"page_size": 0}),
		http.StatusBadRequest, "OUT_OF_RANGE", "page_size")
	assertOneFault(t, doJSON(t, s, http.MethodPost, jiraPrioritiesListPath(), map[string]any{"q": "High"}),
		http.StatusBadRequest, "UNKNOWN_FIELD", "q")
}

// TestJiraPriorityGet: one priority by its id, as the list serves it. An id
// Jira does not know is a 404, and one outside the grammar never reaches Jira.
func TestJiraPriorityGet(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY")
	rec := doJSON(t, s, http.MethodGet, jiraOrgPath(runmode.LocalDefaultOrgID)+"/priorities/2", nil)
	var got jiraPriorityJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || got != (jiraPriorityJSON{ID: "2", Name: "High"}) {
		t.Errorf("GET priority = %d %+v, want 200 High", rec.Code, got)
	}
	assertOneFault(t, doJSON(t, s, http.MethodGet, jiraOrgPath(runmode.LocalDefaultOrgID)+"/priorities/99", nil),
		http.StatusNotFound, "NOT_FOUND", "")
	before := fake.Calls()
	assertOneFault(t, doJSON(t, s, http.MethodGet, jiraOrgPath(runmode.LocalDefaultOrgID)+"/priorities/High", nil),
		http.StatusNotFound, "NOT_FOUND", "")
	if fake.Calls() != before {
		t.Error("a priority name reached Jira")
	}
}

// TestJiraIssueTypesList_PagesTheProjectsIssueTypes reads a project's issue
// types ordered by name; a project Jira cannot show is a 404, and a page token
// is bound to its project.
func TestJiraIssueTypesList_PagesTheProjectsIssueTypes(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY", "OPS")
	fake.SetIssueTypes("SKY",
		fakeJiraStatus{ID: "10001", Name: "Task"}, fakeJiraStatus{ID: "10004", Name: "Bug"}, fakeJiraStatus{ID: "10002", Name: "Sub-task"})

	first := decodeList[jiraIssueTypeJSON](t, doJSON(t, s, http.MethodPost, jiraIssueTypesListPath("SKY"), map[string]any{"page_size": 2}))
	if first.TotalCount != nil || first.NextPageToken == "" {
		t.Fatalf("page 1 = %+v, want a null total and a next page", first)
	}
	second := decodeList[jiraIssueTypeJSON](t, doJSON(t, s, http.MethodPost, jiraIssueTypesListPath("SKY"),
		map[string]any{"page_size": 2, "page_token": first.NextPageToken}))
	got := append(first.Items, second.Items...)
	want := []jiraIssueTypeJSON{{ID: "10004", Name: "Bug"}, {ID: "10002", Name: "Sub-task"}, {ID: "10001", Name: "Task"}}
	if !slices.Equal(got, want) || second.NextPageToken != "" {
		t.Errorf("issue types = %+v (next %q), want %+v", got, second.NextPageToken, want)
	}

	assertOneFault(t, doJSON(t, s, http.MethodPost, jiraIssueTypesListPath("OPS"),
		map[string]any{"page_size": 2, "page_token": first.NextPageToken}),
		http.StatusBadRequest, "INVALID_PARAM", "page_token")
	fake.Hide("GONE")
	assertOneFault(t, doJSON(t, s, http.MethodPost, jiraIssueTypesListPath("GONE"), map[string]any{}),
		http.StatusNotFound, "NOT_FOUND", "")
	assertOneFault(t, doJSON(t, s, http.MethodPost, jiraProjectSegmentPath("SKY")+"/issue-types/list", map[string]any{}),
		http.StatusNotFound, "NOT_FOUND", "")
}

// TestJiraIssueTypeGet: one issue type through a project that uses it. An
// issue type of another project is a 404 at this project's address.
func TestJiraIssueTypeGet(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY", "OPS")
	fake.SetIssueTypes("OPS", fakeJiraStatus{ID: "10100", Name: "Incident"})

	rec := doJSON(t, s, http.MethodGet, jiraProjectPath("OPS")+"/issue-types/10100", nil)
	var got jiraIssueTypeJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || got != (jiraIssueTypeJSON{ID: "10100", Name: "Incident"}) {
		t.Errorf("GET issue type = %d %+v, want 200 Incident", rec.Code, got)
	}
	assertOneFault(t, doJSON(t, s, http.MethodGet, jiraProjectPath("SKY")+"/issue-types/10100", nil),
		http.StatusNotFound, "NOT_FOUND", "")
	assertOneFault(t, doJSON(t, s, http.MethodGet, jiraProjectPath("SKY")+"/issue-types/Bug", nil),
		http.StatusNotFound, "NOT_FOUND", "")
}
