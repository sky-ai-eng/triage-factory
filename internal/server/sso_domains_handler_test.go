package server

import (
	"net/http"
	"testing"
)

// TestSSODomainRoutes_LocalAreNotFound: SSO domain claim/verification is a
// multi-org concept. In local mode (N=1) every route 404s — the "feature
// absent" posture matching the invite routes and teams_handler's hosted-only
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

// TestNormalizeSSODomain covers the claim-time normalizer in isolation: lower
// + trim + trailing-dot strip on the accepted side, and the paste-guard
// rejections (empty, email, URL, whitespace, single label) on the other.
func TestNormalizeSSODomain(t *testing.T) {
	ok := []struct{ in, want string }{
		{"corp.com", "corp.com"},
		{"  Corp.Com  ", "corp.com"},
		{"EXAMPLE.CO.UK", "example.co.uk"},
		{"corp.com.", "corp.com"},  // single trailing FQDN dot stripped
		{"corp.com..", "corp.com"}, // all trailing dots stripped (not just one)
		{"eng.corp.com", "eng.corp.com"},
	}
	for _, tc := range ok {
		got, valid := normalizeSSODomain(tc.in)
		if !valid || got != tc.want {
			t.Errorf("normalizeSSODomain(%q) = (%q, %v); want (%q, true)", tc.in, got, valid, tc.want)
		}
	}

	bad := []string{
		"",                 // empty
		"   ",              // whitespace only
		"localhost",        // single label, no dot
		"user@corp.com",    // pasted email
		"https://corp.com", // URL (scheme + slashes)
		"corp.com/path",    // path
		"corp .com",        // embedded space
		"corp\tcom.com",    // embedded tab
		".",                // dot(s) only → empty after strip
		"..",               // dot(s) only → empty after strip
		".corp.com",        // leading dot → empty first label
		"corp..com",        // doubled internal dot → empty middle label
	}
	for _, in := range bad {
		if got, valid := normalizeSSODomain(in); valid {
			t.Errorf("normalizeSSODomain(%q) = (%q, true); want rejected", in, got)
		}
	}
}
