package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
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

// TestJiraApp_LocalMode_ImportFlipsConnectAvailable: storing a BYO app makes
// connect_available true and surfaces the override's client_id — including on
// the per-user Jira identity status endpoint, the signal the frontend reads to
// offer the one-click Connect button.
func TestJiraApp_LocalMode_ImportFlipsConnectAvailable(t *testing.T) {
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
	if out.App == nil || out.App.ClientID != "atl-client-xyz" {
		t.Fatalf("import app=%+v, want client_id atl-client-xyz", out.App)
	}
	if !out.ConnectAvailable {
		t.Error("connect_available=false after import, want true")
	}

	// The per-user Jira status endpoint must now advertise connect_available.
	rec = doJSON(t, s, "GET", "/api/orgs/"+runmode.LocalDefaultOrgID+"/jira/identity", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("identity status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var ident jiraIdentityStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&ident); err != nil {
		t.Fatalf("decode identity: %v", err)
	}
	if !ident.ConnectAvailable {
		t.Error("identity connect_available=false after import, want true")
	}

	// The plaintext secret must NOT be stored on the app row — only a ref.
	// Read it back through the secret store to confirm it round-trips under the
	// fixed key (and that the app table holds the ref, not the value).
	got, err := s.secrets.GetSystem(context.Background(), runmode.LocalDefaultOrgID, jiraOAuthClientSecretKey)
	if err != nil {
		t.Fatalf("read stored secret: %v", err)
	}
	if got != "shhh-secret" {
		t.Errorf("stored secret=%q, want shhh-secret", got)
	}
}

// TestJiraApp_LocalMode_DeleteClearsConnectAvailable: removing the BYO app
// reverts connect_available to false and clears the stored secret.
func TestJiraApp_LocalMode_DeleteClearsConnectAvailable(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	if rec := doJSON(t, s, "POST", jiraAppPath(), map[string]string{
		"client_id":     "atl-client-xyz",
		"client_secret": "shhh-secret",
	}); rec.Code != http.StatusOK {
		t.Fatalf("import status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}

	rec := doJSON(t, s, "DELETE", jiraAppPath(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var out jiraAppStatusResponse
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
// has gone missing must NOT read as a configured override — the status summary
// is driven by what the resolver actually resolves, not the raw row. In local
// mode (no deployment app), the dead row resolves to nothing: app:null,
// connect_available=false.
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

// TestJiraApp_ImportRejectsMissingFields: both client_id and client_secret are
// required; a blank one is a 400 MISSING_FIELD naming the field — the epic's
// line is that a missing required field is shape-level, so it is 400 here as
// it already was on the identity-PAT and jira-credential siblings.
func TestJiraApp_ImportRejectsMissingFields(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	rec := doJSON(t, s, "POST", jiraAppPath(), map[string]string{"client_secret": "x"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	assertFirstError(t, rec, httpx.ReasonMissingField, "client_id")
}
