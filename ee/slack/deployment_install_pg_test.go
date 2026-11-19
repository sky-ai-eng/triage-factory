package slack

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/server"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// ---------- rig ----------

type installRig struct {
	*slackWorkspaceRig
	ceremony *fakeCeremony
	install  *installHandler
	metrics  *sdkmetric.ManualReader
}

// newInstallRig is the workspace rig plus the install handler, with the
// deployment app configured and an HTTPS public URL. Every test using it
// fails if anything called auth.revoke.
func newInstallRig(t *testing.T) *installRig {
	t.Helper()
	base := newSlackWorkspaceRig(t, "https://tf.example")
	c := newFakeCeremony("https://tf.example")
	stats, metrics := newTestStats()
	r := &installRig{
		slackWorkspaceRig: base,
		ceremony:          c,
		install: &installHandler{
			tx:       base.stores.Tx,
			az:       base.hdl.az,
			client:   slackHTTPClient,
			app:      testDeploymentApp(),
			ceremony: c,
			stats:    stats,
		},
		metrics: metrics,
	}
	t.Cleanup(func() {
		if base.fake.revokeHits != 0 {
			t.Errorf("auth.revoke called %d times; no path may revoke", base.fake.revokeHits)
		}
	})
	return r
}

// installOutcomes is slack.install.attempts so far, by outcome.
func (r *installRig) installOutcomes() map[string]int64 {
	r.t.Helper()
	return sumByAttr(r.t, r.metrics, "slack.install.attempts", "outcome")
}

type ctxOpt func(context.Context) context.Context

func withAPIToken(orgID string) ctxOpt {
	return func(ctx context.Context) context.Context {
		return httpx.WithTokenAuth(ctx, &httpx.TokenAuth{TokenID: "tok-1", OrgID: orgID})
	}
}

func (r *installRig) start(callerID, orgID string, query url.Values, opts ...ctxOpt) *httptest.ResponseRecorder {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/orgs/"+orgID+"/slack/install/start?"+query.Encode(), nil)
	req.SetPathValue("org_id", orgID)
	ctx := httpx.WithClaims(req.Context(), &verify.Claims{Subject: callerID})
	for _, o := range opts {
		ctx = o(ctx)
	}
	rec := httptest.NewRecorder()
	r.install.handleStart(rec, req.WithContext(ctx))
	return rec
}

// callback visits the callback as callerID (no session when ""), with the
// session's active org sessionOrg, the state cookie when cookie is not "",
// and query.
func (r *installRig) callback(callerID, sessionOrg, cookie string, query url.Values, opts ...ctxOpt) *httptest.ResponseRecorder {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodGet, installCallbackPath+"?"+query.Encode(), nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: installStateCookieName, Value: cookie})
	}
	ctx := req.Context()
	if callerID != "" {
		ctx = httpx.WithClaims(ctx, &verify.Claims{Subject: callerID})
	}
	if sessionOrg != "" {
		ctx = httpx.WithOrgID(ctx, sessionOrg)
	}
	for _, o := range opts {
		ctx = o(ctx)
	}
	rec := httptest.NewRecorder()
	r.install.handleCallback(rec, req.WithContext(ctx))
	return rec
}

// issue mints a live state for (orgID, userID, returnTo), as the start leg
// would, and returns the cookie and the CSRF value Slack echoes.
func (r *installRig) issue(orgID, userID, returnTo string) (cookie, csrf string) {
	r.t.Helper()
	signed, st, err := r.ceremony.SignConnectState(orgID, userID, returnTo, installStateTTL)
	if err != nil {
		r.t.Fatalf("sign state: %v", err)
	}
	return signed, st.CSRF
}

// stageInstall stages a successful exchange of code into workspace teamID
// with bot token botToken, and the auth.test and bots.info answers that name
// the bot botName.
func (r *installRig) stageInstall(code, teamID, teamName, botToken, botName string) map[string]any {
	r.t.Helper()
	resp := map[string]any{
		"ok":                    true,
		"app_id":                testDeploymentApp().AppID,
		"access_token":          botToken,
		"token_type":            "bot",
		"scope":                 strings.Join(slackBotScopes, ","),
		"bot_user_id":           "U0BOT" + teamID,
		"team":                  map[string]any{"id": teamID, "name": teamName},
		"enterprise":            nil,
		"is_enterprise_install": false,
	}
	r.fake.mu.Lock()
	r.fake.oauthAccess[code] = resp
	r.fake.mu.Unlock()
	r.fake.stageConnectOK(botToken, teamID, teamName, "U0BOT"+teamID, testDeploymentApp().AppID, "")
	r.fake.stageBotName(botToken, botName)
	return resp
}

// installAs runs a whole ceremony for callerID into orgID, through the start
// leg, with Slack answering code.
func (r *installRig) installAs(callerID, orgID, code string) *httptest.ResponseRecorder {
	r.t.Helper()
	start := r.start(callerID, orgID, url.Values{})
	if start.Code != http.StatusFound {
		r.t.Fatalf("start status = %d, want 302 (body=%s)", start.Code, start.Body.String())
	}
	cookie := stateCookieFrom(r.t, start)
	csrf := redirectQuery(r.t, start).Get("state")
	return r.callback(callerID, orgID, cookie.Value, url.Values{"code": {code}, "state": {csrf}})
}

func (r *installRig) deploymentRow(workspaceID string) *slackstore.Connection {
	r.t.Helper()
	c, err := slackstore.FromStores(r.stores).Connections.GetByWorkspaceAppSystem(r.t.Context(), workspaceID, testDeploymentApp().AppID)
	if err != nil {
		r.t.Fatalf("read the deployment connection: %v", err)
	}
	return c
}

func (r *installRig) botTokenOf(orgID, workspaceID string) string {
	r.t.Helper()
	return r.secretValue(orgID, integrations.SlackConnectionKeysFor(workspaceID, testDeploymentApp().AppID).BotToken)
}

// assertNothingStored checks that orgID has no deployment connection, bot
// token, or audit entry for workspaceID.
func (r *installRig) assertNothingStored(orgID, workspaceID string) {
	r.t.Helper()
	if c := r.deploymentRow(workspaceID); c != nil {
		r.t.Errorf("deployment connection = %+v; want none", c)
	}
	if v := r.botTokenOf(orgID, workspaceID); v != "" {
		r.t.Errorf("bot token stored = %q; want none", v)
	}
	if rows := r.auditRows(orgID); len(rows) != 0 {
		r.t.Errorf("audit rows = %+v; want none", rows)
	}
}

func stateCookieFrom(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == installStateCookieName {
			return c
		}
	}
	t.Fatalf("no %s cookie set; Set-Cookie = %v", installStateCookieName, rec.Header().Values("Set-Cookie"))
	return nil
}

func assertNoStateCookie(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == installStateCookieName {
			t.Errorf("state cookie set: %+v; want none", c)
		}
	}
}

// assertCookieCleared checks that the response spends the state cookie.
func assertCookieCleared(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	c := stateCookieFrom(t, rec)
	if c.MaxAge >= 0 || c.Value != "" || c.Path != installStatePath {
		t.Errorf("state cookie = %+v; want it cleared at %s", c, installStatePath)
	}
}

func redirectURL(t *testing.T, rec *httptest.ResponseRecorder) *url.URL {
	t.Helper()
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (body=%s)", rec.Code, rec.Body.String())
	}
	u, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	return u
}

func redirectQuery(t *testing.T, rec *httptest.ResponseRecorder) url.Values {
	t.Helper()
	return redirectURL(t, rec).Query()
}

// assertRedirect checks that rec redirects to path with exactly want on its
// query.
func assertRedirect(t *testing.T, rec *httptest.ResponseRecorder, path string, want url.Values) {
	t.Helper()
	u := redirectURL(t, rec)
	if u.Path != path || u.Query().Encode() != want.Encode() {
		t.Errorf("redirect = %s; want %s?%s", u, path, want.Encode())
	}
}

func settingsPath(orgID string) string { return "/orgs/" + orgID + "/settings" }

func failure(code string) url.Values { return url.Values{"slack_error": {code}} }

// ---------- start ----------

// TestInstallStart_Refusals: each refusal answers before the ceremony begins,
// and sets no cookie.
func TestInstallStart_Refusals(t *testing.T) {
	r := newInstallRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-install-start")
	member := pgtest.SeedUser(t, r.h, "slack-install-member")
	pgtest.AddOrgMember(t, r.h, member, orgID, teamID, "member", "member")
	outsider := pgtest.SeedUser(t, r.h, "slack-install-outsider")

	cases := []struct {
		name       string
		caller     string
		query      url.Values
		opts       []ctxOpt
		unentitled bool
		want       int
		field      string
	}{
		{name: "member", caller: member, want: http.StatusForbidden},
		{name: "outsider", caller: outsider, want: http.StatusNotFound},
		{name: "unentitled org", caller: owner, unentitled: true, want: http.StatusNotFound},
		{name: "API token", caller: owner, opts: []ctxOpt{withAPIToken(orgID)}, want: http.StatusForbidden},
		{name: "empty workspace_id", caller: owner, query: url.Values{"workspace_id": {""}}, want: http.StatusBadRequest, field: "workspace_id"},
		{name: "channel id as workspace_id", caller: owner, query: url.Values{"workspace_id": {"C0123ABC"}}, want: http.StatusBadRequest, field: "workspace_id"},
		{name: "lowercase workspace_id", caller: owner, query: url.Values{"workspace_id": {"t0123abc"}}, want: http.StatusBadRequest, field: "workspace_id"},
		{name: "short workspace_id", caller: owner, query: url.Values{"workspace_id": {"T1"}}, want: http.StatusBadRequest, field: "workspace_id"},
		{name: "two workspace_ids", caller: owner, query: url.Values{"workspace_id": {"T0123ABC", "T0456DEF"}}, want: http.StatusBadRequest, field: "workspace_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.unentitled {
				entitlements.RegisterProvider(entitlements.Static())
				t.Cleanup(func() { entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureSlack)) })
			}
			rec := r.start(tc.caller, orgID, tc.query, tc.opts...)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.want, rec.Body.String())
			}
			if tc.field != "" {
				errs := decodeBody[struct {
					Errors []httpx.ErrorItem `json:"errors"`
				}](t, rec)
				if len(errs.Errors) != 1 || errs.Errors[0].Reason != httpx.ReasonInvalidParam || errs.Errors[0].Field != tc.field {
					t.Errorf("errors = %+v; want one INVALID_PARAM on %s", errs.Errors, tc.field)
				}
			}
			assertNoStateCookie(t, rec)
		})
	}
	if len(r.ceremony.ttls) != 0 {
		t.Errorf("states signed = %d; want none", len(r.ceremony.ttls))
	}
}

// TestInstallStart_NoApp: a deployment with no Slack app sends the admin back
// with no_app, and sets no cookie.
func TestInstallStart_NoApp(t *testing.T) {
	r := newInstallRig(t)
	r.install.app = deploymentApp{}
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-install-noapp")

	rec := r.start(owner, orgID, url.Values{})
	assertRedirect(t, rec, settingsPath(orgID), failure("no_app"))
	assertNoStateCookie(t, rec)

	rec = r.start(owner, orgID, url.Values{"return_to": {"/orgs/" + orgID + "/settings/slack?tab=1"}})
	assertRedirect(t, rec, "/orgs/"+orgID+"/settings/slack", url.Values{"tab": {"1"}, "slack_error": {"no_app"}})
}

// TestInstallStart_RedirectsToSlack: the admin is sent to Slack's authorize
// page for the deployment app, with every bot scope and no user scope, the
// fixed redirect URI, and the state cookie's CSRF value; team is present only
// when workspace_id was given.
func TestInstallStart_RedirectsToSlack(t *testing.T) {
	r := newInstallRig(t)
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-install-redirect")
	returnTo := "/orgs/" + orgID + "/settings?tab=slack"

	for _, tc := range []struct {
		name, workspaceID string
	}{
		{"no workspace", ""},
		{"workspace hint", "T0HINT01"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := url.Values{"return_to": {returnTo}}
			if tc.workspaceID != "" {
				q.Set("workspace_id", tc.workspaceID)
			}
			rec := r.start(owner, orgID, q)
			u := redirectURL(t, rec)
			if u.Scheme+"://"+u.Host+u.Path != slackAuthorizeURL {
				t.Errorf("redirect = %s; want %s", u, slackAuthorizeURL)
			}
			got := u.Query()
			want := url.Values{
				"client_id":    {"111.222"},
				"scope":        {strings.Join(slackBotScopes, ",")},
				"redirect_uri": {"https://tf.example/api/slack/install/callback"},
				"state":        {got.Get("state")},
			}
			if tc.workspaceID != "" {
				want.Set("team", tc.workspaceID)
			}
			if got.Encode() != want.Encode() {
				t.Errorf("authorize query = %s; want %s", got.Encode(), want.Encode())
			}

			c := stateCookieFrom(t, rec)
			if c.Path != installStatePath || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.MaxAge != 600 {
				t.Errorf("state cookie = %+v; want path %s, HttpOnly, Secure, SameSite Lax, max age 600", c, installStatePath)
			}
			st, err := r.ceremony.ParseConnectState(c.Value)
			if err != nil {
				t.Fatalf("parse the cookie's state: %v", err)
			}
			if st.CSRF == "" || st.CSRF != got.Get("state") {
				t.Errorf("state = %q, cookie CSRF = %q; want them equal", got.Get("state"), st.CSRF)
			}
			if st.OrgID != orgID || st.UserID != owner || st.ReturnTo != returnTo {
				t.Errorf("cookie state = %+v; want org %s, user %s, return_to %s", st, orgID, owner, returnTo)
			}
		})
	}
	for _, ttl := range r.ceremony.ttls {
		if ttl != 10*time.Minute {
			t.Errorf("state ttl = %v, want 10m", ttl)
		}
	}
}

// ---------- callback: refusals ----------

// TestInstallCallback_Unauthenticated401: no session, or an API token, is 401,
// and the cookie is spent all the same.
func TestInstallCallback_Unauthenticated401(t *testing.T) {
	r := newInstallRig(t)
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-install-401")
	r.stageInstall("code-401", "T0UNAUTH1", "Acme", "xoxb-401", "triage-bot")

	for _, tc := range []struct {
		name   string
		caller string
		opts   []ctxOpt
	}{
		{"no session", "", nil},
		{"API token", owner, []ctxOpt{withAPIToken(orgID)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cookie, csrf := r.issue(orgID, owner, "")
			rec := r.callback(tc.caller, orgID, cookie, url.Values{"code": {"code-401"}, "state": {csrf}}, tc.opts...)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
			}
			assertCookieCleared(t, rec)
		})
	}
	if len(r.fake.oauthRequests) != 0 {
		t.Errorf("code exchanges = %d; want none", len(r.fake.oauthRequests))
	}
	r.assertNothingStored(orgID, "T0UNAUTH1")
}

// TestInstallCallback_StateRefusals: a cookie that is missing, forged,
// expired, echoed back wrong by the state parameter, or minted for another
// user is state, redirected to the session's active org, since the cookie's
// org is not trusted.
func TestInstallCallback_StateRefusals(t *testing.T) {
	r := newInstallRig(t)
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-install-state")
	otherOrg, otherUser, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-install-state-other")
	r.stageInstall("code-state", "T0STATE01", "Acme", "xoxb-state", "triage-bot")

	live, liveCSRF := r.issue(orgID, owner, "/somewhere")
	expired := r.ceremony.issue(server.ConnectState{OrgID: orgID, UserID: owner, CSRF: "csrf-expired"}, time.Now().Add(-time.Second))
	othersCookie, othersCSRF := r.issue(otherOrg, otherUser, "")

	for _, tc := range []struct {
		name, cookie, state, sessionOrg, wantPath string
	}{
		{"missing cookie", "", liveCSRF, orgID, settingsPath(orgID)},
		{"forged cookie", "forged", liveCSRF, orgID, settingsPath(orgID)},
		{"expired cookie", expired, "csrf-expired", orgID, settingsPath(orgID)},
		{"wrong state", live, "csrf-other", orgID, settingsPath(orgID)},
		{"no state", live, "", orgID, settingsPath(orgID)},
		{"another user's cookie", othersCookie, othersCSRF, orgID, settingsPath(orgID)},
		{"no active org", "", liveCSRF, "", "/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := r.callback(owner, tc.sessionOrg, tc.cookie, url.Values{"code": {"code-state"}, "state": {tc.state}})
			assertRedirect(t, rec, tc.wantPath, failure("state"))
			assertCookieCleared(t, rec)
		})
	}
	if len(r.fake.oauthRequests) != 0 {
		t.Errorf("code exchanges = %d; want none", len(r.fake.oauthRequests))
	}
	r.assertNothingStored(orgID, "T0STATE01")
	if got := r.installOutcomes(); got[installState] != 7 || len(got) != 1 {
		t.Errorf("install outcomes = %v; want {state: 7}", got)
	}
}

// TestInstallCallback_NoLongerAllowed: an admin demoted since the start, and
// an org whose slack entitlement lapsed, are state; "Require SSO" barring the
// caller from the org is sso_required. Each lands on the cookie's return_to.
func TestInstallCallback_NoLongerAllowed(t *testing.T) {
	r := newInstallRig(t)
	orgID, _, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-install-demoted")
	admin := pgtest.SeedUser(t, r.h, "slack-install-admin")
	pgtest.AddOrgMember(t, r.h, admin, orgID, teamID, "admin", "member")
	r.stageInstall("code-demoted", "T0DEMOTE1", "Acme", "xoxb-demoted", "triage-bot")
	returnTo := "/orgs/" + orgID + "/settings/slack"
	q := func(csrf string) url.Values { return url.Values{"code": {"code-demoted"}, "state": {csrf}} }

	t.Run("SSO required", func(t *testing.T) {
		r.ceremony.barred[orgID] = true
		t.Cleanup(func() { delete(r.ceremony.barred, orgID) })
		cookie, csrf := r.issue(orgID, admin, returnTo)
		rec := r.callback(admin, orgID, cookie, q(csrf))
		assertRedirect(t, rec, returnTo, failure("sso_required"))
		assertCookieCleared(t, rec)
	})

	t.Run("entitlement lapsed", func(t *testing.T) {
		cookie, csrf := r.issue(orgID, admin, returnTo)
		entitlements.RegisterProvider(entitlements.Static())
		t.Cleanup(func() { entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureSlack)) })
		rec := r.callback(admin, orgID, cookie, q(csrf))
		assertRedirect(t, rec, returnTo, failure("state"))
		assertCookieCleared(t, rec)
	})

	t.Run("demoted", func(t *testing.T) {
		cookie, csrf := r.issue(orgID, admin, returnTo)
		pgtest.MustExec(t, r.h.AdminDB, `UPDATE org_memberships SET role = 'member' WHERE user_id = $1 AND org_id = $2`, admin, orgID)
		rec := r.callback(admin, orgID, cookie, q(csrf))
		assertRedirect(t, rec, returnTo, failure("state"))
		assertCookieCleared(t, rec)
	})

	if len(r.fake.oauthRequests) != 0 {
		t.Errorf("code exchanges = %d; want none", len(r.fake.oauthRequests))
	}
	r.assertNothingStored(orgID, "T0DEMOTE1")
	if got := r.installOutcomes(); !maps.Equal(got, map[string]int64{installSSORequired: 1, installState: 2}) {
		t.Errorf("install outcomes = %v; want {sso_required: 1, state: 2}", got)
	}
}

// TestInstallCallback_SlackAnswers: what Slack sends back, short of a usable
// install, decides the code, and nothing is stored.
func TestInstallCallback_SlackAnswers(t *testing.T) {
	r := newInstallRig(t)
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-install-answers")
	const team = "T0ANSWER1"

	stage := func(code string, edit func(map[string]any)) {
		resp := r.stageInstall(code, team, "Acme", "xoxb-"+code, "triage-bot")
		edit(resp)
	}
	stage("wrong-app", func(m map[string]any) { m["app_id"] = "A0SOMEONEELSE" })
	stage("no-scope", func(m map[string]any) {
		m["scope"] = strings.Join(slackBotScopes[:len(slackBotScopes)-1], ",")
	})
	stage("user-token", func(m map[string]any) { m["token_type"] = "user" })
	stage("grid", func(m map[string]any) {
		m["team"] = nil
		m["enterprise"] = map[string]any{"id": "E0GRID", "name": "Grid"}
		m["is_enterprise_install"] = true
	})
	stage("no-team", func(m map[string]any) { m["team"] = nil })
	stage("rotation", func(m map[string]any) {
		m["access_token"] = "xoxe.xoxb-rotation"
		m["refresh_token"] = "xoxe-1-rotation"
		m["expires_in"] = 43200
	})

	for _, tc := range []struct {
		name  string
		query url.Values
		want  string
		// outcome is what slack.install.attempts counts it as.
		outcome string
		// exchanged is whether the code reached oauth.v2.access.
		exchanged bool
	}{
		{"declined", url.Values{"error": {"access_denied"}}, "denied", installDenied, false},
		{"other Slack error", url.Values{"error": {"invalid_scope"}}, "install_failed", installSlackError, false},
		{"no code", url.Values{}, "install_failed", installNoCode, false},
		{"failed exchange", url.Values{"code": {"unstaged"}}, "install_failed", installExchangeFailed, true},
		{"another app", url.Values{"code": {"wrong-app"}}, "install_failed", installWrongApp, true},
		{"missing scope", url.Values{"code": {"no-scope"}}, "install_failed", installMissingScopes, true},
		{"user token", url.Values{"code": {"user-token"}}, "install_failed", installMissingScopes, true},
		{"org-wide install", url.Values{"code": {"grid"}}, "enterprise_install", installEnterpriseInstall, true},
		{"no workspace", url.Values{"code": {"no-team"}}, "install_failed", installNoWorkspace, true},
		{"token rotation", url.Values{"code": {"rotation"}}, "install_failed", installTokenRotation, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(r.fake.oauthRequests)
			outcomesBefore := r.installOutcomes()
			cookie, csrf := r.issue(orgID, owner, "")
			tc.query.Set("state", csrf)
			rec := r.callback(owner, orgID, cookie, tc.query)
			assertRedirect(t, rec, settingsPath(orgID), failure(tc.want))
			assertCookieCleared(t, rec)
			if got := len(r.fake.oauthRequests) - before; (got == 1) != tc.exchanged {
				t.Errorf("code exchanges = %d; want exchanged %v", got, tc.exchanged)
			}
			if got := r.installOutcomes()[tc.outcome] - outcomesBefore[tc.outcome]; got != 1 {
				t.Errorf("install outcome %s counted %d times; want 1", tc.outcome, got)
			}
		})
	}
	r.assertNothingStored(orgID, team)
}

// TestInstallCallback_NoApp: a deployment app removed after the start is
// no_app, before any exchange.
func TestInstallCallback_NoApp(t *testing.T) {
	r := newInstallRig(t)
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-install-cb-noapp")
	r.stageInstall("code-noapp", "T0NOAPP01", "Acme", "xoxb-noapp", "triage-bot")
	cookie, csrf := r.issue(orgID, owner, "")
	r.install.app = deploymentApp{}

	rec := r.callback(owner, orgID, cookie, url.Values{"code": {"code-noapp"}, "state": {csrf}})
	assertRedirect(t, rec, settingsPath(orgID), failure("no_app"))
	if len(r.fake.oauthRequests) != 0 {
		t.Errorf("code exchanges = %d; want none", len(r.fake.oauthRequests))
	}
	r.assertNothingStored(orgID, "T0NOAPP01")
}

// ---------- callback: store ----------

// TestInstallCallback_NewWorkspace: the first install into a workspace stores
// one active deployment connection with its bot token and an access-log entry,
// and lands on return_to with slack=installed.
func TestInstallCallback_NewWorkspace(t *testing.T) {
	r := newInstallRig(t)
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-install-new")
	r.stageInstall("code-new", "T0NEW0001", "Acme", "xoxb-new", "triage-bot")
	returnTo := "/orgs/" + orgID + "/settings/slack?slack_error=denied"

	start := r.start(owner, orgID, url.Values{"return_to": {returnTo}})
	cookie := stateCookieFrom(t, start)
	rec := r.callback(owner, orgID, cookie.Value, url.Values{"code": {"code-new"}, "state": {redirectQuery(t, start).Get("state")}})
	assertRedirect(t, rec, "/orgs/"+orgID+"/settings/slack", url.Values{"slack": {"installed"}, "slack_workspace": {"T0NEW0001"}})
	assertCookieCleared(t, rec)
	if got := r.installOutcomes(); got[installInstalled] != 1 || len(got) != 1 {
		t.Errorf("install outcomes = %v; want {installed: 1}", got)
	}

	ex := r.fake.oauthRequests[0]
	if !ex.BasicAuth || ex.ClientID != "111.222" || ex.ClientSecret != "client-secret" ||
		ex.Form.Get("code") != "code-new" || ex.Form.Get("redirect_uri") != "https://tf.example/api/slack/install/callback" {
		t.Errorf("exchange = %+v; want basic auth as the deployment app, the code, and the fixed redirect URI", ex)
	}

	c := r.deploymentRow("T0NEW0001")
	if c == nil {
		t.Fatal("no deployment connection stored")
	}
	want := slackstore.Connection{
		WorkspaceID:        "T0NEW0001",
		APIAppID:           testDeploymentApp().AppID,
		OrgID:              orgID,
		Source:             slackstore.SourceDeployment,
		Active:             true,
		WorkspaceName:      "Acme",
		AppName:            "triage-bot",
		BotUserID:          "U0BOTT0NEW0001",
		BotTokenRef:        integrations.SlackConnectionKeysFor("T0NEW0001", testDeploymentApp().AppID).BotToken,
		RegisteredByUserID: owner,
	}
	got := *c
	got.CreatedAt, got.ConnectedAt, got.UpdatedAt = time.Time{}, time.Time{}, time.Time{}
	if got != want {
		t.Errorf("connection = %+v\nwant %+v", got, want)
	}
	if v := r.botTokenOf(orgID, "T0NEW0001"); v != "xoxb-new" {
		t.Errorf("bot token = %q, want xoxb-new", v)
	}
	audit := r.auditRows(orgID)
	if len(audit) != 1 || audit[0].Action != string(domain.AccessActionCredentialSet) || audit[0].Actor != owner ||
		audit[0].Kind != domain.CredentialKindSlackWorkspace || audit[0].Name != "Acme" ||
		audit[0].WorkspaceID != "T0NEW0001" || audit[0].APIAppID != testDeploymentApp().AppID {
		t.Errorf("audit rows = %+v; want one credential_set by the caller naming the workspace and the app", audit)
	}
}

// TestInstallCallback_StagedBesideOwnApp: in a workspace where the org's own
// app is active, the deployment connection is stored inactive, the own app's
// connection is unchanged, and the result is slack=staged.
func TestInstallCallback_StagedBesideOwnApp(t *testing.T) {
	r := newInstallRig(t)
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-install-staged")
	r.fake.stageConnectOK("xoxb-own", "T0STAGED1", "Acme", "U0OWNBOT", "A0OWNAPP01", "")
	if rec := r.connectAs(owner, orgID, "xoxb-own"); rec.Code != http.StatusCreated {
		t.Fatalf("own app connect status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	conns := slackstore.FromStores(r.stores).Connections
	ownBefore, err := conns.GetByWorkspaceAppSystem(t.Context(), "T0STAGED1", "A0OWNAPP01")
	if err != nil || ownBefore == nil {
		t.Fatalf("read own connection: %v (%v)", ownBefore, err)
	}
	r.stageInstall("code-staged", "T0STAGED1", "Acme", "xoxb-staged", "triage-bot")

	rec := r.installAs(owner, orgID, "code-staged")
	assertRedirect(t, rec, settingsPath(orgID), url.Values{"slack": {"staged"}, "slack_workspace": {"T0STAGED1"}})
	if got := r.installOutcomes(); got[installStaged] != 1 || len(got) != 1 {
		t.Errorf("install outcomes = %v; want {staged: 1}", got)
	}

	dep := r.deploymentRow("T0STAGED1")
	if dep == nil || dep.Active || dep.Source != slackstore.SourceDeployment {
		t.Errorf("deployment connection = %+v; want an inactive deployment row", dep)
	}
	if v := r.botTokenOf(orgID, "T0STAGED1"); v != "xoxb-staged" {
		t.Errorf("deployment bot token = %q, want xoxb-staged", v)
	}
	ownAfter, err := conns.GetByWorkspaceAppSystem(t.Context(), "T0STAGED1", "A0OWNAPP01")
	if err != nil || ownAfter == nil || *ownAfter != *ownBefore {
		t.Errorf("own connection = %+v (%v); want unchanged %+v", ownAfter, err, ownBefore)
	}
	active, err := conns.ActiveForWorkspaceSystem(t.Context(), orgID, "T0STAGED1")
	if err != nil || active == nil || active.APIAppID != "A0OWNAPP01" {
		t.Errorf("active connection = %+v (%v); want the own app's", active, err)
	}
}

// TestInstallCallback_Refresh: installing again into a workspace the org
// already holds keeps one row, replaces the token and the display columns,
// and keeps active and the registering user; an inactive connection stays
// staged.
func TestInstallCallback_Refresh(t *testing.T) {
	r := newInstallRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-install-refresh")
	admin := pgtest.SeedUser(t, r.h, "slack-install-refresh-admin")
	pgtest.AddOrgMember(t, r.h, admin, orgID, teamID, "admin", "member")

	t.Run("active", func(t *testing.T) {
		r.stageInstall("code-first", "T0REFRESH", "Acme", "xoxb-first", "triage-bot")
		if q := redirectQuery(t, r.installAs(owner, orgID, "code-first")); q.Get("slack") != "installed" {
			t.Fatalf("first install = %v; want installed", q)
		}
		r.stageInstall("code-second", "T0REFRESH", "Acme Renamed", "xoxb-second", "triage-bot (renamed)")
		rec := r.installAs(admin, orgID, "code-second")
		assertRedirect(t, rec, settingsPath(orgID), url.Values{"slack": {"installed"}, "slack_workspace": {"T0REFRESH"}})

		c := r.deploymentRow("T0REFRESH")
		if c == nil || !c.Active || c.WorkspaceName != "Acme Renamed" || c.AppName != "triage-bot (renamed)" || c.RegisteredByUserID != owner {
			t.Errorf("connection = %+v; want active, renamed, still registered by the first installer", c)
		}
		if v := r.botTokenOf(orgID, "T0REFRESH"); v != "xoxb-second" {
			t.Errorf("bot token = %q, want xoxb-second", v)
		}
		if got := len(r.auditRows(orgID)); got != 2 {
			t.Errorf("audit rows = %d, want 2", got)
		}
	})

	t.Run("inactive", func(t *testing.T) {
		r.fake.stageConnectOK("xoxb-own", "T0REFRESH2", "Beta", "U0OWNBOT", "A0OWNAPP02", "")
		if rec := r.connectAs(owner, orgID, "xoxb-own"); rec.Code != http.StatusCreated {
			t.Fatalf("own app connect status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
		}
		r.stageInstall("code-third", "T0REFRESH2", "Beta", "xoxb-third", "triage-bot")
		if q := redirectQuery(t, r.installAs(owner, orgID, "code-third")); q.Get("slack") != "staged" {
			t.Fatalf("first install = %v; want staged", q)
		}
		r.stageInstall("code-fourth", "T0REFRESH2", "Beta", "xoxb-fourth", "triage-bot")
		if q := redirectQuery(t, r.installAs(owner, orgID, "code-fourth")); q.Get("slack") != "staged" {
			t.Errorf("second install = %v; want staged", q)
		}
		if c := r.deploymentRow("T0REFRESH2"); c == nil || c.Active {
			t.Errorf("connection = %+v; want still inactive", c)
		}
		if v := r.botTokenOf(orgID, "T0REFRESH2"); v != "xoxb-fourth" {
			t.Errorf("bot token = %q, want xoxb-fourth", v)
		}
	})
}

// TestInstallCallback_WorkspaceTaken: a workspace where another org holds the
// deployment app's install is workspace_taken, and that org's connection and
// token are unchanged.
func TestInstallCallback_WorkspaceTaken(t *testing.T) {
	r := newInstallRig(t)
	holder, holderUser, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-install-holder")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-install-second")
	r.stageInstall("code-holder", "T0TAKEN01", "Acme", "xoxb-holder", "triage-bot")
	if q := redirectQuery(t, r.installAs(holderUser, holder, "code-holder")); q.Get("slack") != "installed" {
		t.Fatalf("holder install = %v; want installed", q)
	}
	before := r.deploymentRow("T0TAKEN01")

	r.stageInstall("code-second", "T0TAKEN01", "Acme", "xoxb-second", "triage-bot")
	rec := r.installAs(owner, orgID, "code-second")
	assertRedirect(t, rec, settingsPath(orgID), failure("workspace_taken"))
	if got := r.installOutcomes(); got[installInstalled] != 1 || got[installWorkspaceTaken] != 1 || len(got) != 2 {
		t.Errorf("install outcomes = %v; want {installed: 1, workspace_taken: 1}", got)
	}

	if after := r.deploymentRow("T0TAKEN01"); after == nil || *after != *before {
		t.Errorf("holder's connection = %+v; want unchanged %+v", after, before)
	}
	if v := r.botTokenOf(holder, "T0TAKEN01"); v != "xoxb-holder" {
		t.Errorf("holder's bot token = %q, want xoxb-holder", v)
	}
	if v := r.botTokenOf(orgID, "T0TAKEN01"); v != "" {
		t.Errorf("second org's bot token = %q; want none", v)
	}
	if rows := r.auditRows(orgID); len(rows) != 0 {
		t.Errorf("second org's audit rows = %+v; want none", rows)
	}
}

// TestInstallCallback_ConcurrentOrgs: two orgs installing into one workspace
// at once store exactly one connection, and the other org is workspace_taken.
func TestInstallCallback_ConcurrentOrgs(t *testing.T) {
	r := newInstallRig(t)
	orgA, userA, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-install-race-a")
	orgB, userB, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-install-race-b")
	r.stageInstall("code-a", "T0RACE001", "Acme", "xoxb-a", "triage-bot")
	r.stageInstall("code-b", "T0RACE001", "Acme", "xoxb-b", "triage-bot")
	cookieA, csrfA := r.issue(orgA, userA, "")
	cookieB, csrfB := r.issue(orgB, userB, "")

	var wg sync.WaitGroup
	results := make([]url.Values, 2)
	gate := make(chan struct{})
	for i, v := range []struct{ user, org, cookie, code, csrf string }{
		{userA, orgA, cookieA, "code-a", csrfA},
		{userB, orgB, cookieB, "code-b", csrfB},
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			rec := r.callback(v.user, v.org, v.cookie, url.Values{"code": {v.code}, "state": {v.csrf}})
			if rec.Code == http.StatusFound {
				u, _ := url.Parse(rec.Header().Get("Location"))
				results[i] = u.Query()
			}
		}()
	}
	close(gate)
	wg.Wait()

	installed, taken := 0, 0
	for _, q := range results {
		switch {
		case q.Get("slack") == "installed":
			installed++
		case q.Get("slack_error") == "workspace_taken":
			taken++
		}
	}
	if installed != 1 || taken != 1 {
		t.Fatalf("results = %v; want one installed and one workspace_taken", results)
	}
	var n int
	if err := r.h.AdminDB.QueryRowContext(t.Context(),
		`SELECT count(*) FROM org_slack_connections WHERE workspace_id = $1`, "T0RACE001").Scan(&n); err != nil {
		t.Fatalf("count connections: %v", err)
	}
	if n != 1 {
		t.Errorf("connections in the workspace = %d, want 1", n)
	}
}

// TestInstallCallback_BotNameUnavailable: when the new token cannot name its
// bot, the install is stored with no app name.
func TestInstallCallback_BotNameUnavailable(t *testing.T) {
	r := newInstallRig(t)
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-install-noname")
	r.stageInstall("code-noname", "T0NONAME1", "Acme", "xoxb-noname", "triage-bot")
	r.fake.stageBotsInfoFail(botIDForToken("xoxb-noname"), "bot_not_found")

	rec := r.installAs(owner, orgID, "code-noname")
	assertRedirect(t, rec, settingsPath(orgID), url.Values{"slack": {"installed"}, "slack_workspace": {"T0NONAME1"}})
	if c := r.deploymentRow("T0NONAME1"); c == nil || c.AppName != "" || !c.Active {
		t.Errorf("connection = %+v; want an active row with no app name", c)
	}
}
