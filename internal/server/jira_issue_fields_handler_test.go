package server

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

func jiraPrioritiesPath() string { return jiraOrgPath(runmode.LocalDefaultOrgID) + "/priorities" }

func jiraIssueTypesListPath(key string) string { return jiraProjectPath(key) + "/issue-types/list" }

// readTwoPages lists path two items at a time and returns both pages' items
// and the first page's token, failing unless the list ends on the second page
// and serves a null total_count.
func readTwoPages[T any](t *testing.T, s *Server, path string) ([]T, string) {
	t.Helper()
	first := httpxtest.DecodeList[T](t, doJSON(t, s, http.MethodPost, path, map[string]any{"page_size": 2}))
	second := httpxtest.DecodeList[T](t, doJSON(t, s, http.MethodPost, path, map[string]any{"page_size": 2, "page_token": first.NextPageToken}))
	if first.NextPageToken == "" || second.NextPageToken != "" || first.TotalCount != nil || second.TotalCount != nil {
		t.Fatalf("pages = %+v then %+v, want two pages and a null total", first, second)
	}
	return append(first.Items, second.Items...), first.NextPageToken
}

// listNames lists path under body, one page, and returns the name of each row
// served, in order.
func listNames[T any](t *testing.T, s *Server, path string, body map[string]any, name func(T) string) []string {
	t.Helper()
	page := httpxtest.DecodeList[T](t, doJSON(t, s, http.MethodPost, path, body))
	names := make([]string, len(page.Items))
	for i, row := range page.Items {
		names[i] = name(row)
	}
	return names
}

// assertSearches lists path under each search and fails unless it serves
// exactly the names that search maps to, in order.
func assertSearches[T any](t *testing.T, s *Server, path string, name func(T) string, cases map[string][]string) {
	t.Helper()
	for search, want := range cases {
		if got := listNames(t, s, path, map[string]any{"search": search}, name); !slices.Equal(got, want) {
			t.Errorf("search %q = %q, want %q", search, got, want)
		}
	}
}

// assertTokenBoundToSearch mints a one-row page token under search and fails
// unless the token continues under a re-spelling of it in case and spacing,
// serving next, and is refused under other.
func assertTokenBoundToSearch[T any](t *testing.T, s *Server, path, search, other string, name func(T) string, next []string) {
	t.Helper()
	first := httpxtest.DecodeList[T](t, doJSON(t, s, http.MethodPost, path, map[string]any{"search": search, "page_size": 1}))
	if first.NextPageToken == "" {
		t.Fatalf("search %q page 1 carried no next_page_token", search)
	}
	assertOneFault(t, doJSON(t, s, http.MethodPost, path, map[string]any{"search": other, "page_size": 1, "page_token": first.NextPageToken}),
		http.StatusBadRequest, "INVALID_PARAM", "page_token")
	respelled := "  " + strings.ToUpper(search) + "  "
	got := listNames(t, s, path, map[string]any{"search": respelled, "page_size": 1, "page_token": first.NextPageToken}, name)
	if !slices.Equal(got, next) {
		t.Errorf("page 2 under %q = %q, want %q", respelled, got, next)
	}
}

// assertSearchCap: a search past httpx.MaxSearchRunes runes is an INVALID_FIELD
// on `search` that never reaches the upstream, and one of exactly that many
// runes, each of them multi-byte, is a search like any other. body is the rest
// of the request; calls counts the upstream's requests.
func assertSearchCap(t *testing.T, s *Server, path string, body map[string]any, calls func() int) {
	t.Helper()
	with := func(search string) map[string]any {
		out := maps.Clone(body)
		out["search"] = search
		return out
	}
	before := calls()
	assertOneFault(t, doJSON(t, s, http.MethodPost, path, with(strings.Repeat("x", httpx.MaxSearchRunes+1))),
		http.StatusBadRequest, "INVALID_FIELD", "search")
	if calls() != before {
		t.Error("a search past the cap reached the upstream")
	}
	if rec := doJSON(t, s, http.MethodPost, path, with(strings.Repeat("é", httpx.MaxSearchRunes))); rec.Code != http.StatusOK {
		t.Errorf("a search of %d runes = %d, want 200; body=%s", httpx.MaxSearchRunes, rec.Code, rec.Body.String())
	}
}

func jiraPriorityName(p jiraPriorityJSON) string    { return p.Name }
func jiraIssueTypeName(it jiraIssueTypeJSON) string { return it.Name }
func jiraStatusName(st jiraStatusJSON) string       { return st.Name }

// TestJiraPriorities_Search: the search narrows the instance's priorities in
// Jira's order before the window, a % or an _ in it is the character itself,
// and a token continues only its own search. The old name of the field is
// refused, and the cap holds.
func TestJiraPriorities_Search(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY")
	path := jiraPrioritiesPath() + "/list"
	assertSearches(t, s, path, jiraPriorityName, map[string][]string{
		"high":    {"Highest", "High"},
		" MED ":   {"Medium"},
		"nothing": {},
		"":        {"Highest", "High", "Medium"},
	})
	assertTokenBoundToSearch(t, s, path, "high", "med", jiraPriorityName, []string{"High"})

	fake.SetPriorities(fakeJiraStatus{ID: "1", Name: "P1 100%"}, fakeJiraStatus{ID: "2", Name: "P2 1000"},
		fakeJiraStatus{ID: "3", Name: "P_3"}, fakeJiraStatus{ID: "4", Name: "PX3"})
	assertSearches(t, s, path, jiraPriorityName, map[string][]string{
		"100%": {"P1 100%"},
		"p_3":  {"P_3"},
	})

	assertOneFault(t, doJSON(t, s, http.MethodPost, path, map[string]any{"q": "high"}),
		http.StatusBadRequest, "UNKNOWN_FIELD", "q")
	assertSearchCap(t, s, path, map[string]any{}, fake.Calls)
}

// TestJiraIssueTypes_Search: the search narrows a project's issue types, by
// name, before the window, literally, under a token bound to it.
func TestJiraIssueTypes_Search(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY")
	fake.SetIssueTypes("SKY",
		fakeJiraStatus{ID: "10001", Name: "Task"}, fakeJiraStatus{ID: "10002", Name: "Sub-task"},
		fakeJiraStatus{ID: "10004", Name: "Bug"}, fakeJiraStatus{ID: "10005", Name: "50% Spike"},
		fakeJiraStatus{ID: "10006", Name: "50 Spikes"})
	path := jiraIssueTypesListPath("SKY")
	assertSearches(t, s, path, jiraIssueTypeName, map[string][]string{
		"TASK": {"Sub-task", "Task"},
		"50%":  {"50% Spike"},
		"_":    {},
	})
	assertTokenBoundToSearch(t, s, path, "task", "bug", jiraIssueTypeName, []string{"Task"})
	assertSearchCap(t, s, path, map[string]any{}, fake.Calls)
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
