package server

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestGitHubAppRegister_LocalMode_Returns404 pins that the manifest
// registration endpoints are unreachable in local mode (authCfg is nil).
func TestGitHubAppRegister_LocalMode_Returns404(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	for _, tc := range []struct {
		method, path string
	}{
		{"POST", "/api/orgs/" + runmode.LocalDefaultOrgID + "/github-app/register/start"},
		{"GET", "/api/orgs/" + runmode.LocalDefaultOrgID + "/github-app/register/callback?code=x&state=y"},
	} {
		rec := doJSON(t, s, tc.method, tc.path, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: got %d, want 404", tc.method, tc.path, rec.Code)
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

// TestGitHubAppRegister_StartEndpoint_MultiMode exercises the start
// endpoint with a real Postgres-backed auth rig. Verifies:
//   - org admin gets 200 with manifest JSON + signed state
//   - non-member gets 404
//   - conflict when an app is already registered
func TestGitHubAppRegister_StartEndpoint_MultiMode(t *testing.T) {
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

	body := map[string]string{
		"owner_type":  "user",
		"owner_login": "alice",
	}

	t.Run("admin_gets_200", func(t *testing.T) {
		bodyJSON, _ := json.Marshal(body)
		req := httptest.NewRequest("POST", "/api/orgs/"+orgA.String()+"/github-app/register/start",
			jsonBody(bodyJSON))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: rig.srv.sidCookieName(), Value: sidA})
		req.Header.Set("Origin", rig.srv.authCfg.publicURL)
		rec := httptest.NewRecorder()
		rig.srv.mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
		}

		var out map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if _, ok := out["manifest_post_url"]; !ok {
			t.Error("response missing manifest_post_url")
		}
		if _, ok := out["manifest_json"]; !ok {
			t.Error("response missing manifest_json")
		}
		if _, ok := out["state"]; !ok {
			t.Error("response missing state")
		}

		// Verify manifest JSON is parseable and has expected fields.
		var manifest map[string]any
		if err := json.Unmarshal([]byte(out["manifest_json"].(string)), &manifest); err != nil {
			t.Fatalf("manifest_json not valid JSON: %v", err)
		}
		if name, ok := manifest["name"].(string); !ok || name == "" {
			t.Error("manifest missing name")
		}
		perms, ok := manifest["default_permissions"].(map[string]any)
		if !ok {
			t.Fatal("manifest missing default_permissions")
		}
		if perms["pull_requests"] != "write" {
			t.Errorf("pull_requests permission = %v, want write", perms["pull_requests"])
		}
	})

	t.Run("non_member_gets_404", func(t *testing.T) {
		bodyJSON, _ := json.Marshal(body)
		req := httptest.NewRequest("POST", "/api/orgs/"+orgA.String()+"/github-app/register/start",
			jsonBody(bodyJSON))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: rig.srv.sidCookieName(), Value: sidB})
		req.Header.Set("Origin", rig.srv.authCfg.publicURL)
		rec := httptest.NewRecorder()
		rig.srv.mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("non-member status=%d, want 404", rec.Code)
		}
	})

	t.Run("missing_body_fields", func(t *testing.T) {
		bodyJSON, _ := json.Marshal(map[string]string{"owner_type": "user"})
		req := httptest.NewRequest("POST", "/api/orgs/"+orgA.String()+"/github-app/register/start",
			jsonBody(bodyJSON))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: rig.srv.sidCookieName(), Value: sidA})
		req.Header.Set("Origin", rig.srv.authCfg.publicURL)
		rec := httptest.NewRecorder()
		rig.srv.mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("missing fields status=%d, want 400", rec.Code)
		}
	})
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

	// Sign a valid state token.
	state := appRegisterState{
		OrgID:     orgA.String(),
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
	}
	signed, err := state.sign(rig.srv.authCfg.stateKey)
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
		if loc != "/settings/workspace#github-app" {
			t.Errorf("redirect location=%q, want /settings/workspace#github-app", loc)
		}

		// Verify the org_github_apps row was written.
		var appID, slug string
		if err := rig.h.AdminDB.QueryRow(`
			SELECT app_id, slug FROM org_github_apps WHERE org_id = $1
		`, orgA.String()).Scan(&appID, &slug); err != nil {
			t.Fatalf("read org_github_apps: %v", err)
		}
		if appID != "12345" {
			t.Errorf("app_id=%q, want 12345", appID)
		}
		if slug != "triage-factory-test" {
			t.Errorf("slug=%q, want triage-factory-test", slug)
		}
	})

	t.Run("conflict_on_second_registration", func(t *testing.T) {
		// The row was already written by the previous subtest.
		state2 := appRegisterState{
			OrgID:     orgA.String(),
			ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
		}
		signed2, _ := state2.sign(rig.srv.authCfg.stateKey)

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
		tok, _ := expired.sign(rig.srv.authCfg.stateKey)

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
		tok, _ := wrongOrg.sign(rig.srv.authCfg.stateKey)

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

// TestGitHubAPIBase verifies the URL derivation helper.
func TestGitHubAPIBase(t *testing.T) {
	tests := []struct {
		base, want string
	}{
		{"https://github.com", "https://api.github.com"},
		{"https://github.com/", "https://api.github.com"},
		{"https://github.acme.com", "https://github.acme.com/api/v3"},
		{"https://github.acme.com/", "https://github.acme.com/api/v3"},
		{"http://localhost:3000", "http://localhost:3000/api/v3"},
	}
	for _, tt := range tests {
		got := githubAPIBase(tt.base)
		if got != tt.want {
			t.Errorf("githubAPIBase(%q) = %q, want %q", tt.base, got, tt.want)
		}
	}
}

func jsonBody(b []byte) io.Reader {
	return bytes.NewReader(b)
}
