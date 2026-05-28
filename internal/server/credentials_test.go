package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestHandleIntegrationsClear_SurfacesEnvOverlayWarning pins the
// load-bearing UX from the SecretStore sweep: when env vars supply a
// credential the DELETE handler reports success on the keychain layer
// but tells the user the env still surfaces the value. Without this
// the disconnect button looks like a silent no-op.
func TestHandleIntegrationsClear_SurfacesEnvOverlayWarning(t *testing.T) {
	keyring.MockInit()
	s := newTestServer(t)
	ctx := t.Context()
	org := runmode.LocalDefaultOrgID

	// Seed keychain creds, then set env vars to provoke the warning.
	if err := integrations.Save(ctx, s.secrets, org, auth.Credentials{
		GitHubURL: "https://github.example.com",
		GitHubPAT: "ghp-test",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	t.Setenv("TRIAGE_FACTORY_GITHUB_URL", "https://env.example.com")
	t.Setenv("TRIAGE_FACTORY_GITHUB_PAT", "env-pat")

	req := httptest.NewRequest(http.MethodDelete, "/api/integrations", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got=%d want=200, body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "cleared" {
		t.Errorf("status field got=%v want=cleared", body["status"])
	}
	if _, ok := body["warning"]; !ok {
		t.Errorf("expected warning field surfaced for env-overlay case, body=%+v", body)
	}
	envs, _ := body["env_provided"].([]any)
	found := false
	for _, e := range envs {
		if e == "github" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("env_provided should include github, got=%v", body["env_provided"])
	}
}

// TestHandleIntegrationsClear_NoWarningWithoutEnv confirms the warning
// only fires when env vars are actually present — the happy-path
// clear response stays minimal.
func TestHandleIntegrationsClear_NoWarningWithoutEnv(t *testing.T) {
	keyring.MockInit()
	s := newTestServer(t)
	ctx := t.Context()
	org := runmode.LocalDefaultOrgID

	if err := integrations.Save(ctx, s.secrets, org, auth.Credentials{
		GitHubURL: "https://github.example.com",
		GitHubPAT: "ghp-test",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/integrations", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got=%d want=200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["warning"]; ok {
		t.Errorf("unexpected warning field on env-free clear: %+v", body)
	}
}

// TestHandleIntegrationsDeleteJira_SurfacesJiraOnlyWarning pins the
// targeted Jira clear handler's env-overlay behavior: a GitHub env
// overlay shouldn't trigger a warning on a Jira-only clear.
func TestHandleIntegrationsDeleteJira_SurfacesJiraOnlyWarning(t *testing.T) {
	keyring.MockInit()
	s := newTestServer(t)
	ctx := t.Context()
	org := runmode.LocalDefaultOrgID

	if err := integrations.Save(ctx, s.secrets, org, auth.Credentials{
		JiraURL: "https://jira.example.com",
		JiraPAT: "jira-test",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	t.Setenv("TRIAGE_FACTORY_JIRA_URL", "https://env.example.com")
	t.Setenv("TRIAGE_FACTORY_JIRA_PAT", "env-jira-pat")

	req := httptest.NewRequest(http.MethodDelete, "/api/integrations/jira", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got=%d want=200, body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["warning"]; !ok {
		t.Errorf("expected warning on env-overlay Jira clear, body=%+v", body)
	}
}
