package slack

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// muxMounter mounts the route table on a plain mux, with every handler
// replaced by one that answers 200, so a request shows only whether a route
// matched it.
type muxMounter struct{ mux *http.ServeMux }

func (m muxMounter) matched(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

func (m muxMounter) API(pattern string, _ http.HandlerFunc) { m.mux.HandleFunc(pattern, m.matched) }

func (m muxMounter) Raw(pattern string, _ http.Handler) { m.mux.HandleFunc(pattern, m.matched) }

func (m muxMounter) Op(op httpx.Operation, _ http.HandlerFunc) {
	m.mux.HandleFunc(op.Spec().Pattern, m.matched)
}

func (m muxMounter) RawOp(op httpx.Operation, _ http.Handler) {
	m.mux.HandleFunc(op.Spec().Pattern, m.matched)
}

func (m muxMounter) SignedWebhookRateLimit(h http.Handler) http.Handler { return h }

// TestRoutes_OldPathsAreGone: the Slack routes that were addressed by the
// session's org, or that lived under /api/slack/ for a team, answer 404, and
// the paths that replace them are served.
func TestRoutes_OldPathsAreGone(t *testing.T) {
	m := muxMounter{mux: http.NewServeMux()}
	mountRoutes(m, &workspacesHandler{}, &deploymentAppHandler{}, &installHandler{}, &channelsHandler{}, &webhookHandler{})

	const org = "00000000-0000-0000-0000-000000000001"
	gone := []struct{ method, path string }{
		{http.MethodGet, "/api/slack/workspaces"},
		{http.MethodPost, "/api/slack/workspaces"},
		{http.MethodDelete, "/api/slack/workspaces/T1/A1"},
		{http.MethodGet, "/api/slack/manifest?transport=socket"},
		{http.MethodPut, "/api/slack/teams/team1/channels"},
		{http.MethodPost, "/api/slack/teams/team1/channels/list"},
		{http.MethodPost, "/api/slack/teams/team1/channels/facets"},
		{http.MethodPost, "/api/slack/channels/C1/primary"},
	}
	for _, tc := range gone {
		rec := httptest.NewRecorder()
		m.mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d; want 404", tc.method, tc.path, rec.Code)
		}
	}

	served := []struct{ method, path string }{
		{http.MethodGet, "/api/orgs/" + org + "/slack/manifest?transport=socket"},
		{http.MethodPost, "/api/orgs/" + org + "/slack/connections/import"},
		{http.MethodPost, "/api/orgs/" + org + "/slack/connections/list"},
		{http.MethodGet, "/api/orgs/" + org + "/slack/connections/T1/A1"},
		{http.MethodDelete, "/api/orgs/" + org + "/slack/connections/T1/A1"},
		{http.MethodGet, "/api/orgs/" + org + "/slack/connections/T1/A1/activation-preview"},
		{http.MethodPost, "/api/orgs/" + org + "/slack/connections/T1/A1/activate"},
		{http.MethodGet, "/api/slack/deployment-app"},
		{http.MethodGet, "/api/orgs/" + org + "/slack/install/start"},
		{http.MethodGet, "/api/slack/install/callback"},
		{http.MethodPut, "/api/teams/team1/slack-channels"},
		{http.MethodPost, "/api/teams/team1/slack-channels/list"},
		{http.MethodPost, "/api/teams/team1/slack-channels/facets"},
		{http.MethodGet, "/api/teams/team1/slack-channels/C1"},
		{http.MethodPatch, "/api/teams/team1/slack-channels/C1"},
		{http.MethodPost, "/api/webhooks/slack/" + org},
		{http.MethodPost, "/api/webhooks/slack"},
	}
	for _, tc := range served {
		rec := httptest.NewRecorder()
		m.mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s %s = %d; want it served", tc.method, tc.path, rec.Code)
		}
	}
}
