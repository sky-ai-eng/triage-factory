package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/reachability"
)

// postReachability drives a reachability handler directly (the handlers touch
// no Server state; route gating is covered by TestRoutesCoverage) and decodes
// the Result. It returns the HTTP status alongside so the malformed-input 400
// path is assertable.
func postReachability(t *testing.T, h http.HandlerFunc, body string) (int, reachability.Result) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h(rec, req)
	var res reachability.Result
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("decode body %q: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, res
}

func TestHandleGitHubReachability_Reachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	s := &Server{}

	// srv.URL is a non-github host, so APIBase derives the GHES /api/v3 mount;
	// the test server answers 200 on any path.
	code, res := postReachability(t, s.handleGitHubReachability, `{"url":"`+srv.URL+`"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !res.Reachable {
		t.Errorf("Reachable=false, want true; reason=%q", res.Reason)
	}
	if !strings.HasSuffix(res.ProbedURL, "/api/v3") {
		t.Errorf("ProbedURL = %q, want it to end with /api/v3 (derived API base)", res.ProbedURL)
	}
}

func TestHandleGitHubReachability_Unreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.URL
	srv.Close() // nothing listens now
	s := &Server{}

	code, res := postReachability(t, s.handleGitHubReachability, `{"url":"`+addr+`"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (unreachable is a body verdict, not an HTTP error)", code)
	}
	if res.Reachable {
		t.Errorf("Reachable=true, want false against a closed host")
	}
	if res.Reason == "" {
		t.Errorf("want a non-empty reason on an unreachable host")
	}
}

func TestHandleGitHubReachability_BadURL(t *testing.T) {
	s := &Server{}
	for _, body := range []string{
		`{"url":""}`,
		`{"url":"not a url"}`,
		`{"url":"ftp://example.com"}`,
		`{"url":"example.com"}`, // no scheme
	} {
		if code, _ := postReachability(t, s.handleGitHubReachability, body); code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, code)
		}
	}
}

func TestHandleJiraReachability_Reachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // a 401 still proves reachability
	}))
	defer srv.Close()
	s := &Server{}

	code, res := postReachability(t, s.handleJiraReachability, `{"url":"`+srv.URL+`"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !res.Reachable {
		t.Errorf("Reachable=false, want true (host answered 401); reason=%q", res.Reason)
	}
	if res.Status != http.StatusUnauthorized {
		t.Errorf("Result.Status = %d, want 401 passed through", res.Status)
	}
	if !strings.HasSuffix(res.ProbedURL, "/rest/api/2/serverInfo") {
		t.Errorf("ProbedURL = %q, want the Jira serverInfo path", res.ProbedURL)
	}
}

func TestHandleJiraReachability_BadURL(t *testing.T) {
	s := &Server{}
	if code, _ := postReachability(t, s.handleJiraReachability, `{"url":"://bad"}`); code != http.StatusBadRequest {
		t.Errorf("malformed jira url: status = %d, want 400", code)
	}
}
