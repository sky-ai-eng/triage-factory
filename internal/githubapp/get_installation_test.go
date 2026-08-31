package githubapp_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
)

// TestGetInstallation_HappyPath pins the App-JWT auth, the request shape, and
// the parse of one /app/installations/{id} object — the read the bind ceremony
// takes its facts from, rather than from the user-token association listing it
// uses only as a gate.
func TestGetInstallation_HappyPath(t *testing.T) {
	key := newTestKey(t)

	var seenMethod, seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenMethod, seenPath = r.Method, r.URL.Path

		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if _, err := jwt.Parse(bearer, func(*jwt.Token) (interface{}, error) {
			return &key.PublicKey, nil
		}, jwt.WithValidMethods([]string{"RS256"})); err != nil {
			http.Error(w, fmt.Sprintf("bad jwt: %v", err), http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{
			"id": 4242,
			"account": {"id": 700, "login": "acme-eng", "type": "Organization"},
			"repository_selection": "selected",
			"created_at": "2026-01-02T03:04:05Z"
		}`))
	}))
	defer srv.Close()

	m, err := githubapp.NewMinter(githubapp.Config{
		PrivateKey: key, AppID: 424242, APIBase: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}

	inst, err := m.GetInstallation(context.Background(), 4242)
	if err != nil {
		t.Fatalf("GetInstallation: %v", err)
	}
	if seenMethod != http.MethodGet || seenPath != "/app/installations/4242" {
		t.Errorf("request = %s %s, want GET /app/installations/4242", seenMethod, seenPath)
	}
	if inst.ID != 4242 || inst.AccountID != 700 || inst.AccountLogin != "acme-eng" || inst.AccountType != "Organization" {
		t.Errorf("installation = %+v, want id 4242 on Organization acme-eng (700)", inst)
	}
	if inst.RepositorySelection != "selected" {
		t.Errorf("RepositorySelection = %q, want \"selected\"", inst.RepositorySelection)
	}
}

// TestGetInstallation_NotFoundIsAStatusError pins the shape a spoofed
// installation_id produces. The caller has to tell "no such installation for
// this App" from "GitHub did not answer" — both refuse a bind, but only one of
// them is worth retrying.
func TestGetInstallation_NotFoundIsAStatusError(t *testing.T) {
	key := newTestKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()

	m, err := githubapp.NewMinter(githubapp.Config{
		PrivateKey: key, AppID: 424242, APIBase: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}

	_, err = m.GetInstallation(context.Background(), 999)
	var status *githubapp.APIStatusError
	if !errors.As(err, &status) {
		t.Fatalf("GetInstallation error = %v, want an *APIStatusError", err)
	}
	if status.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want 404", status.StatusCode)
	}
}
