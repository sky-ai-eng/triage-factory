package server

import (
	"net/http"
	"testing"
)

// TestInviteRoutes_LocalAreNotFound: org invites are a multi-mode concept.
// In local mode (N=1) every invite route 404s — the "feature absent"
// posture matching teams_handler's multi-only create. This is the guard
// the ticket calls for (runmode.ModeLocal → http.NotFound on every route).
func TestInviteRoutes_LocalAreNotFound(t *testing.T) {
	s := newTestServer(t)

	cases := []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/api/invites", map[string]string{"email": "x@y.com", "role": "member"}},
		{http.MethodPost, "/api/invites/list", map[string]any{}},
		{http.MethodGet, "/api/invites/" + "11111111-1111-1111-1111-111111111111", nil},
		{http.MethodPost, "/api/invites/" + "11111111-1111-1111-1111-111111111111" + "/revoke", nil},
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
