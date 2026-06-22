package sso

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestSSOConnection_LocalMode404: every connection route 404s in local mode
// (N=1, no IdP). adminGate checks runmode before touching any dependency, so
// this is a pure unit test — no rig, no GoTrue, no store needed.
func TestSSOConnection_LocalMode404(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	h := &ssoConnectionHandler{}

	cases := []struct {
		method string
		fn     func(http.ResponseWriter, *http.Request)
	}{
		{http.MethodGet, h.handleSSOConnectionGet},
		{http.MethodPost, h.handleSSOConnectionCreate},
		{http.MethodPatch, h.handleSSOConnectionUpdate},
	}
	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/api/sso/connection", nil)
			rec := httptest.NewRecorder()
			tc.fn(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s status = %d; want 404 in local mode", tc.method, rec.Code)
			}
		})
	}
}

// TestSSOTestResultRendering executes the verify-before-enforce result template
// for every outcome, escapes the IdP-derived fields, and ships the lock-down
// CSP. Runs without a testcontainer.
func TestSSOTestResultRendering(t *testing.T) {
	le := &loginExt{}
	cases := []struct {
		name string
		res  samlTestResult
		want []string
		deny []string
	}{
		{
			name: "pass with domain match",
			res:  samlTestResult{Pass: true, Email: "a@corp.com", EmailVerified: true, Domain: "corp.com", DomainMatch: true},
			want: []string{"SSO test passed", "a@corp.com", "is verified for your organization"},
			deny: []string{"SSO test failed"},
		},
		{
			name: "pass without domain match",
			res:  samlTestResult{Pass: true, Email: "a@corp.com", EmailVerified: true, Domain: "corp.com"},
			want: []string{"SSO test passed", "is not a verified domain for your organization"},
		},
		{
			name: "fail surfaces the reason",
			res:  samlTestResult{Reason: "Sign-in completed, but the assertion carried no email claim."},
			want: []string{"SSO test failed", "no email claim"},
			deny: []string{"SSO test passed"},
		},
		{
			name: "escapes IdP-derived markup",
			res:  samlTestResult{Pass: true, Email: "<script>alert(1)</script>@x.com", Domain: "x.com"},
			want: []string{"&lt;script&gt;"},
			deny: []string{"<script>alert(1)"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			le.renderSAMLTestResult(rec, tc.res)
			if rec.Code != http.StatusOK {
				t.Fatalf("code=%d, want 200", rec.Code)
			}
			body := rec.Body.String()
			for _, w := range tc.want {
				if !strings.Contains(body, w) {
					t.Errorf("body missing %q:\n%s", w, body)
				}
			}
			for _, d := range tc.deny {
				if strings.Contains(body, d) {
					t.Errorf("body unexpectedly contains %q", d)
				}
			}
			csp := rec.Header().Get("Content-Security-Policy")
			if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "frame-ancestors 'none'") {
				t.Errorf("CSP backstop missing or weakened: %q", csp)
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("Cache-Control=%q, want no-store (page carries PII)", cc)
			}
		})
	}
}
