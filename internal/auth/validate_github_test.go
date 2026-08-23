package auth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestCaptureGitHubIdentity(t *testing.T) {
	t.Parallel()

	var (
		calls   []string
		callsMu sync.Mutex
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsMu.Lock()
		calls = append(calls, r.URL.Path)
		callsMu.Unlock()
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q, want Bearer secret", got)
		}
		switch r.URL.Path {
		case "/api/v3/user":
			_, _ = w.Write([]byte(`{"id":583231,"login":"octocat","email":"public@example.com"}`))
		case "/api/v3/user/emails":
			_, _ = w.Write([]byte(`[
				{"email":"other@example.com","primary":false,"verified":true},
				{"email":"octocat@example.com","primary":true,"verified":true}
			]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	got, err := CaptureGitHubIdentity(context.Background(), srv.URL, "secret")
	if err != nil {
		t.Fatalf("CaptureGitHubIdentity: %v", err)
	}
	if got.Login != "octocat" || got.UserID() != "583231" || got.PrimaryEmail != "octocat@example.com" {
		t.Fatalf("identity = %#v, want octocat/583231/octocat@example.com", got)
	}
	callsMu.Lock()
	gotCalls := strings.Join(calls, ",")
	callsSnapshot := append([]string(nil), calls...)
	callsMu.Unlock()
	if gotCalls != "/api/v3/user,/api/v3/user/emails" {
		t.Fatalf("calls = %v", callsSnapshot)
	}
}

func TestCaptureGitHubIdentityRequiresEmailPermission(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/user" {
			_, _ = w.Write([]byte(`{"id":583231,"login":"octocat"}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	_, err := CaptureGitHubIdentity(context.Background(), srv.URL, "secret")
	if err == nil || !strings.Contains(err.Error(), "user:email") {
		t.Fatalf("error = %v, want user:email guidance", err)
	}
}

func TestCaptureGitHubIdentityRequiresVerifiedPrimaryEmail(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/user" {
			_, _ = w.Write([]byte(`{"id":583231,"login":"octocat"}`))
			return
		}
		_, _ = w.Write([]byte(`[{"email":"octocat@example.com","primary":true,"verified":false}]`))
	}))
	t.Cleanup(srv.Close)

	_, err := CaptureGitHubIdentity(context.Background(), srv.URL, "secret")
	if err == nil || !strings.Contains(err.Error(), "no verified primary email") {
		t.Fatalf("error = %v, want verified-primary-email failure", err)
	}
}

// stubGitHubAPI points the package's shared client at a transport that answers
// the two account endpoints and records the URLs they were asked for. It runs
// without t.Parallel deliberately: the transport is package-global, and the
// parallel tests above resume only after every sequential test has finished.
func stubGitHubAPI(t *testing.T, seen *[]string) {
	t.Helper()
	prev := httpClient.Transport
	httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		*seen = append(*seen, r.URL.String())
		body := `{"id":583231,"login":"octocat"}`
		if strings.HasSuffix(r.URL.Path, "/user/emails") {
			body = `[{"email":"octocat@example.com","primary":true,"verified":true}]`
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	})
	t.Cleanup(func() { httpClient.Transport = prev })
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestCaptureGitHubIdentityAPIMount pins where the credential probe looks for
// each host class TF supports. The *.ghe.com row carries the weight: Enterprise
// Cloud data residency mounts the API on an api.* subdomain rather than the
// GHES /api/v3 path, so a probe that reasons "github.com, else {base}/api/v3"
// rejects a token those tenants can poll with — which is the whole reason this
// derivation lives in one place instead of at each call site.
func TestCaptureGitHubIdentityAPIMount(t *testing.T) {
	for _, tt := range []struct {
		name, base, wantUser, wantEmails string
	}{
		{"public", "https://github.com", "https://api.github.com/user", "https://api.github.com/user/emails"},
		{"unconfigured", "", "https://api.github.com/user", "https://api.github.com/user/emails"},
		{"ghes", "https://ghes.acme.com", "https://ghes.acme.com/api/v3/user", "https://ghes.acme.com/api/v3/user/emails"},
		{"data_residency", "https://octocorp.ghe.com", "https://api.octocorp.ghe.com/user", "https://api.octocorp.ghe.com/user/emails"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var seen []string
			stubGitHubAPI(t, &seen)

			if _, err := CaptureGitHubIdentity(context.Background(), tt.base, "secret"); err != nil {
				t.Fatalf("CaptureGitHubIdentity: %v", err)
			}
			want := []string{tt.wantUser, tt.wantEmails}
			if strings.Join(seen, ",") != strings.Join(want, ",") {
				t.Errorf("probed %v, want %v", seen, want)
			}
		})
	}
}
