package jira

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

// projectServer replies with body and records every request URL, so a test can
// assert on the query string the arm under test built as well as on the page it
// parsed back.
func projectServer(t *testing.T, body string) (*httptest.Server, func() []*url.URL) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []*url.URL
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []*url.URL {
		mu.Lock()
		defer mu.Unlock()
		return seen
	}
}

func projectKeys(projects []Project) []string {
	keys := make([]string, len(projects))
	for i, p := range projects {
		keys[i] = p.Key
	}
	return keys
}

func wantProjectKeys(t *testing.T, got []Project, want ...string) {
	t.Helper()
	gotKeys := projectKeys(got)
	if len(gotKeys) != len(want) {
		t.Fatalf("keys = %v, want %v", gotKeys, want)
	}
	for i := range want {
		if gotKeys[i] != want[i] {
			t.Fatalf("keys = %v, want %v", gotKeys, want)
		}
	}
}

func dcClient(rawURL string) *Client { return NewClient(DataCenterPAT(rawURL, "tok")) }

// TestListProjects_CloudPagesUpstream pins the Cloud arm: the versioned
// /project/search path, the window and the filter both sent upstream, and the
// resume offset taken from what came back.
func TestListProjects_CloudPagesUpstream(t *testing.T) {
	srv, requests := projectServer(t, `{
		"values": [
			{"key": "SKY", "name": "Sky Platform"},
			{"key": "OPS", "name": "Operations"}
		],
		"isLast": false
	}`)
	page, err := cloudClient(srv.URL).ListProjects(t.Context(), "sk", 4, 2)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	wantProjectKeys(t, page.Projects, "SKY", "OPS")
	if page.Projects[0].Name != "Sky Platform" {
		t.Errorf("name = %q, want %q", page.Projects[0].Name, "Sky Platform")
	}
	if page.NextStartAt != 6 {
		t.Errorf("NextStartAt = %d, want 6 (startAt 4 + 2 rows)", page.NextStartAt)
	}
	got := requests()
	if len(got) != 1 {
		t.Fatalf("requests = %d, want 1 (the catalog read is a single upstream call)", len(got))
	}
	if got[0].Path != "/rest/api/3/project/search" {
		t.Errorf("path = %q, want the v3 project-search endpoint", got[0].Path)
	}
	q := got[0].Query()
	if q.Get("startAt") != "4" || q.Get("maxResults") != "2" {
		t.Errorf("window = startAt %q maxResults %q, want 4 / 2", q.Get("startAt"), q.Get("maxResults"))
	}
	if q.Get("query") != "sk" {
		t.Errorf("query = %q, want the caller's filter passed upstream", q.Get("query"))
	}
}

// TestListProjects_CloudEmptyQueryOmitted keeps an unfiltered read the request
// Jira has always been sent: no query parameter at all rather than an empty one.
func TestListProjects_CloudEmptyQueryOmitted(t *testing.T) {
	srv, requests := projectServer(t, `{"values": [], "isLast": true}`)
	if _, err := cloudClient(srv.URL).ListProjects(t.Context(), "  ", 0, 50); err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if q := requests()[0].Query(); q.Has("query") {
		t.Errorf("query parameter present (%q) on an unfiltered read", q.Get("query"))
	}
}

// TestListProjects_CloudIsLastEndsTheWalk — the documented end-of-catalog
// signal wins even on a full page.
func TestListProjects_CloudIsLastEndsTheWalk(t *testing.T) {
	srv, _ := projectServer(t, `{"values": [{"key": "SKY", "name": "Sky"}], "isLast": true}`)
	page, err := cloudClient(srv.URL).ListProjects(t.Context(), "", 0, 1)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if page.NextStartAt != 0 {
		t.Errorf("NextStartAt = %d, want 0 on isLast:true", page.NextStartAt)
	}
}

// TestListProjects_CloudShortPageWithoutIsLast covers a response that omits
// isLast: a page shorter than the ask cannot be followed by anything.
func TestListProjects_CloudShortPageWithoutIsLast(t *testing.T) {
	srv, _ := projectServer(t, `{"values": [{"key": "SKY", "name": "Sky"}]}`)
	page, err := cloudClient(srv.URL).ListProjects(t.Context(), "", 0, 50)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if page.NextStartAt != 0 {
		t.Errorf("NextStartAt = %d, want 0 on a short page", page.NextStartAt)
	}
}

// TestListProjects_CloudFullPageWithoutIsLast is the other half: a full page
// and no isLast means there may be more, so the walk continues.
func TestListProjects_CloudFullPageWithoutIsLast(t *testing.T) {
	srv, _ := projectServer(t, `{"values": [{"key": "SKY", "name": "Sky"}, {"key": "OPS", "name": "Ops"}]}`)
	page, err := cloudClient(srv.URL).ListProjects(t.Context(), "", 10, 2)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if page.NextStartAt != 12 {
		t.Errorf("NextStartAt = %d, want 12", page.NextStartAt)
	}
}

const dcCatalog = `[
	{"key": "SKY", "name": "Sky Platform"},
	{"key": "OPS", "name": "Operations"},
	{"key": "INFRA", "name": "Infrastructure"},
	{"key": "DESK", "name": "Service Desk"}
]`

// TestListProjects_DataCenterWholeCatalog pins the DC arm: one unpaged,
// unfiltered v2 call, windowed here.
func TestListProjects_DataCenterWholeCatalog(t *testing.T) {
	srv, requests := projectServer(t, dcCatalog)
	page, err := dcClient(srv.URL).ListProjects(t.Context(), "", 0, 2)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	wantProjectKeys(t, page.Projects, "SKY", "OPS")
	if page.NextStartAt != 2 {
		t.Errorf("NextStartAt = %d, want 2", page.NextStartAt)
	}
	got := requests()
	if got[0].Path != "/rest/api/2/project" {
		t.Errorf("path = %q, want the v2 project catalog", got[0].Path)
	}
	if got[0].RawQuery != "" {
		t.Errorf("query = %q, want none — this endpoint takes neither a window nor a filter", got[0].RawQuery)
	}
}

// TestListProjects_DataCenterFiltersInHand — the filter Cloud sends upstream is
// applied here instead, against both the key and the name, case-insensitively.
func TestListProjects_DataCenterFiltersInHand(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		want  []string
	}{
		{"key substring", "sk", []string{"SKY", "DESK"}},
		{"name substring", "infra", []string{"INFRA"}},
		{"no match", "zzz", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := projectServer(t, dcCatalog)
			page, err := dcClient(srv.URL).ListProjects(t.Context(), tc.query, 0, 50)
			if err != nil {
				t.Fatalf("ListProjects: %v", err)
			}
			wantProjectKeys(t, page.Projects, tc.want...)
			if page.NextStartAt != 0 {
				t.Errorf("NextStartAt = %d, want 0 — the filtered set fits one page", page.NextStartAt)
			}
		})
	}
}

// TestListProjects_DataCenterWindowsTheFilteredSet: the window is applied
// after the filter, so paging a filtered read doesn't skip matches.
func TestListProjects_DataCenterWindowsTheFilteredSet(t *testing.T) {
	srv, _ := projectServer(t, dcCatalog)
	page, err := dcClient(srv.URL).ListProjects(t.Context(), "sk", 1, 1)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	wantProjectKeys(t, page.Projects, "DESK")
	if page.NextStartAt != 0 {
		t.Errorf("NextStartAt = %d, want 0 — DESK is the last match", page.NextStartAt)
	}
}

// TestListProjects_DataCenterPastTheEnd answers an empty page rather than
// erroring, so a stale token costs a caller nothing but a wasted round trip.
func TestListProjects_DataCenterPastTheEnd(t *testing.T) {
	srv, _ := projectServer(t, dcCatalog)
	page, err := dcClient(srv.URL).ListProjects(t.Context(), "", 99, 50)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(page.Projects) != 0 || page.NextStartAt != 0 {
		t.Errorf("page = %+v, want an empty last page", page)
	}
}
