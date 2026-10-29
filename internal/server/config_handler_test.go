package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestHandleConfig_LocalDefaults reports deployment_mode=local and the
// github.com default host. The endpoint carries only deployment facts a
// client needs before sign-in; per-user identity lives on /api/me.
func TestHandleConfig_LocalDefaults(t *testing.T) {
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodGet, "/api/config", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp configResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.DeploymentMode != string(runmode.ModeLocal) {
		t.Errorf("deployment_mode: got %q want %q", resp.DeploymentMode, runmode.ModeLocal)
	}
	if resp.DefaultGitHubHost != ghbase.GitHubCom {
		t.Errorf("default_github_host: got %q want %q", resp.DefaultGitHubHost, ghbase.GitHubCom)
	}
}

// TestHandleConfig_DefaultGitHubHost reports TF_DEFAULT_GITHUB_HOST's
// resolved value, which the login page names as the GitHub sign-in uses.
func TestHandleConfig_DefaultGitHubHost(t *testing.T) {
	ghbase.SetDefaultBaseURLForTest(t, "https://ghe.example.com")
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodGet, "/api/config", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp configResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.DefaultGitHubHost != "https://ghe.example.com" {
		t.Errorf("default_github_host: got %q want %q", resp.DefaultGitHubHost, "https://ghe.example.com")
	}
}
