package slack

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server"
)

// fakeCeremony stands in for the ExtensionAPI's install ceremony primitives.
// A signed state is an opaque handle the fake issued; anything else is forged.
type fakeCeremony struct {
	publicURL string

	mu     sync.Mutex
	n      int
	states map[string]fakeSignedState
	ttls   []time.Duration
	barred map[string]bool
}

type fakeSignedState struct {
	st      server.ConnectState
	expires time.Time
}

func newFakeCeremony(publicURL string) *fakeCeremony {
	return &fakeCeremony{publicURL: publicURL, states: map[string]fakeSignedState{}, barred: map[string]bool{}}
}

func (c *fakeCeremony) issue(st server.ConnectState, expires time.Time) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	signed := fmt.Sprintf("signed-%d", c.n)
	c.states[signed] = fakeSignedState{st: st, expires: expires}
	return signed
}

func (c *fakeCeremony) SignConnectState(orgID, userID, returnTo string, ttl time.Duration) (string, server.ConnectState, error) {
	c.mu.Lock()
	c.ttls = append(c.ttls, ttl)
	csrf := fmt.Sprintf("csrf-%d", len(c.ttls))
	c.mu.Unlock()
	st := server.ConnectState{OrgID: orgID, UserID: userID, CSRF: csrf, ReturnTo: returnTo}
	return c.issue(st, time.Now().Add(ttl)), st, nil
}

func (c *fakeCeremony) ParseConnectState(signed string) (server.ConnectState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.states[signed]
	if !ok {
		return server.ConnectState{}, errors.New("mac mismatch")
	}
	if time.Now().After(s.expires) {
		return server.ConnectState{}, errors.New("expired")
	}
	return s.st, nil
}

func (c *fakeCeremony) CookieSecure(*http.Request) bool { return true }

func (c *fakeCeremony) SSOBarred(_ context.Context, orgID string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.barred[orgID], nil
}

func (c *fakeCeremony) SettingsPath(orgID string) string { return "/orgs/" + orgID + "/settings" }

func (c *fakeCeremony) NormalizeReturnTo(raw string) string {
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return "/"
	}
	return raw
}

func (c *fakeCeremony) PublicURL() string { return c.publicURL }

// TestInstall_Unavailable404: in local mode, or behind a public URL that is
// not HTTPS, neither leg exists.
func TestInstall_Unavailable404(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mode      runmode.Mode
		publicURL string
	}{
		{"local mode", runmode.ModeLocal, "https://tf.example"},
		{"http public URL", runmode.ModeMulti, "http://tf.example"},
		{"no public URL", runmode.ModeMulti, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runmode.SetForTest(t, tc.mode)
			h := &installHandler{app: testDeploymentApp(), ceremony: newFakeCeremony(tc.publicURL)}
			for _, leg := range []struct {
				path string
				fn   http.HandlerFunc
			}{
				{"/api/orgs/00000000-0000-0000-0000-000000000001/slack/install/start", h.handleStart},
				{"/api/slack/install/callback?code=c&state=s", h.handleCallback},
			} {
				rec := httptest.NewRecorder()
				leg.fn(rec, httptest.NewRequest(http.MethodGet, leg.path, nil))
				if rec.Code != http.StatusNotFound {
					t.Errorf("%s: status = %d, want 404", leg.path, rec.Code)
				}
				if got := rec.Header().Values("Set-Cookie"); len(got) != 0 {
					t.Errorf("%s: Set-Cookie = %v, want none", leg.path, got)
				}
			}
		})
	}
}

// TestSlackOAuthV2Access: the exchange authenticates with HTTP Basic, sends
// the code and the redirect URI, and reads the install; ok:false and an ok
// answer with no token are errors.
func TestSlackOAuthV2Access(t *testing.T) {
	fake := newFakeSlackServer(t)
	fake.oauthAccess["good"] = map[string]any{
		"ok": true, "app_id": "A0DEPLOY01", "access_token": "xoxb-new", "token_type": "bot",
		"scope": "chat:write,users:read", "bot_user_id": "U0BOT",
		"team":       map[string]any{"id": "T0ACME", "name": "Acme"},
		"enterprise": map[string]any{"id": "E0GRID", "name": "Grid"},
	}
	fake.oauthAccess["grid"] = map[string]any{
		"ok": true, "app_id": "A0DEPLOY01", "access_token": "xoxb-grid", "token_type": "bot",
		"team": nil, "enterprise": map[string]any{"id": "E0GRID"}, "is_enterprise_install": true,
	}
	fake.oauthAccess["tokenless"] = map[string]any{"ok": true, "app_id": "A0DEPLOY01"}

	got, err := slackOAuthV2Access(t.Context(), slackHTTPClient, slackCaller{orgID: "org"}, "111.222", "s3cret", "good", "https://tf.example/api/slack/install/callback")
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	want := &oauthV2AccessResult{
		AppID: "A0DEPLOY01", AccessToken: "xoxb-new", TokenType: "bot", Scope: "chat:write,users:read",
		BotUserID: "U0BOT", TeamID: "T0ACME", TeamName: "Acme", EnterpriseID: "E0GRID",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("result = %+v, want %+v", got, want)
	}
	req := fake.oauthRequests[0]
	if !req.BasicAuth || req.ClientID != "111.222" || req.ClientSecret != "s3cret" {
		t.Errorf("auth = basic %v, %q:%q; want basic 111.222:s3cret", req.BasicAuth, req.ClientID, req.ClientSecret)
	}
	if req.Form.Get("code") != "good" || req.Form.Get("redirect_uri") != "https://tf.example/api/slack/install/callback" ||
		req.Form.Has("client_id") || req.Form.Has("client_secret") {
		t.Errorf("form = %v; want code and redirect_uri only", req.Form)
	}

	grid, err := slackOAuthV2Access(t.Context(), slackHTTPClient, slackCaller{}, "id", "secret", "grid", "u")
	if err != nil {
		t.Fatalf("grid exchange: %v", err)
	}
	if !grid.IsEnterpriseInstall || grid.TeamID != "" || grid.EnterpriseID != "E0GRID" {
		t.Errorf("grid result = %+v; want an enterprise install with no team", grid)
	}

	if _, err := slackOAuthV2Access(t.Context(), slackHTTPClient, slackCaller{}, "id", "secret", "unstaged", "u"); err == nil || !strings.Contains(err.Error(), "invalid_code") {
		t.Errorf("ok:false err = %v; want one naming invalid_code", err)
	}
	if _, err := slackOAuthV2Access(t.Context(), slackHTTPClient, slackCaller{}, "id", "secret", "tokenless", "u"); err == nil {
		t.Error("an ok answer with no access_token was accepted")
	}
}

func TestMissingBotScopes(t *testing.T) {
	all := strings.Join(slackBotScopes, ",")
	if got := missingBotScopes(all); len(got) != 0 {
		t.Errorf("every scope granted: missing = %v, want none", got)
	}
	if got := missingBotScopes("commands," + strings.ReplaceAll(all, ",", ", ")); len(got) != 0 {
		t.Errorf("every scope with spaces and an extra: missing = %v, want none", got)
	}
	if got := missingBotScopes(strings.Replace(all, "files:write,", "", 1)); !reflect.DeepEqual(got, []string{"files:write"}) {
		t.Errorf("files:write absent: missing = %v, want [files:write]", got)
	}
	if got := missingBotScopes(""); len(got) != len(slackBotScopes) {
		t.Errorf("nothing granted: missing = %v, want all %d", got, len(slackBotScopes))
	}
}

func testDeploymentApp() deploymentApp {
	return deploymentApp{
		AppID:         "A0DEPLOY01",
		ClientID:      "111.222",
		ClientSecret:  "client-secret",
		SigningSecret: "signing-secret",
		Transport:     transportEventsAPI,
	}
}
