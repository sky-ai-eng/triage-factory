package sso

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestNormalizeSSODomain covers the claim-time normalizer in isolation: lower
// + trim + trailing-dot strip on the accepted side, and the paste-guard
// rejections (empty, email, URL, whitespace, single label) on the other.
// Moved verbatim from internal/server/sso_domains_handler_test.go.
func TestNormalizeSSODomain(t *testing.T) {
	ok := []struct{ in, want string }{
		{"corp.com", "corp.com"},
		{"  Corp.Com  ", "corp.com"},
		{"EXAMPLE.CO.UK", "example.co.uk"},
		{"corp.com.", "corp.com"},
		{"corp.com..", "corp.com"},
		{"eng.corp.com", "eng.corp.com"},
	}
	for _, tc := range ok {
		got, valid := normalizeSSODomain(tc.in)
		if !valid || got != tc.want {
			t.Errorf("normalizeSSODomain(%q) = (%q, %v); want (%q, true)", tc.in, got, valid, tc.want)
		}
	}

	bad := []string{
		"", "   ", "localhost", "user@corp.com", "https://corp.com",
		"corp.com/path", "corp .com", "corp\tcom.com", ".", "..",
		".corp.com", "corp..com",
	}
	for _, in := range bad {
		if got, valid := normalizeSSODomain(in); valid {
			t.Errorf("normalizeSSODomain(%q) = (%q, true); want rejected", in, got)
		}
	}
}

// TestSSOStartURL asserts the discovery start_url is exactly the SP-initiated
// SAML endpoint shape StartSSO expects: provider_id + return_to, both encoded,
// keys sorted.
func TestSSOStartURL(t *testing.T) {
	got := ssoStartURL("prov-123", "/dashboard")
	const want = "/api/auth/oauth/saml?provider_id=prov-123&return_to=%2Fdashboard"
	if got != want {
		t.Fatalf("ssoStartURL = %q, want %q", got, want)
	}
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

// TestSSOStartURL_EncodesHostileProviderID guards against a provider_id
// injecting extra query params.
func TestSSOStartURL_EncodesHostileProviderID(t *testing.T) {
	got := ssoStartURL("abc&return_to=https://evil.example", "/")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if pid := u.Query().Get("provider_id"); pid != "abc&return_to=https://evil.example" {
		t.Errorf("provider_id = %q, want the literal value (no param split)", pid)
	}
	if rt := u.Query().Get("return_to"); rt != "/" {
		t.Errorf("return_to = %q, want / (hostile provider_id did not leak into it)", rt)
	}
}

// TestSAMLTestErrorReason covers the GoTrue ACS-rejection message builder.
func TestSAMLTestErrorReason(t *testing.T) {
	if got := samlTestErrorReason("access_denied", ""); !strings.Contains(got, "access_denied") {
		t.Errorf("no-desc reason = %q, want it to name the code", got)
	}
	if got := samlTestErrorReason("x", "cert mismatch"); !strings.Contains(got, "cert mismatch") {
		t.Errorf("desc reason = %q, want it to carry the description", got)
	}
}

// TestHandleDiscover_LocalMode_FallsToGitHub: local mode answers {sso:false}
// before ever touching the store, so a loginExt with no api wiring suffices
// (the store is never reached — that's the proof).
func TestHandleDiscover_LocalMode_FallsToGitHub(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)

	req := httptest.NewRequest(http.MethodPost, "/api/sso/discover",
		bytes.NewReader([]byte(`{"email":"alice@corp.com","return_to":"/dashboard"}`)))
	rec := httptest.NewRecorder()
	(&loginExt{}).handleDiscover(rec, req)

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

// TestHandleDiscover_OversizeBody_Rejected locks in the pre-auth body cap. The
// kernel's decoder answers 413 PAYLOAD_TOO_LARGE for a body past the cap,
// rather than folding it into the generic 400 the old lenient decoder gave.
func TestHandleDiscover_OversizeBody_Rejected(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)

	huge := strings.Repeat("a", ssoDiscoverMaxBody+(1<<10))
	req := httptest.NewRequest(http.MethodPost, "/api/sso/discover",
		strings.NewReader(`{"email":"`+huge+`@corp.com"}`))
	rec := httptest.NewRecorder()
	(&loginExt{}).handleDiscover(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 for an over-cap body (body=%q)", rec.Code, rec.Body.String())
	}
}

// TestHandleDiscover_BadBody_400 asserts a non-JSON body is a clean 400.
func TestHandleDiscover_BadBody_400(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)

	req := httptest.NewRequest(http.MethodPost, "/api/sso/discover",
		bytes.NewReader([]byte(`{not json`)))
	rec := httptest.NewRecorder()
	(&loginExt{}).handleDiscover(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a malformed body (body=%q)", rec.Code, rec.Body.String())
	}
}

// TestIdPFromMetadataURL covers the registration-time guess: the vendor hosts
// map to their product (any depth of subdomain, any case), everything else is
// "other" — a real answer — and only an unparseable URL is "" (not known).
func TestIdPFromMetadataURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://login.microsoftonline.com/9f1a/federationmetadata/2007-06/federationmetadata.xml?appid=1", "entra"},
		{"https://LOGIN.MICROSOFTONLINE.COM/x/y", "entra"},
		{"https://login.microsoftonline.us/x/y", "entra"},
		{"https://sts.windows.net/9f1a/", "entra"},
		{"https://dev-123.okta.com/app/abc/sso/saml/metadata", "okta"},
		{"https://corp.oktapreview.com/app/abc/sso/saml/metadata", "okta"},
		{"https://corp.okta-emea.com/app/abc/sso/saml/metadata", "okta"},
		{"https://accounts.google.com/o/saml2/idp?idpid=C0abc", "google"},
		{"https://app.onelogin.com/saml/metadata/abc", "onelogin"},
		{"https://corp.onelogin.com/saml/metadata/abc", "onelogin"},
		{"https://auth.pingone.com/abc/saml20/metadata", "ping"},
		{"https://sso.pingidentity.com/idp/metadata", "ping"},
		{"https://idp.corp.example/metadata.xml", "other"},
		{"https://notokta.com/metadata", "other"}, // suffix match wants a dot boundary
		{"https://okta.com.evil.example/metadata", "other"},
		{"", ""},
		{"://nope", ""},
	}
	for _, tc := range cases {
		if got := idpFromMetadataURL(tc.in); got != tc.want {
			t.Errorf("idpFromMetadataURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
