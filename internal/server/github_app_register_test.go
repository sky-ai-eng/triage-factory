package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestGitHubAppRegister_LocalMode_NilDeployConfig_Returns404 pins that
// the endpoints return 404 when deployCfg is nil (server not fully
// booted).
func TestGitHubAppRegister_LocalMode_NilDeployConfig_Returns404(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	// Don't call SetDeployConfig — deployCfg stays nil.

	for _, tc := range []struct {
		method, path string
	}{
		{"GET", "/api/orgs/" + runmode.LocalDefaultOrgID + "/github-app/register/launch?owner_type=user&owner_login=testuser"},
		{"GET", "/api/orgs/" + runmode.LocalDefaultOrgID + "/github-app/register/callback?code=x&state=y"},
	} {
		rec := doJSON(t, s, tc.method, tc.path, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: got %d, want 404", tc.method, tc.path, rec.Code)
		}
	}
}

// TestGitHubAppRegister_LocalMode_WithDeployConfig_Works verifies the
// launch endpoint serves the bounce page in local mode when deployCfg is
// wired, with a per-response CSP scoped to GitHub (not 'self').
func TestGitHubAppRegister_LocalMode_WithDeployConfig_Works(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	s.SetDeployConfig("http://localhost:3000", key)

	rec := doJSON(t, s, "GET",
		"/api/orgs/"+runmode.LocalDefaultOrgID+"/github-app/register/launch?owner_type=user&owner_login=testuser", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("launch endpoint status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}

	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "form-action https://github.com") {
		t.Errorf("launch CSP missing scoped form-action: %q", csp)
	}
	if strings.Contains(csp, "form-action 'self'") {
		t.Errorf("launch CSP must not use 'self': %q", csp)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `action="https://github.com/settings/apps/new?state=`) {
		t.Errorf("bounce page form action missing/incorrect: %s", body)
	}
	if !strings.Contains(body, `name="manifest"`) {
		t.Errorf("bounce page missing manifest field: %s", body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control=%q, want no-store (page carries signed state)", cc)
	}
}

// TestGitHubAppRegister_Launch_ErrorPageOnBadInput verifies a launch
// failure renders a graceful HTML page (with a back-link) rather than a JSON
// dead-end — important because the user arrives via a top-level navigation, not
// a fetch — and that the back-link honors return_to: a Settings launch (the
// default) links back to Settings, a wizard launch back to /setup.
func TestGitHubAppRegister_Launch_ErrorPageOnBadInput(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	s.SetDeployConfig("http://localhost:3000", key)

	base := "/api/orgs/" + runmode.LocalDefaultOrgID + "/github-app/register/launch?owner_type=user"

	// owner_login omitted, no return_to → defaults to the Settings back-link.
	rec := doJSON(t, s, "GET", base, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type=%q, want an HTML error page", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Back to Settings") ||
		!strings.Contains(body, `href="/settings#github-app"`) {
		t.Errorf("default error page missing Settings back-link: %s", body)
	}

	// Same failure launched from the wizard (return_to=setup) → /setup back-link,
	// so an early validation failure doesn't strand a wizard user on Settings.
	recSetup := doJSON(t, s, "GET", base+"&return_to=setup", nil)
	if recSetup.Code != http.StatusBadRequest {
		t.Fatalf("setup status=%d, want 400", recSetup.Code)
	}
	if body := recSetup.Body.String(); !strings.Contains(body, "Back to setup") ||
		!strings.Contains(body, `href="/setup"`) {
		t.Errorf("setup error page missing /setup back-link: %s", body)
	}
}

// TestGitHubWebOrigin pins the validation/normalization of the
// admin-controlled GitHub base URL before it's concatenated into a CSP
// header: only http(s) origins pass, paths/whitespace are stripped, and
// malformed input is rejected (so it can't distort the CSP).
func TestGitHubWebOrigin(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"https://github.com", "https://github.com", true},
		{"https://git.corp.example.com", "https://git.corp.example.com", true},
		{"https://git.corp.example.com:8443", "https://git.corp.example.com:8443", true},
		{"https://github.com/path/ignored", "https://github.com", true},
		{"  https://github.com  ", "https://github.com", true},
		{"http://localhost:3000", "http://localhost:3000", true},
		{"ftp://github.com", "", false},
		{"github.com", "", false},
		{"", "", false},
		{"https://bad host", "", false},
	}
	for _, tc := range cases {
		got, ok := gitHubWebOrigin(tc.in)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("gitHubWebOrigin(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

// TestIsPubliclyReachable pins which hosts GitHub could reach for a
// hook_attributes.url: loopback / unspecified / private / link-local
// hosts are not reachable (manifest substitutes an inert public
// placeholder hook), public DNS names and public IPs are.
func TestIsPubliclyReachable(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"http://localhost:3000", false},
		{"http://127.0.0.1:3000", false},
		{"http://127.0.0.1", false},
		{"http://[::1]:3000", false},
		{"http://0.0.0.0:3000", false},
		{"http://[::]:3000", false},
		{"http://10.0.0.5:3000", false},
		{"http://192.168.1.10", false},
		{"http://172.16.4.2", false},
		{"http://169.254.10.1", false},
		{"http://100.64.0.1:3000", false}, // carrier-grade NAT
		{"http://192.0.2.5", false},       // TEST-NET-1
		{"http://198.51.100.7", false},    // TEST-NET-2
		{"http://203.0.113.9", false},     // TEST-NET-3
		{"http://198.18.0.1", false},      // benchmarking
		{"http://240.0.0.1", false},       // reserved
		{"http://[2001:db8::1]", false},   // IPv6 documentation
		{"http://[ff02::1]", false},       // IPv6 multicast
		{"https://app.triagefactory.com", true},
		{"https://git.corp.example.com", true},
		{"https://github.acme.com:8443", true},
		{"https://8.8.8.8", true},
		{"https://[2606:4700:4700::1111]", true}, // public IPv6
		{"", false},
	}
	for _, tc := range cases {
		if got := isPubliclyReachable(tc.in); got != tc.want {
			t.Errorf("isPubliclyReachable(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestGitHubAppRegister_LocalManifest_PlaceholderHookAndNoInstallationEvents
// pins the manifest shape for a non-public (localhost) deployment:
// GitHub rejects both a localhost hook URL and a blank/absent one, so the
// manifest carries an inert public placeholder hook (active:false, never
// delivered — local installs are discovered via API backfill). It also
// keeps a default_events list free of the App-lifecycle installation
// events GitHub rejects. Asserts against the decoded manifest JSON rather
// than the HTML-escaped page body so a reintroduced "installation" event
// can't slip past the check.
func TestGitHubAppRegister_LocalManifest_PlaceholderHookAndNoInstallationEvents(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	s.SetDeployConfig("http://localhost:3000", key)

	_, manifestJSON, _, err := s.buildManifestAndState(context.Background(),
		runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "user", "testuser", "settings")
	if err != nil {
		t.Fatalf("buildManifestAndState: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal([]byte(manifestJSON), &m); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}

	hook, ok := m["hook_attributes"].(map[string]any)
	if !ok {
		t.Fatalf("localhost manifest must include hook_attributes — GitHub rejects a blank/absent hook url: %s", manifestJSON)
	}
	if got := hook["url"]; got != "https://example.com/triage-factory-webhook-unconfigured" {
		t.Errorf("localhost hook url = %v, want the inert example.com placeholder: %s", got, manifestJSON)
	}
	if active, _ := hook["active"].(bool); active {
		t.Errorf("localhost placeholder hook must be inactive (active:false): %s", manifestJSON)
	}

	// Homepage url is cosmetic; for a non-public deployment it shows the
	// product homepage rather than a meaningless localhost address.
	if got := m["url"]; got != "https://www.triagefactory.com" {
		t.Errorf("localhost manifest url (homepage) = %v, want the product homepage: %s", got, manifestJSON)
	}
	// But the functional callback URL must still point at this deployment.
	if got, _ := m["redirect_url"].(string); !strings.HasPrefix(got, "http://localhost:3000/") {
		t.Errorf("redirect_url = %v, must point at the local deployment, not the homepage", got)
	}

	rawEvents, ok := m["default_events"].([]any)
	if !ok {
		t.Fatalf("default_events missing or not a list: %s", manifestJSON)
	}
	events := make(map[string]bool, len(rawEvents))
	for _, e := range rawEvents {
		events[e.(string)] = true
	}
	if events["installation"] || events["installation_repositories"] {
		t.Errorf("manifest must not subscribe to installation events: %v", rawEvents)
	}
	for _, want := range []string{"pull_request", "check_suite", "issue_comment"} {
		if !events[want] {
			t.Errorf("manifest missing expected default_event %q: %v", want, rawEvents)
		}
	}
}

// TestGitHubAppRegister_StateToken_RoundTrip verifies the HMAC-signed
// state token signs and parses correctly, and that expired / tampered
// tokens are rejected.
func TestGitHubAppRegister_StateToken_RoundTrip(t *testing.T) {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	st := appRegisterState{
		OrgID:     "00000000-0000-0000-0000-000000000099",
		OwnerType: "org",
		ReturnTo:  "setup",
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
	}
	signed, err := st.sign(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	parsed, err := parseAppRegisterState(signed, key)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.OrgID != st.OrgID {
		t.Errorf("org_id = %q, want %q", parsed.OrgID, st.OrgID)
	}
	if parsed.OwnerType != st.OwnerType {
		t.Errorf("owner_type = %q, want %q", parsed.OwnerType, st.OwnerType)
	}
	if parsed.ReturnTo != st.ReturnTo {
		t.Errorf("return_to = %q, want %q", parsed.ReturnTo, st.ReturnTo)
	}

	t.Run("expired", func(t *testing.T) {
		expired := appRegisterState{
			OrgID:     st.OrgID,
			ExpiresAt: time.Now().Add(-1 * time.Minute).Unix(),
		}
		tok, _ := expired.sign(key)
		if _, err := parseAppRegisterState(tok, key); err == nil {
			t.Error("expected error for expired token")
		}
	})

	t.Run("wrong_key", func(t *testing.T) {
		var otherKey [32]byte
		if _, err := rand.Read(otherKey[:]); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := parseAppRegisterState(signed, otherKey); err == nil {
			t.Error("expected error for wrong key")
		}
	})

	t.Run("malformed", func(t *testing.T) {
		if _, err := parseAppRegisterState("not.valid.token", key); err == nil {
			t.Error("expected error for malformed token")
		}
	})
}

// TestGitHubAppRegister_LaunchEndpoint_MultiMode exercises the launch
// bounce page with a real Postgres-backed auth rig. Verifies:
//   - org admin gets a 200 HTML page with the manifest form
//   - the per-response CSP scopes form-action to the org's GitHub host
//     (github.com by default), NOT 'self'
//   - non-member gets 404
//   - missing query params get 400
func TestGitHubAppRegister_LaunchEndpoint_MultiMode(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	rig := newAuthRig(t)

	alice := rig.seedUser()
	orgA, _ := rig.seedOrg(alice, "alice-org")
	resp, _ := rig.driveCallback(alice)
	sidA := rig.sidFromResp(resp)

	bob := rig.seedUser()
	rig.seedOrg(bob, "bob-org")
	respB, _ := rig.driveCallback(bob)
	sidB := rig.sidFromResp(respB)

	launch := func(orgID, sid, query string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET",
			"/api/orgs/"+orgID+"/github-app/register/launch?"+query, nil)
		req.AddCookie(&http.Cookie{Name: rig.srv.sidCookieName(), Value: sid})
		rec := httptest.NewRecorder()
		rig.srv.mux.ServeHTTP(rec, req)
		return rec
	}

	t.Run("admin_gets_html_with_scoped_csp", func(t *testing.T) {
		rec := launch(orgA.String(), sidA, "owner_type=user&owner_login=alice")
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("Content-Type=%q, want text/html", ct)
		}

		csp := rec.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "form-action https://github.com") {
			t.Errorf("CSP missing scoped form-action: %q", csp)
		}
		if strings.Contains(csp, "form-action 'self'") {
			t.Errorf("CSP must not use 'self' on the bounce page: %q", csp)
		}
		if !strings.Contains(csp, "default-src 'none'") {
			t.Errorf("CSP missing default-src 'none': %q", csp)
		}

		body := rec.Body.String()
		if !strings.Contains(body, `action="https://github.com/settings/apps/new?state=`) {
			t.Errorf("form action missing/incorrect: %s", body)
		}
		if !strings.Contains(body, `name="manifest"`) {
			t.Errorf("manifest field missing: %s", body)
		}
		// The manifest JSON lands in the hidden input value, HTML-escaped.
		if !strings.Contains(body, `pull_requests`) {
			t.Errorf("manifest payload missing from page: %s", body)
		}
	})

	t.Run("non_member_gets_404", func(t *testing.T) {
		rec := launch(orgA.String(), sidB, "owner_type=user&owner_login=alice")
		if rec.Code != http.StatusNotFound {
			t.Errorf("non-member status=%d, want 404", rec.Code)
		}
	})

	t.Run("missing_query_params", func(t *testing.T) {
		rec := launch(orgA.String(), sidA, "owner_type=user")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("missing params status=%d, want 400", rec.Code)
		}
	})
}

// TestGitHubAppRegister_LaunchEndpoint_GHES verifies an org whose
// github_base_url points at a GitHub Enterprise host gets that host in
// both the form action and the per-response form-action CSP — proving
// the scoping is per-org, which a single global CSP couldn't express.
func TestGitHubAppRegister_LaunchEndpoint_GHES(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	rig := newAuthRig(t)

	alice := rig.seedUser()
	orgA, _ := rig.seedOrg(alice, "ghes-org")
	resp, _ := rig.driveCallback(alice)
	sidA := rig.sidFromResp(resp)

	const ghesHost = "https://git.corp.example.com"
	if _, err := rig.h.AdminDB.Exec(`
		INSERT INTO org_settings (org_id, github_base_url)
		VALUES ($1, $2)
		ON CONFLICT (org_id) DO UPDATE SET github_base_url = $2
	`, orgA.String(), ghesHost); err != nil {
		t.Fatalf("seed org_settings: %v", err)
	}

	req := httptest.NewRequest("GET",
		"/api/orgs/"+orgA.String()+"/github-app/register/launch?owner_type=org&owner_login=acme-eng", nil)
	req.AddCookie(&http.Cookie{Name: rig.srv.sidCookieName(), Value: sidA})
	rec := httptest.NewRecorder()
	rig.srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}

	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "form-action "+ghesHost) {
		t.Errorf("CSP form-action not scoped to GHES host: %q", csp)
	}
	if strings.Contains(csp, "github.com") {
		t.Errorf("CSP leaked github.com for a GHES org: %q", csp)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `action="`+ghesHost+`/organizations/acme-eng/settings/apps/new?state=`) {
		t.Errorf("form action not scoped to GHES host: %s", body)
	}
}

// TestGitHubAppRegister_CallbackEndpoint_MultiMode exercises the
// callback endpoint with a stubbed GitHub API server.
func TestGitHubAppRegister_CallbackEndpoint_MultiMode(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	rig := newAuthRig(t)

	alice := rig.seedUser()
	orgA, _ := rig.seedOrg(alice, "alice-org")
	resp, _ := rig.driveCallback(alice)
	sidA := rig.sidFromResp(resp)

	// Stub GitHub's /app-manifests/{code}/conversions endpoint.
	ghStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{
			"id": 12345,
			"slug": "triage-factory-test",
			"client_id": "Iv1.test_client_id",
			"client_secret": "test_client_secret_value",
			"pem": "-----BEGIN RSA PRIVATE KEY-----\ntest\n-----END RSA PRIVATE KEY-----",
			"webhook_secret": "test_webhook_secret"
		}`)
	}))
	t.Cleanup(ghStub.Close)

	// Seed the org's GitHubBaseURL to point at our stub.
	if _, err := rig.h.AdminDB.Exec(`
		INSERT INTO org_settings (org_id, github_base_url)
		VALUES ($1, $2)
		ON CONFLICT (org_id) DO UPDATE SET github_base_url = $2
	`, orgA.String(), ghStub.URL); err != nil {
		t.Fatalf("seed org_settings: %v", err)
	}

	// Sign a valid state token. owner_type=org rides the signed state — the
	// callback persists it from there (not a re-supplied query param).
	state := appRegisterState{
		OrgID:     orgA.String(),
		OwnerType: "org",
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
	}
	signed, err := state.sign(rig.srv.deployCfg.hmacKey)
	if err != nil {
		t.Fatalf("sign state: %v", err)
	}

	t.Run("valid_exchange", func(t *testing.T) {
		req := httptest.NewRequest("GET",
			"/api/orgs/"+orgA.String()+"/github-app/register/callback?code=test_code&state="+signed, nil)
		req.AddCookie(&http.Cookie{Name: rig.srv.sidCookieName(), Value: sidA})
		rec := httptest.NewRecorder()
		rig.srv.mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusFound {
			t.Fatalf("callback status=%d body=%s, want 302", rec.Code, rec.Body.String())
		}
		loc := rec.Header().Get("Location")
		wantLoc := "/orgs/" + orgA.String() + "/settings#github-app"
		if loc != wantLoc {
			t.Errorf("redirect location=%q, want %q", loc, wantLoc)
		}

		// Verify the org_github_apps row was written, including the owner_type
		// captured from the signed state.
		var appID, slug, ownerType string
		if err := rig.h.AdminDB.QueryRow(`
			SELECT app_id, slug, owner_type FROM org_github_apps WHERE org_id = $1
		`, orgA.String()).Scan(&appID, &slug, &ownerType); err != nil {
			t.Fatalf("read org_github_apps: %v", err)
		}
		if appID != "12345" {
			t.Errorf("app_id=%q, want 12345", appID)
		}
		if slug != "triage-factory-test" {
			t.Errorf("slug=%q, want triage-factory-test", slug)
		}
		if ownerType != "org" {
			t.Errorf("owner_type=%q, want org (from signed state)", ownerType)
		}
	})

	t.Run("status_carries_owner_type", func(t *testing.T) {
		// The row registered above must surface owner_type through the status
		// endpoint so Setup/Settings can seed the App account-type summary.
		req := httptest.NewRequest("GET", "/api/orgs/"+orgA.String()+"/github-app", nil)
		req.AddCookie(&http.Cookie{Name: rig.srv.sidCookieName(), Value: sidA})
		rec := httptest.NewRecorder()
		rig.srv.mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
		}
		var out githubAppStatusResponse
		if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out.App == nil {
			t.Fatal("app=nil, want registered App")
		}
		if out.App.OwnerType != "org" {
			t.Errorf("status owner_type=%q, want org", out.App.OwnerType)
		}
	})

	t.Run("conflict_on_second_registration", func(t *testing.T) {
		// The row was already written by the previous subtest.
		state2 := appRegisterState{
			OrgID:     orgA.String(),
			ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
		}
		signed2, _ := state2.sign(rig.srv.deployCfg.hmacKey)

		req := httptest.NewRequest("GET",
			"/api/orgs/"+orgA.String()+"/github-app/register/callback?code=test_code2&state="+signed2, nil)
		req.AddCookie(&http.Cookie{Name: rig.srv.sidCookieName(), Value: sidA})
		rec := httptest.NewRecorder()
		rig.srv.mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusConflict {
			t.Errorf("second registration status=%d body=%s, want 409", rec.Code, rec.Body.String())
		}
	})

	t.Run("expired_state", func(t *testing.T) {
		expired := appRegisterState{
			OrgID:     orgA.String(),
			ExpiresAt: time.Now().Add(-1 * time.Minute).Unix(),
		}
		tok, _ := expired.sign(rig.srv.deployCfg.hmacKey)

		req := httptest.NewRequest("GET",
			"/api/orgs/"+orgA.String()+"/github-app/register/callback?code=c&state="+tok, nil)
		req.AddCookie(&http.Cookie{Name: rig.srv.sidCookieName(), Value: sidA})
		rec := httptest.NewRecorder()
		rig.srv.mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expired state status=%d, want 401", rec.Code)
		}
	})

	t.Run("wrong_org_in_state", func(t *testing.T) {
		wrongOrg := appRegisterState{
			OrgID:     "00000000-0000-0000-0000-ffffffffffff",
			ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
		}
		tok, _ := wrongOrg.sign(rig.srv.deployCfg.hmacKey)

		req := httptest.NewRequest("GET",
			"/api/orgs/"+orgA.String()+"/github-app/register/callback?code=c&state="+tok, nil)
		req.AddCookie(&http.Cookie{Name: rig.srv.sidCookieName(), Value: sidA})
		rec := httptest.NewRecorder()
		rig.srv.mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("wrong org state status=%d, want 401", rec.Code)
		}
	})
}

// TestGitHubAppRegister_Callback_HooklessNoWebhookSecret verifies the
// callback's defensive tolerance: if GitHub's manifest conversion returns
// no webhook_secret, the callback still succeeds, stores the App with an
// empty webhook_secret_ref, and writes no Vault entry for the
// (nonexistent) secret. NOTE: our manifests now always request a hook
// (real URL when public, inert example.com placeholder otherwise), so
// GitHub returns a secret in practice — this exercises the defensive path
// against a partial response, not the normal local flow.
func TestGitHubAppRegister_Callback_HooklessNoWebhookSecret(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	rig := newAuthRig(t)

	alice := rig.seedUser()
	orgA, _ := rig.seedOrg(alice, "alice-org")
	resp, _ := rig.driveCallback(alice)
	sidA := rig.sidFromResp(resp)

	// Stub conversion response with NO webhook_secret field.
	ghStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{
			"id": 67890,
			"slug": "triage-factory-hookless",
			"client_id": "Iv1.hookless_client_id",
			"client_secret": "hookless_client_secret",
			"pem": "-----BEGIN RSA PRIVATE KEY-----\ntest\n-----END RSA PRIVATE KEY-----"
		}`)
	}))
	t.Cleanup(ghStub.Close)

	if _, err := rig.h.AdminDB.Exec(`
		INSERT INTO org_settings (org_id, github_base_url)
		VALUES ($1, $2)
		ON CONFLICT (org_id) DO UPDATE SET github_base_url = $2
	`, orgA.String(), ghStub.URL); err != nil {
		t.Fatalf("seed org_settings: %v", err)
	}

	state := appRegisterState{
		OrgID: orgA.String(),
		// The production launch flow always signs a validated owner type, which
		// the callback persists; set it so this token matches a real one.
		OwnerType: "user",
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
	}
	signed, err := state.sign(rig.srv.deployCfg.hmacKey)
	if err != nil {
		t.Fatalf("sign state: %v", err)
	}

	req := httptest.NewRequest("GET",
		"/api/orgs/"+orgA.String()+"/github-app/register/callback?code=hookless_code&state="+signed, nil)
	req.AddCookie(&http.Cookie{Name: rig.srv.sidCookieName(), Value: sidA})
	rec := httptest.NewRecorder()
	rig.srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("callback status=%d body=%s, want 302", rec.Code, rec.Body.String())
	}

	var appID, webhookRef string
	if err := rig.h.AdminDB.QueryRow(`
		SELECT app_id, webhook_secret_ref FROM org_github_apps WHERE org_id = $1
	`, orgA.String()).Scan(&appID, &webhookRef); err != nil {
		t.Fatalf("read org_github_apps: %v", err)
	}
	if appID != "67890" {
		t.Errorf("app_id=%q, want 67890", appID)
	}
	if webhookRef != "" {
		t.Errorf("webhook_secret_ref=%q, want empty for a hookless App", webhookRef)
	}

	// No Vault entry should exist under the conventional webhook key.
	// Read through the org-scoped tx so the Vault wrapper sees the org
	// context its RLS requires; a missing secret comes back as "".
	var webhookSecret string
	if err := rig.srv.tx.WithTx(context.Background(), orgA.String(), alice.String(), func(tx db.TxStores) error {
		var e error
		webhookSecret, e = tx.Secrets.Get(context.Background(), orgA.String(), "github_app_67890_webhook_secret")
		return e
	}); err != nil {
		t.Fatalf("read webhook secret: %v", err)
	}
	if webhookSecret != "" {
		t.Errorf("expected no Vault entry for the webhook secret, got %q", webhookSecret)
	}
}

// TestSettingsRedirectPath pins the mode-aware post-callback redirect:
// local mode lands on the flat /settings route, multi mode on the
// org-scoped one. The #github-app fragment is appended by the caller.
func TestSettingsRedirectPath(t *testing.T) {
	t.Run("local", func(t *testing.T) {
		runmode.SetForTest(t, runmode.ModeLocal)
		if got := settingsRedirectPath("org-x"); got != "/settings" {
			t.Errorf("local redirect = %q, want /settings", got)
		}
	})
	t.Run("multi", func(t *testing.T) {
		runmode.SetForTest(t, runmode.ModeMulti)
		if got := settingsRedirectPath("org-x"); got != "/orgs/org-x/settings" {
			t.Errorf("multi redirect = %q, want /orgs/org-x/settings", got)
		}
	})
}

// extractLaunchStateParam pulls the signed state token out of the launch
// bounce page's form action (action="…?state=<token>"). The token charset
// (base64url plus '.') is both URL- and HTML-safe, so it survives template
// rendering verbatim between "?state=" and the closing quote.
func extractLaunchStateParam(t *testing.T, body string) string {
	t.Helper()
	const marker = "?state="
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("launch page has no ?state= param: %s", body)
	}
	rest := body[i+len(marker):]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		t.Fatalf("launch page action attr is unterminated: %s", body)
	}
	raw, err := url.QueryUnescape(rest[:end])
	if err != nil {
		t.Fatalf("unescape state: %v", err)
	}
	return raw
}

// TestGitHubAppRegister_Launch_ReturnTo pins the launch handler's return_to
// validation: a recognized value ("setup"/"settings") is carried into the
// signed state verbatim, and any other or absent value defaults to "settings"
// (so the callback's redirect degrades to the safe Settings landing rather
// than erroring). Decodes the signed state out of the bounce page to assert
// the value the callback will actually read.
func TestGitHubAppRegister_Launch_ReturnTo(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	s.SetDeployConfig("http://localhost:3000", key)

	cases := []struct {
		name   string
		query  string
		wantRT string
	}{
		{"setup", "&return_to=setup", "setup"},
		{"settings", "&return_to=settings", "settings"},
		{"bogus_defaults_to_settings", "&return_to=team-config", "settings"},
		{"absent_defaults_to_settings", "", "settings"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSON(t, s, "GET",
				"/api/orgs/"+runmode.LocalDefaultOrgID+"/github-app/register/launch?owner_type=user&owner_login=testuser"+tc.query, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
			}
			st, err := parseAppRegisterState(extractLaunchStateParam(t, rec.Body.String()), key)
			if err != nil {
				t.Fatalf("parse state: %v", err)
			}
			if st.ReturnTo != tc.wantRT {
				t.Errorf("ReturnTo=%q, want %q", st.ReturnTo, tc.wantRT)
			}
		})
	}
}

// TestGitHubAppRegister_Callback_ReturnToRedirect pins the callback's
// post-registration redirect: a signed state carrying rt=setup lands the
// browser on /setup (so the wizard resumes on the "Install the App" step
// instead of teleporting past it), while a Settings-launched flow (no rt)
// returns to the org's Settings GitHub panel.
func TestGitHubAppRegister_Callback_ReturnToRedirect(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	rig := newAuthRig(t)

	ghStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{
			"id": 24680,
			"slug": "tf-returnto",
			"client_id": "Iv1.rt",
			"client_secret": "secret",
			"pem": "-----BEGIN RSA PRIVATE KEY-----\ntest\n-----END RSA PRIVATE KEY-----",
			"webhook_secret": "whsec"
		}`)
	}))
	t.Cleanup(ghStub.Close)

	// driveOnce registers an App for a fresh alice-owned org, signing the state
	// with the given ReturnTo, and returns the callback's redirect Location plus
	// the org id. A fresh org per call sidesteps the one-App-per-org conflict.
	driveOnce := func(t *testing.T, name, returnTo string) (loc, orgID string) {
		t.Helper()
		alice := rig.seedUser()
		org, _ := rig.seedOrg(alice, name)
		resp, _ := rig.driveCallback(alice)
		sid := rig.sidFromResp(resp)
		if _, err := rig.h.AdminDB.Exec(`
			INSERT INTO org_settings (org_id, github_base_url)
			VALUES ($1, $2)
			ON CONFLICT (org_id) DO UPDATE SET github_base_url = $2
		`, org.String(), ghStub.URL); err != nil {
			t.Fatalf("seed org_settings: %v", err)
		}
		state := appRegisterState{
			OrgID: org.String(),
			// The production launch flow always signs a validated owner type into
			// the state, which the callback persists; set it here so the token
			// matches a real one and stays valid if callback-side owner-type
			// validation is ever tightened.
			OwnerType: "user",
			ReturnTo:  returnTo,
			ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
		}
		signed, err := state.sign(rig.srv.deployCfg.hmacKey)
		if err != nil {
			t.Fatalf("sign state: %v", err)
		}
		req := httptest.NewRequest("GET",
			"/api/orgs/"+org.String()+"/github-app/register/callback?code=c&state="+signed, nil)
		req.AddCookie(&http.Cookie{Name: rig.srv.sidCookieName(), Value: sid})
		rec := httptest.NewRecorder()
		rig.srv.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusFound {
			t.Fatalf("callback status=%d body=%s, want 302", rec.Code, rec.Body.String())
		}
		return rec.Header().Get("Location"), org.String()
	}

	t.Run("rt_setup_lands_on_setup", func(t *testing.T) {
		loc, _ := driveOnce(t, "rt-setup-org", "setup")
		if loc != "/setup" {
			t.Errorf("redirect=%q, want /setup", loc)
		}
	})

	t.Run("default_lands_on_settings", func(t *testing.T) {
		loc, orgID := driveOnce(t, "rt-settings-org", "")
		if want := "/orgs/" + orgID + "/settings#github-app"; loc != want {
			t.Errorf("redirect=%q, want %q", loc, want)
		}
	})
}
