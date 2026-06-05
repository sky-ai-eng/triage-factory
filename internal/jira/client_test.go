package jira

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// capture records the Authorization header and request path the test server
// saw. It is mutex-guarded so reads in the test goroutine are race-free
// against the server's handler goroutine even under `go test -race`.
type capture struct {
	mu   sync.Mutex
	auth string
	path string
}

func (c *capture) record(r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.auth = r.Header.Get("Authorization")
	c.path = r.URL.Path
}

func (c *capture) read() (auth, path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.auth, c.path
}

// captureServer returns an httptest server that records each request and
// replies with body (a JSON document the client method under test can parse).
func captureServer(t *testing.T, body string) (*httptest.Server, *capture) {
	t.Helper()
	rec := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// TestDataCenterPATConfig pins the {Bearer, v2, DataCenter} shape.
func TestDataCenterPATConfig(t *testing.T) {
	cfg := DataCenterPAT("https://jira.example.com", "tok")
	if cfg.Deployment != DeploymentDataCenter {
		t.Errorf("Deployment = %q, want %q", cfg.Deployment, DeploymentDataCenter)
	}
	if cfg.APIVersion != APIv2 {
		t.Errorf("APIVersion = %q, want %q", cfg.APIVersion, APIv2)
	}
	if _, ok := cfg.auth.(bearerAuth); !ok {
		t.Errorf("auth = %T, want bearerAuth", cfg.auth)
	}
}

// TestCloudAPITokenConfig pins the {Basic, v3, Cloud} shape.
func TestCloudAPITokenConfig(t *testing.T) {
	cfg := CloudAPIToken("https://acme.atlassian.net", "me@acme.com", "tok")
	if cfg.Deployment != DeploymentCloud {
		t.Errorf("Deployment = %q, want %q", cfg.Deployment, DeploymentCloud)
	}
	if cfg.APIVersion != APIv3 {
		t.Errorf("APIVersion = %q, want %q", cfg.APIVersion, APIv3)
	}
	if _, ok := cfg.auth.(basicAuth); !ok {
		t.Errorf("auth = %T, want basicAuth", cfg.auth)
	}
}

// TestBearerAuthApply checks the scheme renders "Bearer <token>".
func TestBearerAuthApply(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "http://example.test", nil)
	bearerAuth{token: "tok-123"}.apply(req)
	if got, want := req.Header.Get("Authorization"), "Bearer tok-123"; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
}

// TestBasicAuthApply checks the scheme renders "Basic base64(email:token)".
func TestBasicAuthApply(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "http://example.test", nil)
	basicAuth{email: "me@acme.com", token: "tok-123"}.apply(req)
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("me@acme.com:tok-123"))
	if got := req.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
}

// TestAPIURLVersion checks v2 vs v3 land in the path, and that a trailing
// slash on BaseURL is trimmed (no "//rest").
func TestAPIURLVersion(t *testing.T) {
	v2 := DataCenterPAT("https://jira.example.com", "tok").apiURL("issue/%s", "PROJ-1")
	if want := "https://jira.example.com/rest/api/2/issue/PROJ-1"; v2 != want {
		t.Errorf("v2 apiURL = %q, want %q", v2, want)
	}
	v3 := CloudAPIToken("https://acme.atlassian.net", "me@acme.com", "tok").apiURL("issue/%s", "PROJ-1")
	if want := "https://acme.atlassian.net/rest/api/3/issue/PROJ-1"; v3 != want {
		t.Errorf("v3 apiURL = %q, want %q", v3, want)
	}
	trimmed := DataCenterPAT("https://jira.example.com/", "tok").apiURL("myself")
	if want := "https://jira.example.com/rest/api/2/myself"; trimmed != want {
		t.Errorf("trailing-slash apiURL = %q, want %q", trimmed, want)
	}
}

// TestDataCenterPATRequest exercises the full client path: Bearer auth header
// and a /rest/api/2/ URL on the wire.
func TestDataCenterPATRequest(t *testing.T) {
	srv, rec := captureServer(t, `{"key":"PROJ-1"}`)
	c := NewClient(DataCenterPAT(srv.URL, "tok-123"))

	if _, err := c.GetIssue("PROJ-1"); err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	auth, path := rec.read()
	if want := "Bearer tok-123"; auth != want {
		t.Errorf("Authorization = %q, want %q", auth, want)
	}
	if want := "/rest/api/2/issue/PROJ-1"; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

// TestCloudAPITokenRequest exercises the full client path: Basic auth header
// and a /rest/api/3/ URL on the wire.
func TestCloudAPITokenRequest(t *testing.T) {
	srv, rec := captureServer(t, `{"key":"PROJ-1"}`)
	c := NewClient(CloudAPIToken(srv.URL, "me@acme.com", "tok-123"))

	if _, err := c.GetIssue("PROJ-1"); err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	auth, path := rec.read()
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("me@acme.com:tok-123"))
	if auth != want {
		t.Errorf("Authorization = %q, want %q", auth, want)
	}
	if want := "/rest/api/3/issue/PROJ-1"; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

// TestNewAPIRequest checks the validator seam applies the same scheme +
// version a Client would, so auth.ValidateJira probes the right endpoint.
func TestNewAPIRequest(t *testing.T) {
	req, err := CloudAPIToken("https://acme.atlassian.net", "me@acme.com", "tok-123").
		NewAPIRequest(t.Context(), http.MethodGet, "myself", nil)
	if err != nil {
		t.Fatalf("NewAPIRequest: %v", err)
	}
	if want := "https://acme.atlassian.net/rest/api/3/myself"; req.URL.String() != want {
		t.Errorf("URL = %q, want %q", req.URL.String(), want)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("me@acme.com:tok-123"))
	if got := req.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}
