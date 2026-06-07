package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/zalando/go-keyring"
)

// TestResolveJiraHost pins the host canonicalization the per-user credential is
// keyed under: a trailing slash and surrounding whitespace are trimmed so the
// status reader and PAT writer compose the same "jira_token/<host>" key; an
// empty config (Jira not configured) and a malformed/non-http base are
// rejected (no host to key a lookup off). Unlike GitHub there's no default
// host — empty is genuinely "not configured."
func TestResolveJiraHost(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"https://jira.corp.example.com", "https://jira.corp.example.com", true},
		{"https://jira.corp.example.com/", "https://jira.corp.example.com", true},          // trailing slash trimmed
		{"  https://acme.atlassian.net  ", "https://acme.atlassian.net", true},             // whitespace trimmed
		{"https://jira.corp.example.com/jira", "https://jira.corp.example.com/jira", true}, // path preserved
		{"", "", false},          // not configured
		{"not-a-url", "", false}, // no scheme/host
		{"ftp://jira.corp.example.com", "", false},
	}
	for _, tc := range cases {
		got, ok := resolveJiraHost(tc.in)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("resolveJiraHost(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

// seedLocalOrgJiraHost sets the local sentinel org's jira_base_url via the
// store (the value the Jira identity handlers resolve + key the per-user
// credential under). The Jira mirror of seedLocalOrgGitHubHost.
func seedLocalOrgJiraHost(t *testing.T, s *Server, host string) {
	t.Helper()
	ctx := context.Background()
	if err := s.tx.WithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(tx db.TxStores) error {
		set, err := tx.Orgs.GetSettings(ctx, runmode.LocalDefaultOrgID)
		if err != nil {
			return err
		}
		set.JiraBaseURL = host
		return tx.Orgs.UpdateSettings(ctx, runmode.LocalDefaultOrgID, set)
	}); err != nil {
		t.Fatalf("seed org jira host: %v", err)
	}
}

// jiraMyselfStub stands up a Jira host whose /rest/api/2/myself answers with the
// given JSON body, capturing the Authorization header the validator sent. The
// DC PAT config (Bearer, REST v2) is what auth.ValidateJira probes.
func jiraMyselfStub(t *testing.T, body string, gotAuth *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/2/myself" {
			http.NotFound(w, r)
			return
		}
		if gotAuth != nil {
			*gotAuth = r.Header.Get("Authorization")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestJiraIdentityPAT_StoresCredentialAndBindsIdentity drives the capture path
// against a stubbed DC host: the supplied PAT validates (GET /rest/api/2/myself
// with Bearer auth), the credential is STORED under "jira_token/<host>" (the
// Jira difference — GitHub discards), and the user's Jira identity is derived
// from the /myself response. The stored credential round-trips via SKY-442
// GetUserSystem.
func TestJiraIdentityPAT_StoresCredentialAndBindsIdentity(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit() // in-memory keychain — the sandbox has no dbus backend
	s := newTestServer(t)
	ctx := context.Background()

	var gotAuth string
	// Cloud-shaped: accountId present, so StableID() prefers it over key.
	jiraStub := jiraMyselfStub(t, `{"key":"jdoe","accountId":"acc-123","displayName":"Jane Doe"}`, &gotAuth)
	seedLocalOrgJiraHost(t, s, jiraStub.URL)

	rec := doJSON(t, s, "POST",
		"/api/orgs/"+runmode.LocalDefaultOrgID+"/identity/jira/pat",
		map[string]any{"pat": "jira_secret_token"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}

	var out jiraIdentityCaptureResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Account != "Jane Doe" {
		t.Errorf("account = %q, want Jane Doe", out.Account)
	}
	if out.Host != jiraStub.URL {
		t.Errorf("host = %q, want %q", out.Host, jiraStub.URL)
	}
	if gotAuth != "Bearer jira_secret_token" {
		t.Errorf("myself Authorization = %q, want the supplied PAT (Bearer)", gotAuth)
	}

	// The credential is STORED — round-trips via SKY-442 GetUserSystem under the
	// host-scoped key. This is the structural difference from GitHub (PAT_2),
	// which retains no credential.
	stored, err := s.secrets.GetUserSystem(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "jira_token/"+jiraStub.URL)
	if err != nil {
		t.Fatalf("GetUserSystem: %v", err)
	}
	if stored != "jira_secret_token" {
		t.Errorf("stored credential = %q, want the supplied PAT", stored)
	}

	// Identity is derived from the validated /myself response (accountId wins).
	accountID, displayName, err := s.users.GetJiraIdentity(ctx, runmode.LocalDefaultUserID)
	if err != nil {
		t.Fatalf("GetJiraIdentity: %v", err)
	}
	if accountID != "acc-123" || displayName != "Jane Doe" {
		t.Errorf("identity = (%q, %q), want (acc-123, Jane Doe)", accountID, displayName)
	}
}

// TestJiraIdentityStatus exercises the gate's status read: a fresh user reports
// connected=false; after a credential is stored, connected=true with the
// account + host. connect_available is always false (no Cloud OAuth yet).
func TestJiraIdentityStatus(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	jiraStub := jiraMyselfStub(t, `{"accountId":"acc-9","displayName":"Octo Jira"}`, nil)
	seedLocalOrgJiraHost(t, s, jiraStub.URL)

	get := func() jiraIdentityStatusResponse {
		rec := doJSON(t, s, "GET", "/api/orgs/"+runmode.LocalDefaultOrgID+"/identity/jira", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status endpoint=%d body=%s", rec.Code, rec.Body.String())
		}
		var out jiraIdentityStatusResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	if out := get(); out.Connected {
		t.Errorf("fresh user reported connected=true: %+v", out)
	}

	// Bind via the PAT endpoint; the status must then resume connected.
	rec := doJSON(t, s, "POST",
		"/api/orgs/"+runmode.LocalDefaultOrgID+"/identity/jira/pat",
		map[string]any{"pat": "tok"})
	if rec.Code != http.StatusOK {
		t.Fatalf("capture status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}

	out := get()
	if !out.Connected {
		t.Errorf("after capture, connected=false: %+v", out)
	}
	if out.Account != "Octo Jira" {
		t.Errorf("account = %q, want Octo Jira", out.Account)
	}
	if out.Host != jiraStub.URL {
		t.Errorf("host = %q, want %q", out.Host, jiraStub.URL)
	}
	if out.ConnectAvailable {
		t.Errorf("connect_available=true, want false (no Cloud OAuth yet): %+v", out)
	}
}

// TestJiraIdentityPAT_BadToken_Returns422 pins the "your action" failure shape:
// a token the host rejects is a 422 (not the infra 502), and no credential is
// stored.
func TestJiraIdentityPAT_BadToken_Returns422(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	ctx := context.Background()

	jiraStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Unauthorized"}`, http.StatusUnauthorized)
	}))
	t.Cleanup(jiraStub.Close)
	seedLocalOrgJiraHost(t, s, jiraStub.URL)

	rec := doJSON(t, s, "POST",
		"/api/orgs/"+runmode.LocalDefaultOrgID+"/identity/jira/pat",
		map[string]any{"pat": "bad"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s, want 422", rec.Code, rec.Body.String())
	}
	// A rejected token leaves no stored credential.
	stored, _ := s.secrets.GetUserSystem(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "jira_token/"+jiraStub.URL)
	if stored != "" {
		t.Errorf("credential stored on a bad token: %q", stored)
	}
}

// TestJiraIdentityPAT_HostUnreachable_Returns502 pins the infra failure shape:
// when TF can't reach the host, the response is a 502 (distinct from a bad
// token) and no credential is stored.
func TestJiraIdentityPAT_HostUnreachable_Returns502(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	ctx := context.Background()

	// Stand a stub up to claim a port, then close it so dials are refused.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	seedLocalOrgJiraHost(t, s, deadURL)

	rec := doJSON(t, s, "POST",
		"/api/orgs/"+runmode.LocalDefaultOrgID+"/identity/jira/pat",
		map[string]any{"pat": "whatever"})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s, want 502", rec.Code, rec.Body.String())
	}
	stored, _ := s.secrets.GetUserSystem(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "jira_token/"+deadURL)
	if stored != "" {
		t.Errorf("credential stored despite unreachable host: %q", stored)
	}
}

// TestJiraIdentityPAT_EmptyToken_Returns400 pins that an empty PAT is rejected
// before any host round-trip (the empty check precedes host resolution).
func TestJiraIdentityPAT_EmptyToken_Returns400(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	rec := doJSON(t, s, "POST",
		"/api/orgs/"+runmode.LocalDefaultOrgID+"/identity/jira/pat",
		map[string]any{"pat": "   "})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
}

// TestJiraIdentityPAT_NoJiraHost_Returns422 pins that capturing before Jira is
// configured (no jira_base_url) is a 422 with the "ask your admin" shape, not a
// host round-trip against an empty URL.
func TestJiraIdentityPAT_NoJiraHost_Returns422(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	// Deliberately do NOT seed a Jira host.

	rec := doJSON(t, s, "POST",
		"/api/orgs/"+runmode.LocalDefaultOrgID+"/identity/jira/pat",
		map[string]any{"pat": "tok"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s, want 422", rec.Code, rec.Body.String())
	}
}
