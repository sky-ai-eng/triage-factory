package githubapp_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
)

// deploymentAppMinter builds the preflight's input off a real DeploymentApp, so
// what these tests exercise is the shape production hands it. It spells out
// what DeploymentApp.Minter does (TestDeploymentApp_Minter covers that method
// directly) because the test server needs its own HTTP client, which the
// deployment config has no business carrying.
func deploymentAppMinter(t *testing.T, apiBase string, client *http.Client) *githubapp.Minter {
	t.Helper()
	key, err := githubapp.ParsePrivateKey([]byte(testPEM(t)))
	if err != nil {
		t.Fatalf("ParsePrivateKey: %v", err)
	}
	app := githubapp.DeploymentApp{
		AppID:         424242,
		PrivateKey:    key,
		WebhookSecret: testWebhookSecret,
		ClientSecret:  testClientSecret,
	}
	m, err := githubapp.NewMinter(githubapp.Config{
		PrivateKey: app.PrivateKey,
		AppID:      app.AppID,
		APIBase:    apiBase,
		HTTPClient: client,
	})
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}
	return m
}

// appJSON is a GET /app body with the given members grant. An empty level omits
// the permission entirely, which is what an App that was never granted it looks
// like on the wire.
func appJSON(membersLevel string) string {
	perms := `"issues": "write", "pull_requests": "write", "contents": "write", "metadata": "read"`
	if membersLevel != "" {
		perms += `, "members": "` + membersLevel + `"`
	}
	return `{
		"id": 424242,
		"slug": "acme-triage-bot",
		"client_id": "Iv1.deadbeef",
		"owner": {"login": "acme-eng", "type": "Organization"},
		"permissions": {` + perms + `},
		"events": ["pull_request", "push"]
	}`
}

// TestPreflightDeploymentApp_DerivesIdentity: the slug, the client id and the
// owner are learned from GET /app rather than configured, which is the whole
// reason the deployment App is four env vars and not six.
func TestPreflightDeploymentApp_DerivesIdentity(t *testing.T) {
	for _, level := range []string{"read", "write", "admin"} {
		t.Run(level, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/app" {
					t.Errorf("path = %q, want /app", r.URL.Path)
				}
				_, _ = w.Write([]byte(appJSON(level)))
			}))
			defer srv.Close()

			id, err := githubapp.PreflightDeploymentApp(context.Background(), deploymentAppMinter(t, srv.URL, srv.Client()))
			if err != nil {
				t.Fatalf("PreflightDeploymentApp: %v", err)
			}
			if id.Slug != "acme-triage-bot" {
				t.Errorf("Slug = %q", id.Slug)
			}
			if id.ClientID != "Iv1.deadbeef" {
				t.Errorf("ClientID = %q", id.ClientID)
			}
			if id.OwnerLogin != "acme-eng" || id.OwnerType != "Organization" {
				t.Errorf("owner = %q/%q", id.OwnerLogin, id.OwnerType)
			}
			if id.AppID != 424242 {
				t.Errorf("AppID = %d", id.AppID)
			}
			if id.MembersPermission != level {
				t.Errorf("MembersPermission = %q, want %q", id.MembersPermission, level)
			}
		})
	}
}

// TestPreflightDeploymentApp_KeyRejected: GitHub answers 401 to a JWT whose iss
// does not match the signing key, so a 401 means the App ID and private key are
// not a pair — an operator message about .env, not about GitHub.
func TestPreflightDeploymentApp_KeyRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"A JSON web token could not be decoded"}`))
	}))
	defer srv.Close()

	_, err := githubapp.PreflightDeploymentApp(context.Background(), deploymentAppMinter(t, srv.URL, srv.Client()))
	if !errors.Is(err, githubapp.ErrDeploymentAppKeyRejected) {
		t.Fatalf("err = %v, want ErrDeploymentAppKeyRejected", err)
	}
	if errors.Is(err, githubapp.ErrDeploymentAppUnreachable) {
		t.Error("a 401 must not also read as unreachable; the two want opposite operator actions")
	}
	for _, name := range []string{"TF_GITHUB_APP_ID", "TF_GITHUB_APP_PRIVATE_KEY"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error does not point at %s: %v", name, err)
		}
	}
}

// TestPreflightDeploymentApp_Unreachable: a GitHub that does not answer — a
// dead connection, or any non-2xx that is not the 401 above — is a statement
// about GitHub, not a verdict on the configuration.
func TestPreflightDeploymentApp_Unreachable(t *testing.T) {
	t.Run("transport failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		client := srv.Client()
		base := srv.URL
		srv.Close() // nothing is listening now

		_, err := githubapp.PreflightDeploymentApp(context.Background(), deploymentAppMinter(t, base, client))
		if !errors.Is(err, githubapp.ErrDeploymentAppUnreachable) {
			t.Fatalf("err = %v, want ErrDeploymentAppUnreachable", err)
		}
	})

	t.Run("server error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))
		defer srv.Close()

		_, err := githubapp.PreflightDeploymentApp(context.Background(), deploymentAppMinter(t, srv.URL, srv.Client()))
		if !errors.Is(err, githubapp.ErrDeploymentAppUnreachable) {
			t.Fatalf("err = %v, want ErrDeploymentAppUnreachable", err)
		}
		if errors.Is(err, githubapp.ErrDeploymentAppKeyRejected) {
			t.Error("a 502 must not read as a bad key; the key is fine and GitHub is not")
		}
	})
}

// TestPreflightDeploymentApp_MembersPermission: members is the only
// organization permission the App requests, and both the installer restriction
// and the bind ceremony's authority gate hang off it — so its absence refuses
// rather than warns.
func TestPreflightDeploymentApp_MembersPermission(t *testing.T) {
	for name, level := range map[string]string{
		"absent":       "",
		"unrecognised": "none",
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(appJSON(level)))
			}))
			defer srv.Close()

			id, err := githubapp.PreflightDeploymentApp(context.Background(), deploymentAppMinter(t, srv.URL, srv.Client()))
			if !errors.Is(err, githubapp.ErrDeploymentAppMembersPermission) {
				t.Fatalf("err = %v, want ErrDeploymentAppMembersPermission", err)
			}
			if id != (githubapp.DeploymentAppIdentity{}) {
				t.Errorf("identity = %+v; a refused App must not return one", id)
			}
			if !strings.Contains(err.Error(), "Members") {
				t.Errorf("error does not name the permission to grant: %v", err)
			}
		})
	}
}

// TestPreflightDeploymentApp_NoMinter: the preflight is meaningless without the
// config it preflights, and says so rather than nil-panicking.
func TestPreflightDeploymentApp_NoMinter(t *testing.T) {
	if _, err := githubapp.PreflightDeploymentApp(context.Background(), nil); err == nil {
		t.Fatal("PreflightDeploymentApp(nil) returned no error")
	}
}
