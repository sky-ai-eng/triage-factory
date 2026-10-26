package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/jiraoauth"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/zalando/go-keyring"
)

const jiraOAuthSite = "https://acme.atlassian.net"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// fakeAtlassian answers the three Atlassian calls the Connect callback makes:
// the code exchange, accessible-resources, and /myself on the api.atlassian.com
// gateway. The minter is pointed at it directly; the gateway URL is fixed, so
// requests to api.atlassian.com through the default transport are routed to it
// for the test's duration.
func fakeAtlassian(t *testing.T, s *Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth/token":
			fmt.Fprint(w, `{"access_token":"acc-1","refresh_token":"ref-1","expires_in":3600}`)
		case "/oauth/token/accessible-resources":
			fmt.Fprintf(w, `[{"id":"cloud-1","url":%q,"name":"Acme"}]`, jiraOAuthSite)
		case "/ex/jira/cloud-1/rest/api/3/myself":
			fmt.Fprint(w, `{"accountId":"acc-oauth","displayName":"OAuth User"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	s.jiraOAuthMinter = jiraoauth.NewMinterWithEndpoints(srv.URL+"/oauth/token", srv.URL+"/oauth/token/accessible-resources")

	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse fake atlassian url: %v", err)
	}
	orig := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "api.atlassian.com" {
			r = r.Clone(r.Context())
			r.URL.Scheme, r.URL.Host, r.Host = target.Scheme, target.Host, ""
		}
		return orig.RoundTrip(r)
	})
	t.Cleanup(func() { http.DefaultTransport = orig })
}

// saveJiraApp stores the org's own Atlassian app, as an admin does.
func saveJiraApp(t *testing.T, s *Server, clientID string) {
	t.Helper()
	rec := doJSON(t, s, http.MethodPost, jiraAppPath(), map[string]string{"client_id": clientID, "client_secret": clientID + "-secret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("save jira app: %d: %s", rec.Code, rec.Body.String())
	}
}

func jiraIdentityStatus(t *testing.T, s *Server) jiraIdentityStatusResponse {
	t.Helper()
	rec := doJSON(t, s, http.MethodGet, "/api/orgs/"+runmode.LocalDefaultOrgID+"/jira/identity", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("jira identity status: %d: %s", rec.Code, rec.Body.String())
	}
	var out jiraIdentityStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func storedJiraUserCredential(t *testing.T, s *Server) jira.UserCredential {
	t.Helper()
	raw, err := s.secrets.GetUser(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, jiraTokenKey(jiraOAuthSite))
	if err != nil || raw == "" {
		t.Fatalf("read stored credential = (%q, %v), want one", raw, err)
	}
	cred, err := jira.ParseUserCredential(raw)
	if err != nil {
		t.Fatalf("parse stored credential: %v", err)
	}
	return cred
}

func seedJiraOAuthCredential(t *testing.T, s *Server, clientID string) {
	t.Helper()
	env, err := jira.MarshalUserCredential(jira.UserCredential{
		Method:       jira.AuthMethodCloudOAuth,
		CloudID:      "cloud-1",
		RefreshToken: "ref-0",
		ClientID:     clientID,
	})
	if err != nil {
		t.Fatalf("marshal credential: %v", err)
	}
	if err := s.secrets.PutUser(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, jiraTokenKey(jiraOAuthSite), env, ""); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
}

func newJiraOAuthOrg(t *testing.T) *Server {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	s.SetDeployConfig("http://localhost:3000", [32]byte{})
	seedLocalOrgJiraHost(t, s, jiraOAuthSite)
	seedLocalOrgJiraAuthMethod(t, s, string(jira.AuthMethodCloudOAuth))
	return s
}

// TestJiraConnectCallback_StoresTheIssuingApp: the credential a Connect stores
// names the app the code was exchanged with, the only app that can refresh its
// token.
func TestJiraConnectCallback_StoresTheIssuingApp(t *testing.T) {
	s := newJiraOAuthOrg(t)
	fakeAtlassian(t, s)
	saveJiraApp(t, s, "atl-client")

	start := doJSON(t, s, http.MethodGet, "/api/orgs/"+runmode.LocalDefaultOrgID+"/jira/connect/start", nil)
	if start.Code != http.StatusFound {
		t.Fatalf("start: %d: %s", start.Code, start.Body.String())
	}
	loc, err := url.Parse(start.Header().Get("Location"))
	if err != nil || loc.Host != "auth.atlassian.com" {
		t.Fatalf("start redirected to %q (%v), want Atlassian's authorize page", start.Header().Get("Location"), err)
	}
	var stateCookie *http.Cookie
	for _, c := range start.Result().Cookies() {
		if c.Name == jiraConnectStateCookieName {
			stateCookie = c
		}
	}
	if stateCookie == nil {
		t.Fatal("start set no state cookie")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/orgs/"+runmode.LocalDefaultOrgID+"/jira/connect/callback?"+
		url.Values{"code": {"good-code"}, "state": {loc.Query().Get("state")}}.Encode(), nil)
	req.AddCookie(stateCookie)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/orgs/"+runmode.LocalDefaultOrgID {
		t.Fatalf("callback = %d to %q, want a redirect back to the org", rec.Code, rec.Header().Get("Location"))
	}

	cred := storedJiraUserCredential(t, s)
	if cred.Method != jira.AuthMethodCloudOAuth || cred.RefreshToken != "ref-1" || cred.ClientID != "atl-client" {
		t.Errorf("stored credential = %+v, want ref-1 issued by atl-client", cred)
	}
}

// TestJiraIdentityStatus_IssuingAppRemoved_NotConnected: a Cloud OAuth
// credential whose app the org can no longer authenticate as cannot be
// refreshed, so it reports not connected and the user gets the Connect button.
// Configuring the same app again brings it back.
func TestJiraIdentityStatus_IssuingAppRemoved_NotConnected(t *testing.T) {
	s := newJiraOAuthOrg(t)
	saveJiraApp(t, s, "atl-client")
	seedJiraOAuthCredential(t, s, "atl-client")
	if err := s.users.UpsertJiraIdentity(context.Background(), runmode.LocalDefaultUserID, jiraOAuthSite, "acc-oauth", "OAuth User", "connect_oauth"); err != nil {
		t.Fatalf("seed identity: %v", err)
	}

	if got := jiraIdentityStatus(t, s); !got.Connected || got.Account != "OAuth User" {
		t.Fatalf("status with the issuing app configured = %+v, want connected as OAuth User", got)
	}

	saveJiraApp(t, s, "other-client")
	got := jiraIdentityStatus(t, s)
	if got.Connected || got.Account != "" {
		t.Errorf("status after the app was replaced = %+v, want not connected and no account", got)
	}
	if !got.ConnectAvailable {
		t.Errorf("status after the app was replaced = %+v, want Connect available through the new app", got)
	}

	saveJiraApp(t, s, "atl-client")
	if got := jiraIdentityStatus(t, s); !got.Connected {
		t.Errorf("status after the app was configured again = %+v, want connected", got)
	}
}

// TestJiraIdentityStatus_UnattributedOAuthCredential_Connected: a credential
// that does not record its app reports as it always has, even with no app
// configured; its next refresh is what finds out.
func TestJiraIdentityStatus_UnattributedOAuthCredential_Connected(t *testing.T) {
	s := newJiraOAuthOrg(t)
	seedJiraOAuthCredential(t, s, "")

	if got := jiraIdentityStatus(t, s); !got.Connected {
		t.Errorf("status = %+v, want connected", got)
	}
}
