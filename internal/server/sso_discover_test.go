package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestEmailDomain covers the exact-domain extraction the discovery endpoint
// routes on — no DB, so it runs everywhere (the HTTP+store path lives in
// sso_discover_pg_test.go behind the Docker-gated rig).
func TestEmailDomain(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{"plain", "user@corp.com", "corp.com", true},
		{"uppercased", "Alice@Corp.Com", "corp.com", true},
		{"surrounding whitespace", "  bob@corp.com\t", "corp.com", true},
		{"trailing fqdn dot", "x@corp.com.", "corp.com", true},
		{"multiple trailing dots", "x@corp.com..", "corp.com", true},
		// Exact-match is the whole point: a subdomain address yields the FULL
		// subdomain, never the registrable parent. eng.corp.com must not be
		// truncated to corp.com, or alice@eng.corp.com would wrongly route on a
		// bare corp.com claim.
		{"subdomain kept whole", "alice@eng.corp.com", "eng.corp.com", true},
		{"plus tag in local part", "user+tag@corp.com", "corp.com", true},

		{"empty", "", "", false},
		{"no at", "not-an-email", "", false},
		{"empty local part", "@corp.com", "", false},
		{"empty domain", "user@", "", false},
		{"trailing-dot-only domain", "user@.", "", false},
		{"two ats", "a@b@corp.com", "", false},
		{"interior whitespace in domain", "user@cor p.com", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := emailDomain(tc.in)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("emailDomain(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestSSOStartURL asserts the discovery start_url is exactly the SP-initiated
// SAML endpoint shape handleSAMLStart expects: provider_id + return_to, both
// encoded, keys sorted (provider_id before return_to).
func TestSSOStartURL(t *testing.T) {
	got := ssoStartURL("prov-123", "/dashboard")
	const want = "/api/auth/oauth/saml?provider_id=prov-123&return_to=%2Fdashboard"
	if got != want {
		t.Fatalf("ssoStartURL = %q, want %q", got, want)
	}

	// The pieces parse back out cleanly — the start endpoint reads provider_id
	// and return_to off the query.
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse start_url: %v", err)
	}
	if u.Path != "/api/auth/oauth/saml" {
		t.Errorf("path = %q, want /api/auth/oauth/saml", u.Path)
	}
	if pid := u.Query().Get("provider_id"); pid != "prov-123" {
		t.Errorf("provider_id = %q, want prov-123", pid)
	}
	if rt := u.Query().Get("return_to"); rt != "/dashboard" {
		t.Errorf("return_to = %q, want /dashboard", rt)
	}
}

// TestSSOStartURL_EncodesHostileProviderID guards against a provider_id (or a
// return_to that reached this far) injecting extra query params: url.Values
// percent-encodes '&' and '=', so a crafted value can't smuggle a second
// parameter into the emitted URL.
func TestSSOStartURL_EncodesHostileProviderID(t *testing.T) {
	got := ssoStartURL("abc&return_to=https://evil.example", "/")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if pid := u.Query().Get("provider_id"); pid != "abc&return_to=https://evil.example" {
		t.Errorf("provider_id = %q, want the literal value (no param split)", pid)
	}
	// return_to stays the value we passed — the hostile string is trapped inside
	// provider_id, not promoted to its own parameter.
	if rt := u.Query().Get("return_to"); rt != "/" {
		t.Errorf("return_to = %q, want / (hostile provider_id did not leak into it)", rt)
	}
}

// TestHandleSSODiscover_LocalMode_FallsToGitHub exercises the local-mode
// short-circuit without a DB: local mode answers {sso:false} before ever
// touching the store, so a zero-value Server (nil stores) is enough — and the
// fact that it doesn't panic is itself the proof the store is never reached.
func TestHandleSSODiscover_LocalMode_FallsToGitHub(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)

	req := httptest.NewRequest(http.MethodPost, "/api/sso/discover",
		bytes.NewReader([]byte(`{"email":"alice@corp.com","return_to":"/dashboard"}`)))
	rec := httptest.NewRecorder()
	(&Server{}).handleSSODiscover(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	var out ssoDiscoverResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.SSO || out.StartURL != "" {
		t.Errorf("local-mode response = %+v, want {sso:false} (no IdP in N=1)", out)
	}
}

// TestHandleSSODiscover_BadBody_400 asserts a non-JSON body is a clean 400
// (a malformed request, distinct from a well-formed request whose email just
// doesn't route — that's a 200 {sso:false}). No store needed: decode fails
// first, so a zero-value Server suffices.
func TestHandleSSODiscover_BadBody_400(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)

	req := httptest.NewRequest(http.MethodPost, "/api/sso/discover",
		bytes.NewReader([]byte(`{not json`)))
	rec := httptest.NewRecorder()
	(&Server{}).handleSSODiscover(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a malformed body (body=%q)", rec.Code, rec.Body.String())
	}
}
