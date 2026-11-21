package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
	"github.com/zalando/go-keyring"
)

// TestResolveJiraHost pins the canonical host the per-user credential is keyed
// under, so the status reader and the PAT writer compose the same key. An empty
// or non-http base has no host, and there is no default.
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
		_, err = tx.Orgs.UpdateSettingsVersioned(ctx, runmode.LocalDefaultOrgID, set, set.Version)
		return err
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

// jiraCloudMyselfStub is the Cloud sibling of jiraMyselfStub: it answers the
// REST v3 /myself a Cloud API token (Basic) probes, capturing the Authorization
// header sent.
func jiraCloudMyselfStub(t *testing.T, body string, gotAuth *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/3/myself" {
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

// seedLocalOrgJiraAuthMethod writes the org's Jira auth-method marker (the value
// the capture + status handlers resolve the deployment from). Used to drive the
// Cloud branch against a local stub, where the host shape alone would classify
// as Data Center.
func seedLocalOrgJiraAuthMethod(t *testing.T, s *Server, method string) {
	t.Helper()
	if err := s.secrets.Put(context.Background(), runmode.LocalDefaultOrgID, integrations.KeyJiraAuthMethod, method, ""); err != nil {
		t.Fatalf("seed org jira auth method: %v", err)
	}
}

// jiraIdentityAPITokenRoute and jiraIdentityPATRoute are the local org's two
// paste routes: Cloud's email + API token, and Data Center's PAT.
var (
	jiraIdentityAPITokenRoute = "/api/orgs/" + runmode.LocalDefaultOrgID + "/jira/identity/api-token"
	jiraIdentityPATRoute      = "/api/orgs/" + runmode.LocalDefaultOrgID + "/jira/identity/pat"
)

// TestJiraIdentityAPIToken_StoresEnvelopeAndBindsIdentity: on a cloud_api_token
// org the email and API token are validated with Basic auth against REST v3
// /myself, stored as a cloud_api_token envelope, and bind the identity it names.
func TestJiraIdentityAPIToken_StoresEnvelopeAndBindsIdentity(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	ctx := context.Background()

	var gotAuth string
	jiraStub := jiraCloudMyselfStub(t, `{"accountId":"acc-cloud","displayName":"Cloud User"}`, &gotAuth)
	seedLocalOrgJiraHost(t, s, jiraStub.URL)
	seedLocalOrgJiraAuthMethod(t, s, "cloud_api_token")

	rec := doJSON(t, s, "POST", jiraIdentityAPITokenRoute,
		map[string]any{"email": "me@acme.com", "token": "cloud-tok"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}

	var out jiraIdentityCaptureResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Account != "Cloud User" {
		t.Errorf("account = %q, want Cloud User", out.Account)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("me@acme.com:cloud-tok"))
	if gotAuth != wantAuth {
		t.Errorf("myself Authorization = %q, want %q (Basic email:token)", gotAuth, wantAuth)
	}

	stored, err := s.secrets.GetUserSystem(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "jira_token/"+jiraStub.URL)
	if err != nil {
		t.Fatalf("GetUserSystem: %v", err)
	}
	cred, err := jira.ParseUserCredential(stored)
	if err != nil {
		t.Fatalf("ParseUserCredential(%q): %v", stored, err)
	}
	if cred.Method != jira.AuthMethodCloudAPIToken || cred.Email != "me@acme.com" || cred.Token != "cloud-tok" {
		t.Errorf("stored credential = %+v, want a cloud_api_token envelope", cred)
	}

	accountID, displayName, err := s.users.GetJiraIdentity(ctx, runmode.LocalDefaultUserID, jiraStub.URL)
	if err != nil {
		t.Fatalf("GetJiraIdentity: %v", err)
	}
	if accountID != "acc-cloud" || displayName != "Cloud User" {
		t.Errorf("identity = (%q, %q), want (acc-cloud, Cloud User)", accountID, displayName)
	}
}

// TestJiraIdentityAPIToken_MissingHalf_Returns400 pins that a Cloud org bind
// missing EITHER half of the email + token pair is rejected before any host
// round-trip (both halves are required, symmetrically), naming the half that
// is missing, or email first when both are.
func TestJiraIdentityAPIToken_MissingHalf_Returns400(t *testing.T) {
	cases := map[string]struct {
		body  map[string]any
		field string
	}{
		"no token":    {map[string]any{"email": "me@acme.com"}, "token"},
		"blank token": {map[string]any{"email": "me@acme.com", "token": "  "}, "token"},
		"no email":    {map[string]any{"token": "cloud-tok"}, "email"},
		"empty":       {map[string]any{}, "email"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeLocal)
			keyring.MockInit()
			s := newTestServer(t)
			seedLocalOrgJiraHost(t, s, "https://acme.atlassian.net")
			seedLocalOrgJiraAuthMethod(t, s, "cloud_api_token")

			rec := doJSON(t, s, "POST", jiraIdentityAPITokenRoute, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
			}
			httpxtest.AssertFirstError(t, rec, httpx.ReasonMissingField, tc.field)
		})
	}
}

// TestJiraIdentity_OtherDeploymentsRoute_Returns409: each paste route takes
// the credential of one deployment, so a call to the route for the deployment
// the org is not on is a 409 CONFLICT naming the route it belongs on, and no
// credential is stored. That holds whichever body the call carries: the
// route's own, or the other deployment's, which is what a caller sends when it
// picked the wrong route.
func TestJiraIdentity_OtherDeploymentsRoute_Returns409(t *testing.T) {
	for _, tc := range []struct {
		name, method, host, route, wantRoute string
		body                                 map[string]any
	}{
		{"pat on a Cloud org", "cloud_api_token", "https://acme.atlassian.net", jiraIdentityPATRoute, "/jira/identity/api-token",
			map[string]any{"pat": "tok"}},
		{"email and token to the pat route on a Cloud org", "cloud_api_token", "https://acme.atlassian.net", jiraIdentityPATRoute, "/jira/identity/api-token",
			map[string]any{"email": "me@acme.com", "token": "cloud-tok"}},
		{"api token on a Data Center org", "dc_pat", "https://jira.corp.example.com", jiraIdentityAPITokenRoute, "/jira/identity/pat",
			map[string]any{"email": "me@acme.com", "token": "cloud-tok"}},
		{"pat to the api-token route on a Data Center org", "dc_pat", "https://jira.corp.example.com", jiraIdentityAPITokenRoute, "/jira/identity/pat",
			map[string]any{"pat": "tok"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeLocal)
			keyring.MockInit()
			s := newTestServer(t)
			seedLocalOrgJiraHost(t, s, tc.host)
			seedLocalOrgJiraAuthMethod(t, s, tc.method)

			rec := doJSON(t, s, "POST", tc.route, tc.body)
			if rec.Code != http.StatusConflict {
				t.Fatalf("status=%d body=%s, want 409", rec.Code, rec.Body.String())
			}
			httpxtest.AssertFirstError(t, rec, httpx.ReasonConflict, "")
			if !strings.Contains(rec.Body.String(), tc.wantRoute) {
				t.Errorf("body=%s, want a message naming %s", rec.Body.String(), tc.wantRoute)
			}
			if stored, _ := s.secrets.GetUserSystem(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "jira_token/"+tc.host); stored != "" {
				t.Errorf("credential stored by a refused bind: %q", stored)
			}
		})
	}
}

// TestJiraIdentityPAT_StoresCredentialAndBindsIdentity: a Data Center PAT is
// validated with Bearer auth against REST v2 /myself, stored as a dc_pat envelope,
// and binds the identity it names (accountId over key); the status reads connected.
func TestJiraIdentityPAT_StoresCredentialAndBindsIdentity(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	ctx := context.Background()

	var gotAuth string
	jiraStub := jiraMyselfStub(t, `{"key":"jdoe","accountId":"acc-123","displayName":"Jane Doe"}`, &gotAuth)
	seedLocalOrgJiraHost(t, s, jiraStub.URL)

	rec := doJSON(t, s, "POST", jiraIdentityPATRoute,
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

	stored, err := s.secrets.GetUserSystem(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "jira_token/"+jiraStub.URL)
	if err != nil {
		t.Fatalf("GetUserSystem: %v", err)
	}
	cred, err := jira.ParseUserCredential(stored)
	if err != nil {
		t.Fatalf("ParseUserCredential(%q): %v", stored, err)
	}
	if cred.Method != jira.AuthMethodDCPAT || cred.Token != "jira_secret_token" {
		t.Errorf("stored credential = %+v, want dc_pat envelope with the supplied PAT", cred)
	}

	accountID, displayName, err := s.users.GetJiraIdentity(ctx, runmode.LocalDefaultUserID, jiraStub.URL)
	if err != nil {
		t.Fatalf("GetJiraIdentity: %v", err)
	}
	if accountID != "acc-123" || displayName != "Jane Doe" {
		t.Errorf("identity = (%q, %q), want (acc-123, Jane Doe)", accountID, displayName)
	}

	// No auth-method marker and a non-*.atlassian.net host resolve Data
	// Center, and the org has no Atlassian OAuth app to offer Connect through.
	st := jiraIdentityStatus(t, s)
	if !st.Connected || st.Account != "Jane Doe" || st.Host != jiraStub.URL || st.Deployment != "data_center" || st.ConnectAvailable {
		t.Errorf("status after the bind = %+v, want connected as Jane Doe on the Data Center host with no Connect", st)
	}
}

// TestJiraIdentityStatus_StaleCrossSchemeCredential_NotConnected: a stored Data
// Center PAT on an org that has since moved to Cloud reports not connected, so
// the paste UI asks for a re-bind, and the deployment follows the marker.
func TestJiraIdentityStatus_StaleCrossSchemeCredential_NotConnected(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	// No marker yet, so the host shape resolves Data Center.
	jiraStub := jiraMyselfStub(t, `{"accountId":"acc-9","displayName":"Octo Jira"}`, nil)
	seedLocalOrgJiraHost(t, s, jiraStub.URL)
	rec := doJSON(t, s, "POST", jiraIdentityPATRoute,
		map[string]any{"pat": "tok"})
	if rec.Code != http.StatusOK {
		t.Fatalf("capture status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}

	seedLocalOrgJiraAuthMethod(t, s, "cloud_api_token")

	out := jiraIdentityStatus(t, s)
	if out.Deployment != "cloud" {
		t.Errorf("deployment = %q, want cloud (marker authoritative)", out.Deployment)
	}
	if out.Connected {
		t.Errorf("connected=true for a DC PAT on a Cloud org: %+v (want false — stale cross-scheme cred)", out)
	}
	if out.DisconnectedReason != "" {
		t.Errorf("disconnected_reason = %q, want none for a credential from the other deployment", out.DisconnectedReason)
	}
}

// TestJiraIdentityStatus_ConfiguredHostNoCredential: with a host configured and
// no credential bound, the status reports not connected under that host, with
// no account and no reason. The host is never contacted.
func TestJiraIdentityStatus_ConfiguredHostNoCredential(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	const host = "https://jira.corp.example.com"
	seedLocalOrgJiraHost(t, s, host)

	out := jiraIdentityStatus(t, s)
	if out.Connected {
		t.Errorf("connected=true with no stored credential: %+v", out)
	}
	if out.Host != host {
		t.Errorf("host = %q, want the configured host %q", out.Host, host)
	}
	if out.Account != "" {
		t.Errorf("account = %q, want empty when not connected", out.Account)
	}
	if out.ConnectAvailable {
		t.Errorf("connect_available=true, want false: %+v", out)
	}
	// Never connected is not a lost connection, so there is nothing to explain.
	if out.DisconnectedReason != "" {
		t.Errorf("disconnected_reason = %q, want none for a user who never connected", out.DisconnectedReason)
	}
}

// TestJiraIdentityStatus_NoJiraConfigured_EmptyHost: with no jira_base_url the
// status is a 200 with host "" and not connected, not an error, so the
// RequireJiraIdentity gate can tell a GitHub-only org from a failed read.
func TestJiraIdentityStatus_NoJiraConfigured_EmptyHost(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	out := jiraIdentityStatus(t, s)
	if out.Host != "" {
		t.Errorf("host = %q, want empty for a GitHub-only org (no jira_base_url)", out.Host)
	}
	if out.Connected {
		t.Errorf("connected=true with no Jira configured: %+v", out)
	}
}

// TestJiraIdentityPAT_RefusalsStoreNothing: a token the host rejects, or one
// that authenticates but names no account, is the caller's to fix (422); a
// host TF cannot reach is not (502). None of them stores a credential.
func TestJiraIdentityPAT_RefusalsStoreNothing(t *testing.T) {
	for _, tc := range []struct {
		name       string
		host       func(t *testing.T) string
		want       int
		wantReason string
		wantField  string
	}{
		{"bad token", func(t *testing.T) string {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, `{"message":"Unauthorized"}`, http.StatusUnauthorized)
			}))
			t.Cleanup(srv.Close)
			return srv.URL
		}, http.StatusUnprocessableEntity, httpx.ReasonUpstreamRejected, "pat"},
		{"no account", func(t *testing.T) string { return jiraMyselfStub(t, `{}`, nil).URL }, http.StatusUnprocessableEntity, httpx.ReasonUpstreamRejected, "pat"},
		{"host unreachable", func(t *testing.T) string {
			// A closed stub's port refuses every dial.
			dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			dead.Close()
			return dead.URL
		}, http.StatusBadGateway, httpx.ReasonUpstreamUnavailable, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeLocal)
			keyring.MockInit()
			s := newTestServer(t)
			host := tc.host(t)
			seedLocalOrgJiraHost(t, s, host)

			rec := doJSON(t, s, "POST", jiraIdentityPATRoute,
				map[string]any{"pat": "tok"})
			if rec.Code != tc.want {
				t.Fatalf("status=%d body=%s, want %d", rec.Code, rec.Body.String(), tc.want)
			}
			httpxtest.AssertFirstError(t, rec, tc.wantReason, tc.wantField)
			if stored, _ := s.secrets.GetUserSystem(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "jira_token/"+host); stored != "" {
				t.Errorf("credential stored on a refused bind: %q", stored)
			}
		})
	}
}

// TestJiraIdentityAPIToken_Rejected_NamesNoField: Jira refuses an email and
// API token pair without saying which half is wrong, so the 422 names neither
// field, and nothing is stored.
func TestJiraIdentityAPIToken_Rejected_NamesNoField(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Unauthorized"}`, http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	seedLocalOrgJiraHost(t, s, srv.URL)
	seedLocalOrgJiraAuthMethod(t, s, "cloud_api_token")

	rec := doJSON(t, s, "POST", jiraIdentityAPITokenRoute,
		map[string]any{"email": "me@acme.com", "token": "cloud-tok"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s, want 422", rec.Code, rec.Body.String())
	}
	httpxtest.AssertFirstError(t, rec, httpx.ReasonUpstreamRejected, "")
	if stored, _ := s.secrets.GetUserSystem(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "jira_token/"+srv.URL); stored != "" {
		t.Errorf("credential stored on a refused bind: %q", stored)
	}
}

// TestJiraIdentityPAT_EmptyToken_Returns400: a blank PAT for a Data Center org
// is rejected before any round-trip to the host.
func TestJiraIdentityPAT_EmptyToken_Returns400(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	// A DC-shaped host (not *.atlassian.net) → DC branch, which expects a PAT.
	seedLocalOrgJiraHost(t, s, "https://jira.example.com")

	rec := doJSON(t, s, "POST", jiraIdentityPATRoute,
		map[string]any{"pat": "   "})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
	httpxtest.AssertFirstError(t, rec, httpx.ReasonMissingField, "pat")
}

// TestJiraIdentityPAT_NoJiraHost_ReturnsNotConfigured: a bind before the org has
// a jira_base_url is a 409 NOT_CONFIGURED, which only an admin can fix.
func TestJiraIdentityPAT_NoJiraHost_ReturnsNotConfigured(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	rec := doJSON(t, s, "POST", jiraIdentityPATRoute,
		map[string]any{"pat": "tok"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	httpxtest.AssertFirstError(t, rec, httpx.ReasonNotConfigured, "")

	rec = doJSON(t, s, "POST", jiraIdentityAPITokenRoute,
		map[string]any{"email": "me@acme.com", "token": "cloud-tok"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("api-token status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	httpxtest.AssertFirstError(t, rec, httpx.ReasonNotConfigured, "")
}

// TestJiraConnectStart_DataCenterOrg_BouncesToPaste: a Data Center org has no
// Atlassian OAuth app to authorize against, so Connect start redirects back to
// the paste page with error=not_cloud instead of to auth.atlassian.com.
func TestJiraConnectStart_DataCenterOrg_BouncesToPaste(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	s.SetDeployConfig("http://localhost:3000", [32]byte{})

	// A well-formed DC host + the dc_pat marker → DeploymentDataCenter.
	seedLocalOrgJiraHost(t, s, "https://jira.corp.example.com")
	seedLocalOrgJiraAuthMethod(t, s, "dc_pat")

	rec := doJSON(t, s, "GET", "/api/orgs/"+runmode.LocalDefaultOrgID+"/jira/connect/start", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d body=%s, want 302", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if strings.Contains(loc, "auth.atlassian.com") {
		t.Fatalf("a DC org was redirected into Atlassian OAuth: %q", loc)
	}
	if !strings.Contains(loc, "/connect-jira") || !strings.Contains(loc, "error=not_cloud") {
		t.Errorf("Location=%q, want a bounce to /connect-jira with error=not_cloud", loc)
	}
}
