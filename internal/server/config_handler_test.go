package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestHandleConfig_LocalDefaults reports deployment_mode=local. The
// endpoint carries only the deployment-shape signal AuthGate needs to
// pick a login flow; per-user identity lives on /api/me.
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
}
