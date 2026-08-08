package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// searchRequest is one decoded request the fake search server saw: the path it
// arrived on plus the JSON body, kept as a generic map so a test can assert on
// the presence of a key (startAt, nextPageToken) as well as its value.
type searchRequest struct {
	path string
	body map[string]any
}

// searchServer replies to successive search requests with the given bodies,
// recording each request. Running out of scripted pages fails the test rather
// than serving an empty page, so an unexpected extra round trip is loud.
func searchServer(t *testing.T, pages ...string) (*httptest.Server, func() []searchRequest) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []searchRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := map[string]any{}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("decode request body %q: %v", raw, err)
		}
		mu.Lock()
		seen = append(seen, searchRequest{path: r.URL.Path, body: body})
		n := len(seen)
		mu.Unlock()

		if n > len(pages) {
			t.Errorf("request %d: server has only %d scripted pages", n, len(pages))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, pages[n-1])
	}))
	t.Cleanup(srv.Close)
	return srv, func() []searchRequest {
		mu.Lock()
		defer mu.Unlock()
		return seen
	}
}

func cloudClient(url string) *Client {
	return NewClient(CloudAPIToken(url, "me@example.com", "tok"))
}

func issueKeys(issues []Issue) []string {
	keys := make([]string, len(issues))
	for i, is := range issues {
		keys[i] = is.Key
	}
	return keys
}

func wantKeys(t *testing.T, got []Issue, want ...string) {
	t.Helper()
	if fmt.Sprint(issueKeys(got)) != fmt.Sprint(want) {
		t.Errorf("keys = %v, want %v", issueKeys(got), want)
	}
}

// TestSearchIssues_CloudWalksNextPageToken is the migration's core guarantee:
// a Cloud search hits the enhanced endpoint and follows nextPageToken, so a
// result set larger than one page comes back whole rather than truncated to
// the first page.
func TestSearchIssues_CloudWalksNextPageToken(t *testing.T) {
	srv, requests := searchServer(t,
		`{"issues":[{"key":"SKY-1"},{"key":"SKY-2"}],"nextPageToken":"tok-2","isLast":false}`,
		`{"issues":[{"key":"SKY-3"}],"isLast":true}`,
	)

	issues, err := cloudClient(srv.URL).SearchIssues(context.Background(), "project = SKY", []string{"summary"}, 10)
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	wantKeys(t, issues, "SKY-1", "SKY-2", "SKY-3")

	reqs := requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	for i, r := range reqs {
		if r.path != "/rest/api/3/search/jql" {
			t.Errorf("request %d path = %q, want the enhanced search endpoint", i+1, r.path)
		}
	}
	if _, ok := reqs[0].body["nextPageToken"]; ok {
		t.Errorf("first request carried a page token: %v", reqs[0].body)
	}
	if got := reqs[1].body["nextPageToken"]; got != "tok-2" {
		t.Errorf("second request nextPageToken = %v, want tok-2", got)
	}
	// The second page asks only for what the first left outstanding.
	if got := reqs[1].body["maxResults"]; got != float64(8) {
		t.Errorf("second request maxResults = %v, want 8", got)
	}
}

// TestSearchIssues_CloudStopsWithoutToken pins the end-of-results signal that
// actually matters: Atlassian documents an absent nextPageToken as the last
// page, and isLast is not reliably present, so the walk must end on the
// missing token alone.
func TestSearchIssues_CloudStopsWithoutToken(t *testing.T) {
	srv, requests := searchServer(t, `{"issues":[{"key":"SKY-1"}]}`)

	issues, err := cloudClient(srv.URL).SearchIssues(context.Background(), "project = SKY", nil, 10)
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	wantKeys(t, issues, "SKY-1")
	if n := len(requests()); n != 1 {
		t.Errorf("requests = %d, want 1 (no token means no further page)", n)
	}
}

// TestSearchIssues_CloudStopsOnIsLast covers the other direction: a backend
// that sends both signals and contradicts itself — a token alongside
// isLast:true — is taken at its word that the results are done.
func TestSearchIssues_CloudStopsOnIsLast(t *testing.T) {
	srv, requests := searchServer(t, `{"issues":[{"key":"SKY-1"}],"nextPageToken":"tok-2","isLast":true}`)

	issues, err := cloudClient(srv.URL).SearchIssues(context.Background(), "project = SKY", nil, 10)
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	wantKeys(t, issues, "SKY-1")
	if n := len(requests()); n != 1 {
		t.Errorf("requests = %d, want 1 (isLast ends the walk)", n)
	}
}

// TestSearchIssues_CloudStopsAtMaxResults pins the cap as the caller's, not
// the server's: once maxResults is met the walk stops holding a live token,
// and an over-long page is trimmed rather than handed back.
func TestSearchIssues_CloudStopsAtMaxResults(t *testing.T) {
	srv, requests := searchServer(t,
		`{"issues":[{"key":"SKY-1"},{"key":"SKY-2"}],"nextPageToken":"tok-2"}`,
	)

	issues, err := cloudClient(srv.URL).SearchIssues(context.Background(), "project = SKY", nil, 1)
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	wantKeys(t, issues, "SKY-1")
	if n := len(requests()); n != 1 {
		t.Errorf("requests = %d, want 1 (cap met on the first page)", n)
	}
}

// TestSearchIssues_CloudFillsDefaultFields guards the one place the enhanced
// endpoint's defaults bite: it returns the issue id alone when fields is
// absent, so a nil list has to be filled in client-side.
func TestSearchIssues_CloudFillsDefaultFields(t *testing.T) {
	srv, requests := searchServer(t, `{"issues":[]}`)

	if _, err := cloudClient(srv.URL).SearchIssues(context.Background(), "project = SKY", nil, 10); err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	sent, ok := requests()[0].body["fields"].([]any)
	if !ok {
		t.Fatalf("fields = %v, want a list", requests()[0].body["fields"])
	}
	if len(sent) != len(DefaultSearchFields) {
		t.Fatalf("fields = %v, want DefaultSearchFields %v", sent, DefaultSearchFields)
	}
	for i, f := range DefaultSearchFields {
		if sent[i] != f {
			t.Errorf("fields[%d] = %v, want %q", i, sent[i], f)
		}
	}
}

// TestSearchIssues_DataCenterFirstRequestUnchanged pins the promise made to
// the untouched deployment: the classic endpoint, and the same three-key body
// it has always been sent — in particular no startAt on the first page.
func TestSearchIssues_DataCenterFirstRequestUnchanged(t *testing.T) {
	srv, requests := searchServer(t, `{"issues":[{"key":"SKY-1"}],"total":1}`)

	if _, err := testClient(srv.URL).SearchIssues(context.Background(), "project = SKY", []string{"summary"}, 10); err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	req := requests()[0]
	if req.path != "/rest/api/2/search" {
		t.Errorf("path = %q, want the classic search endpoint", req.path)
	}
	if _, ok := req.body["startAt"]; ok {
		t.Errorf("first request carried startAt: %v", req.body)
	}
	if got := req.body["jql"]; got != "project = SKY" {
		t.Errorf("jql = %v", got)
	}
	if got := req.body["maxResults"]; got != float64(10) {
		t.Errorf("maxResults = %v, want the caller's 10", got)
	}
	if len(req.body) != 3 {
		t.Errorf("body has %d keys (%v), want exactly jql/maxResults/fields", len(req.body), req.body)
	}
}

// TestSearchIssues_DataCenterWalksStartAt pins the same multi-page guarantee
// on the offset-paginated side, including the stop once the reported total is
// reached — no round trip is spent discovering an empty page.
func TestSearchIssues_DataCenterWalksStartAt(t *testing.T) {
	srv, requests := searchServer(t,
		`{"issues":[{"key":"SKY-1"},{"key":"SKY-2"}],"total":3}`,
		`{"issues":[{"key":"SKY-3"}],"total":3}`,
	)

	issues, err := testClient(srv.URL).SearchIssues(context.Background(), "project = SKY", nil, 10)
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	wantKeys(t, issues, "SKY-1", "SKY-2", "SKY-3")

	reqs := requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	if got := reqs[1].body["startAt"]; got != float64(2) {
		t.Errorf("second request startAt = %v, want 2", got)
	}
	if got := reqs[1].body["maxResults"]; got != float64(8) {
		t.Errorf("second request maxResults = %v, want 8", got)
	}
}

// TestSearchIssues_DataCenterStopsWithoutTotal covers a response with no
// total: the classic endpoint always reports one, so its absence is a backend
// this walk knows nothing about — take the page as complete, which is what
// this deployment did before there was a walk at all.
func TestSearchIssues_DataCenterStopsWithoutTotal(t *testing.T) {
	srv, requests := searchServer(t, `{"issues":[{"key":"SKY-1"}]}`)

	issues, err := testClient(srv.URL).SearchIssues(context.Background(), "project = SKY", nil, 10)
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	wantKeys(t, issues, "SKY-1")
	if n := len(requests()); n != 1 {
		t.Errorf("requests = %d, want 1 (no total licenses no further page)", n)
	}
}

// TestSearchIssues_PageBudget bounds the walk against a backend that never
// stops offering pages — here one that replays the same token forever. The
// call must return, and must not exceed its budget.
func TestSearchIssues_PageBudget(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"issues":[],"nextPageToken":"same-token-forever"}`)
	}))
	t.Cleanup(srv.Close)

	issues, err := cloudClient(srv.URL).SearchIssues(context.Background(), "project = SKY", nil, 100)
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	if len(issues) != 0 {
		t.Errorf("issues = %v, want none", issueKeys(issues))
	}
	if n != maxSearchPages {
		t.Errorf("requests = %d, want the %d-page budget", n, maxSearchPages)
	}
}

// TestCloudSearchURL pins the enhanced endpoint at v3 regardless of the
// Config's APIVersion: v3 is where Atlassian's removal notice sends callers,
// and a v2 alias of this endpoint is not something we rely on existing.
func TestCloudSearchURL(t *testing.T) {
	cfg := CloudAPIToken("https://acme.atlassian.net/", "me@example.com", "tok")
	if got, want := cfg.cloudSearchURL(), "https://acme.atlassian.net/rest/api/3/search/jql"; got != want {
		t.Errorf("cloudSearchURL() = %q, want %q", got, want)
	}
	cfg.APIVersion = APIv2
	if got, want := cfg.cloudSearchURL(), "https://acme.atlassian.net/rest/api/3/search/jql"; got != want {
		t.Errorf("cloudSearchURL() with APIVersion v2 = %q, want %q", got, want)
	}
}
