package server

import (
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
	"github.com/zalando/go-keyring"
)

// TestGitHubPreflightSSH_AnswersThePathOrg: the probe reads the GitHub URL of
// the org its path names, so an org the caller cannot reach is 404, and the
// caller's own org with no GitHub URL is refused before ssh runs.
func TestGitHubPreflightSSH_AnswersThePathOrg(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/00000000-0000-4000-8000-000000000001/github/ssh/preflight", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("another org: status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}

	if rec := patchOrgSettings(t, s, map[string]any{"github_base_url": nil}); rec.Code != http.StatusOK {
		t.Fatalf("clear github_base_url = %d, body=%s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/ssh/preflight", nil)
	httpxtest.AssertFirstError(t, rec, httpx.ReasonNotConfigured, "")
}
