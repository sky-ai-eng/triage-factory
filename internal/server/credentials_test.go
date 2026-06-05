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

// TestIntegrationsStatus_SetupCompleteGate pins the mandatory-configuration
// gate the AuthGate keys on: a provisioned-but-unconfigured org is NOT
// setup_complete, and setup_step walks org → team → done as GitHub access
// and a tracked repo land. This is the server-authoritative signal that
// makes "I'll configure later" unreachable — the frontend can't fake it.
func TestIntegrationsStatus_SetupCompleteGate(t *testing.T) {
	keyring.MockInit()
	s := newTestServer(t)
	ctx := t.Context()
	org := runmode.LocalDefaultOrgID

	getStatus := func() map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/integrations/status", nil)
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status: got=%d want=200, body=%s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode status: %v", err)
		}
		return body
	}

	// Provisioned tenant, no GitHub creds, no repos → incomplete, resume at org.
	st := getStatus()
	if st["setup_complete"] != false {
		t.Errorf("setup_complete=%v on a fresh org; want false", st["setup_complete"])
	}
	if st["setup_step"] != "org" {
		t.Errorf("setup_step=%v with no GitHub; want org", st["setup_step"])
	}

	// GitHub configured (PAT) but still no tracked repo → resume at team.
	if err := integrations.Save(ctx, s.secrets, org, auth.Credentials{
		GitHubURL: "https://github.com",
		GitHubPAT: "ghp-test",
	}); err != nil {
		t.Fatalf("Save creds: %v", err)
	}
	st = getStatus()
	if st["github_ready"] != true {
		t.Errorf("github_ready=%v after PAT saved; want true", st["github_ready"])
	}
	if st["setup_complete"] != false {
		t.Errorf("setup_complete=%v with GitHub but no repos; want false", st["setup_complete"])
	}
	if st["setup_step"] != "team" {
		t.Errorf("setup_step=%v with GitHub and no repos; want team", st["setup_step"])
	}

	// A tracked repo lands → setup is complete, gate opens.
	seedConfiguredRepo(t, s, "acme", "widgets")
	st = getStatus()
	if st["setup_complete"] != true {
		t.Errorf("setup_complete=%v with GitHub + a repo; want true", st["setup_complete"])
	}
	if st["setup_step"] != "done" {
		t.Errorf("setup_step=%v when complete; want done", st["setup_step"])
	}
}
