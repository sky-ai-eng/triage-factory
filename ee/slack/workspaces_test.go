package slack

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestHandlers_LocalMode404: every Slack route 404s in local mode before
// touching any dependency (entitlement, authz, store) — the Slack store is
// Postgres-only, so a locally-licensed request that got past this gate
// would otherwise hit db.ErrNotApplicableInLocal as a 500 instead of a
// clean 404. Both org gates check runmode first, so a zero-value handler
// (nil tx/az/client) is enough to prove it — no DB, no rig.
func TestHandlers_LocalMode404(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	h := &workspacesHandler{}
	const testOrg = "00000000-0000-0000-0000-000000000001"

	cases := []struct {
		name   string
		method string
		path   string
		fn     func(http.ResponseWriter, *http.Request)
	}{
		{"list", http.MethodPost, "/api/orgs/" + testOrg + "/slack/connections/list", h.handleList},
		{"get", http.MethodGet, "/api/orgs/" + testOrg + "/slack/connections/T123/A123", h.handleGet},
		{"import", http.MethodPost, "/api/orgs/" + testOrg + "/slack/connections/import", h.handleConnect},
		{"delete", http.MethodDelete, "/api/orgs/" + testOrg + "/slack/connections/T123/A123", h.handleDelete},
		{"manifest", http.MethodGet, "/api/orgs/" + testOrg + "/slack/manifest?transport=socket", h.handleManifest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.SetPathValue("org_id", testOrg)
			req.SetPathValue("workspace_id", "T123")
			req.SetPathValue("api_app_id", "A123")
			rec := httptest.NewRecorder()
			tc.fn(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s status = %d; want 404 in local mode", tc.name, rec.Code)
			}
		})
	}
}
