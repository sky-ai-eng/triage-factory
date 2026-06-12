package githubapp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
)

// TestGetApp_HappyPath pins the App-JWT auth, the GET /app request shape, and
// the parse of GitHub's app metadata into the App struct — including the owner
// login + verbatim account type the import flow derives owner_type from.
func TestGetApp_HappyPath(t *testing.T) {
	key := newTestKey(t)
	const appID int64 = 424242

	var (
		seenMethod string
		seenPath   string
		seenAuth   string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenMethod = r.Method
		seenPath = r.URL.Path
		seenAuth = r.Header.Get("Authorization")

		bearer := strings.TrimPrefix(seenAuth, "Bearer ")
		if _, err := jwt.Parse(bearer, func(*jwt.Token) (interface{}, error) {
			return &key.PublicKey, nil
		}, jwt.WithValidMethods([]string{"RS256"})); err != nil {
			http.Error(w, "bad jwt", http.StatusUnauthorized)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id": 424242,
			"slug": "acme-triage-bot",
			"client_id": "Iv1.deadbeef",
			"owner": {"login": "acme-eng", "type": "Organization"},
			"permissions": {"issues": "write", "pull_requests": "write", "contents": "read", "metadata": "read"},
			"events": ["pull_request", "push"]
		}`))
	}))
	defer srv.Close()

	m, err := githubapp.NewMinter(githubapp.Config{
		PrivateKey: key,
		AppID:      appID,
		APIBase:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}

	app, err := m.GetApp(context.Background())
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}

	if seenMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", seenMethod)
	}
	if seenPath != "/app" {
		t.Errorf("path = %q, want /app", seenPath)
	}
	if !strings.HasPrefix(seenAuth, "Bearer ") {
		t.Errorf("authorization = %q; want Bearer <jwt>", seenAuth)
	}
	if app.ID != 424242 {
		t.Errorf("id = %d, want 424242", app.ID)
	}
	if app.Slug != "acme-triage-bot" {
		t.Errorf("slug = %q, want acme-triage-bot", app.Slug)
	}
	if app.ClientID != "Iv1.deadbeef" {
		t.Errorf("client_id = %q, want Iv1.deadbeef", app.ClientID)
	}
	if app.OwnerLogin != "acme-eng" {
		t.Errorf("owner login = %q, want acme-eng", app.OwnerLogin)
	}
	if app.OwnerType != "Organization" {
		t.Errorf("owner type = %q, want Organization (verbatim)", app.OwnerType)
	}
	if app.Permissions["issues"] != "write" || app.Permissions["metadata"] != "read" {
		t.Errorf("permissions = %v, want issues:write metadata:read", app.Permissions)
	}
	if len(app.Events) != 2 || app.Events[0] != "pull_request" {
		t.Errorf("events = %v, want [pull_request push]", app.Events)
	}
}

// TestGetApp_AuthFailure pins that a 401 — what GitHub returns when the
// submitted App ID and private key don't match (a JWT whose iss doesn't match
// the signing key) — surfaces as an error rather than a zero-value App, so the
// import endpoint can map it to its 422 "ID and key don't match" response.
func TestGetApp_AuthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"A JSON web token could not be decoded"}`))
	}))
	defer srv.Close()

	m, err := githubapp.NewMinter(githubapp.Config{
		PrivateKey: newTestKey(t),
		AppID:      1,
		APIBase:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}

	if _, err := m.GetApp(context.Background()); err == nil {
		t.Fatal("GetApp on 401 = nil err; want error")
	}
}
