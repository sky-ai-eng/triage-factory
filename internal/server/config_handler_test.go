package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestHandleConfig: /api/config answers without a session and reports the
// deployment mode and the resolved TF_DEFAULT_GITHUB_HOST, the facts a client
// needs before sign-in.
func TestHandleConfig(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mode        runmode.Mode
		defaultHost string
		wantHost    string
	}{
		{"local defaults", runmode.ModeLocal, "", ghbase.GitHubCom},
		{"default github host", runmode.ModeLocal, "https://ghe.example.com", "https://ghe.example.com"},
		{"multi mode, unauthenticated", runmode.ModeMulti, "", ghbase.GitHubCom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runmode.SetForTest(t, tc.mode)
			ghbase.SetDefaultBaseURLForTest(t, tc.defaultHost)
			s := newTestServer(t)

			rec := doJSON(t, s, http.MethodGet, "/api/config", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
			}
			var resp configResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp.DeploymentMode != string(tc.mode) {
				t.Errorf("deployment_mode: got %q want %q", resp.DeploymentMode, tc.mode)
			}
			if resp.DefaultGitHubHost != tc.wantHost {
				t.Errorf("default_github_host: got %q want %q", resp.DefaultGitHubHost, tc.wantHost)
			}
		})
	}
}
