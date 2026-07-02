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
// clean 404. memberGate checks runmode first, so a zero-value handler
// (nil tx/az/client) is enough to prove it — no DB, no rig.
func TestHandlers_LocalMode404(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	h := &workspacesHandler{}

	cases := []struct {
		name   string
		method string
		path   string
		fn     func(http.ResponseWriter, *http.Request)
	}{
		{"list", http.MethodGet, "/api/slack/workspaces", h.handleList},
		{"connect", http.MethodPost, "/api/slack/workspaces", h.handleConnect},
		{"delete", http.MethodDelete, "/api/slack/workspaces/T123", h.handleDelete},
		{"manifest", http.MethodGet, "/api/slack/manifest?transport=socket", h.handleManifest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.name == "delete" {
				req.SetPathValue("workspace_id", "T123")
			}
			rec := httptest.NewRecorder()
			tc.fn(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s status = %d; want 404 in local mode", tc.name, rec.Code)
			}
		})
	}
}
