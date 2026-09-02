package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
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

	// srv.URL is a non-github host, so APIBase derives the GHES /api/v3 mount;
	// the test server answers 200 on any path.
	code, res := postReachability(t, handleGitHubReachability, `{"url":"`+srv.URL+`"}`)
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

	code, res := postReachability(t, handleGitHubReachability, `{"url":"`+addr+`"}`)
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
	for _, body := range []string{
		`{"url":""}`,
		`{"url":"not a url"}`,
		`{"url":"ftp://example.com"}`,
		`{"url":"example.com"}`,                        // no scheme
		`{"url":"https://host.example.com?x=1"}`,       // query has no place in a base URL
		`{"url":"https://host.example.com#frag"}`,      // fragment likewise
		`{"url":"https://user:pass@host.example.com"}`, // embedded credentials
	} {
		if code, _ := postReachability(t, handleGitHubReachability, body); code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, code)
		}
	}
}

func TestHandleJiraReachability_Reachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // a 401 still proves reachability
	}))
	defer srv.Close()

	code, res := postReachability(t, handleJiraReachability, `{"url":"`+srv.URL+`"}`)
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
	if code, _ := postReachability(t, handleJiraReachability, `{"url":"://bad"}`); code != http.StatusBadRequest {
		t.Errorf("malformed jira url: status = %d, want 400", code)
	}
}

// TestNormalizeReachabilityURL pins the validator the handlers gate on. It is
// the hermetic half of the *.ghe.com chain: this confirms a data-residency
// host passes through untouched (a full handler probe of api.<tenant>.ghe.com
// would force a flaky external DNS lookup), while ghbase.APIBase's
// own table covers octocorp.ghe.com -> api.octocorp.ghe.com.
func TestNormalizeReachabilityURL(t *testing.T) {
	good := []struct{ in, want string }{
		{"https://github.com", "https://github.com"},
		{"https://github.com/", "https://github.com"},                 // trailing slash trimmed
		{"  https://github.com  ", "https://github.com"},              // surrounding space trimmed
		{"https://octocorp.ghe.com", "https://octocorp.ghe.com"},      // ghe.com not rejected/mangled
		{"https://ghes.acme.com:8443", "https://ghes.acme.com:8443"},  // explicit port kept
		{"https://jira.acme.com/jira", "https://jira.acme.com/jira"},  // Jira context path preserved
		{"https://jira.acme.com/jira/", "https://jira.acme.com/jira"}, // ...slash still trimmed
	}
	for _, tt := range good {
		if got, ok := ghbase.NormalizeBaseURL(tt.in); !ok || got != tt.want {
			t.Errorf("ghbase.NormalizeBaseURL(%q) = (%q, %v), want (%q, true)", tt.in, got, ok, tt.want)
		}
	}

	bad := []string{
		"",
		"   ",
		"example.com",                    // no scheme
		"ftp://example.com",              // wrong scheme
		"https://",                       // no host
		"https://host.example.com?x=1",   // query
		"https://host.example.com#frag",  // fragment
		"https://user:pass@host.example", // userinfo
	}
	for _, in := range bad {
		if got, ok := ghbase.NormalizeBaseURL(in); ok {
			t.Errorf("ghbase.NormalizeBaseURL(%q) = (%q, true), want rejected", in, got)
		}
	}
}
