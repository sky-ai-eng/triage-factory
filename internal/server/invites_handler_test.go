package server

import (
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestInviteRoutes_LocalAreNotFound: org invites are multi-mode only, so every
// invite route 404s in local mode.
func TestInviteRoutes_LocalAreNotFound(t *testing.T) {
	s := newTestServer(t)
	localInvites := "/api/orgs/" + runmode.LocalDefaultOrgID + "/invites"

	cases := []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, localInvites, map[string]string{"email": "x@y.com", "role": "member"}},
		{http.MethodPost, localInvites + "/list", map[string]any{}},
		{http.MethodGet, localInvites + "/11111111-1111-1111-1111-111111111111", nil},
		{http.MethodPost, localInvites + "/11111111-1111-1111-1111-111111111111/revoke", nil},
		{http.MethodGet, "/api/invites/preview?token=abc", nil},
		{http.MethodPost, "/api/invites/accept", map[string]string{"token": "abc"}},
	}
	for _, tc := range cases {
		rec := doJSON(t, s, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: status = %d, want 404 (multi-mode only); body=%s",
				tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}
