package auth

import (
	"context"
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
