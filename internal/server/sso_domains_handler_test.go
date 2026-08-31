package server

import (
	"net/http"
	"testing"
)

// TestSSODomainRoutes_LocalAreNotFound: SSO domain claim/verification is a
// multi-org concept. In local mode (N=1) every route 404s — the "feature
// absent" posture matching the invite routes and teams_handler's multi-only
// create (runmode.ModeLocal → http.NotFound in the shared gate).
func TestSSODomainRoutes_LocalAreNotFound(t *testing.T) {
	s := newTestServer(t)

	const fakeID = "11111111-1111-1111-1111-111111111111"
	cases := []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/sso/domains", nil},
		{http.MethodPost, "/api/sso/domains", map[string]string{"domain": "corp.com"}},
		{http.MethodPost, "/api/sso/domains/" + fakeID + "/verify", nil},
		{http.MethodDelete, "/api/sso/domains/" + fakeID, nil},
	}
	for _, tc := range cases {
		rec := doJSON(t, s, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: status = %d, want 404 (multi-mode only); body=%s",
				tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

// TestSSOTestStart_NoAuthStack_404: SSO is an ee feature gated on the `sso`
// entitlement; a local-mode (community) server mounts no SSO routes, so the
// verify-test-start route 404s — the "feature absent" contract observed at the
// route boundary. (Lives in core: it asserts core's local-mode posture via the
// package-server local test harness, with no ee extension registered.)
func TestSSOTestStart_NoAuthStack_404(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodGet, "/api/sso/connection/test", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d, want 404 when SSO/auth stack is unavailable (local mode)", rec.Code)
	}
}

// TestNormalizeSSODomain moved to ee/sso/funcs_test.go with the normalizer.
