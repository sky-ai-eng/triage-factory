package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

// jiraAppPath is the org-scoped jira/app endpoint for the local sentinel org.
func jiraAppPath() string {
	return "/api/orgs/" + runmode.LocalDefaultOrgID + "/jira/app"
}

// TestJiraApp_LocalMode_StatusEmpty: with no per-org app and no deployment
// app (local has none), status reports app:null and connect_available=false.
func TestJiraApp_LocalMode_StatusEmpty(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	rec := doJSON(t, s, "GET", jiraAppPath(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var out jiraAppStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.App != nil {
		t.Errorf("app=%+v, want nil", out.App)
	}
	if out.ConnectAvailable {
		t.Error("connect_available=true, want false with no app configured")
	}
	if out.UsingDeploymentDefault {
		t.Error("using_deployment_default=true, want false in local mode")
	}
}

// TestJiraApp_LocalMode_DeleteClearsConnectAvailable: storing a BYO app makes
// connect_available true and keeps its secret in the secret store; removing it
// reverts connect_available to false and clears the secret.
func TestJiraApp_LocalMode_DeleteClearsConnectAvailable(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	rec := doJSON(t, s, "POST", jiraAppPath(), map[string]string{
		"client_id":     "atl-client-xyz",
		"client_secret": "shhh-secret",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("import status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var out jiraAppStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode import: %v", err)
	}
	if out.App == nil || out.App.ClientID != "atl-client-xyz" || !out.ConnectAvailable {
		t.Fatalf("import = %+v, want client_id atl-client-xyz and connect_available", out)
	}
	if got, err := s.secrets.GetSystem(context.Background(), runmode.LocalDefaultOrgID, jiraOAuthClientSecretKey); err != nil || got != "shhh-secret" {
		t.Errorf("stored secret = (%q, %v), want shhh-secret", got, err)
	}

	rec = doJSON(t, s, "DELETE", jiraAppPath(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	out = jiraAppStatusResponse{}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode delete: %v", err)
	}
	if out.App != nil {
		t.Errorf("app=%+v after delete, want nil", out.App)
	}
	if out.ConnectAvailable {
		t.Error("connect_available=true after delete, want false")
	}

	// The secret is gone too.
	got, err := s.secrets.GetSystem(context.Background(), runmode.LocalDefaultOrgID, jiraOAuthClientSecretKey)
	if err != nil {
		t.Fatalf("read stored secret: %v", err)
	}
	if got != "" {
		t.Errorf("stored secret=%q after delete, want empty", got)
	}
}

// TestJiraApp_DeadOverrideRowNotShownAsConfigured: a per-org row whose secret
// has gone missing does not read as a configured override, because the status
// follows what the resolver resolves, not the raw row.
func TestJiraApp_DeadOverrideRowNotShownAsConfigured(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	if rec := doJSON(t, s, "POST", jiraAppPath(), map[string]string{
		"client_id":     "atl-client-xyz",
		"client_secret": "shhh-secret",
	}); rec.Code != http.StatusOK {
		t.Fatalf("import status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}

	// Drop the secret out from under the row (the row persists). Mirrors the
	// edge the resolver documents: a secret deleted without the row.
	if _, err := s.secrets.Delete(context.Background(), runmode.LocalDefaultOrgID, jiraOAuthClientSecretKey); err != nil {
		t.Fatalf("delete secret: %v", err)
	}

	rec := doJSON(t, s, "GET", jiraAppPath(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var out jiraAppStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.App != nil {
		t.Errorf("app=%+v, want nil (dead override must not read as configured)", out.App)
	}
	if out.ConnectAvailable {
		t.Error("connect_available=true, want false (dead override, no deployment app in local)")
	}
	if out.UsingDeploymentDefault {
		t.Error("using_deployment_default=true, want false (no deployment app in local)")
	}
}

// TestJiraApp_ImportRejectsMissingFields: client_id and client_secret are both
// required, and a blank one is a 400 MISSING_FIELD naming the field.
func TestJiraApp_ImportRejectsMissingFields(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	rec := doJSON(t, s, "POST", jiraAppPath(), map[string]string{"client_secret": "x"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	httpxtest.AssertFirstError(t, rec, httpx.ReasonMissingField, "client_id")
}
