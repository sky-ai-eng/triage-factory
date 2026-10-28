package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
)

// Connect through the deployment App, against the bind ceremony's fake GitHub.
//
// The org has no App of its own, so Connect's OAuth client is the
// deployment App's and it returns to the callback that App registers — the same
// URL the bind ceremony returns to. Every refusal case asserts that no identity
// row was written for the member, because a refusal that still links an
// identity is the failure that matters.

// connectRig is a bindRig plus a non-admin member of its org, signed in,
// with no GitHub identity on the deployment's GitHub.
type connectRig struct {
	*bindRig
	member    uuid.UUID
	memberSID string
}

func newConnectRig(t *testing.T, gh *fakeGitHub) *connectRig {
	t.Helper()
	rig := newBindRig(t, gh)
	member := rig.seedUser()
	rig.addMember(t, member)
	resp, _ := rig.driveCallback(member)
	return &connectRig{bindRig: rig, member: member, memberSID: rig.sidFromResp(resp)}
}

// addMember makes user a plain member of the rig's org.
func (r *bindRig) addMember(t *testing.T, user uuid.UUID) {
	t.Helper()
	if _, err := r.h.AdminDB.Exec(
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`,
		user, r.orgID.String()); err != nil {
		t.Fatalf("insert org_membership: %v", err)
	}
}

// returnTo is the page a member's Connect starts from in these tests.
func (r *connectRig) returnTo() string { return "/orgs/" + r.orgID.String() + "/triage" }

// startConnect drives Connect's start from the session sid.
func (r *bindRig) startConnect(t *testing.T, sid, returnTo string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET",
		"/api/orgs/"+r.orgID.String()+"/github/connect/start?return_to="+url.QueryEscape(returnTo), nil)
	req.AddCookie(&http.Cookie{Name: r.srv.sidCookieName(), Value: sid})
	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, req)
	return rec
}

// connectStarted drives a start that is expected to send the browser to
// GitHub's authorize page, and returns the state cookie it set and the state
// the authorize URL carries.
func (r *bindRig) connectStarted(t *testing.T, sid, returnTo string) (*http.Cookie, string) {
	t.Helper()
	rec := r.startConnect(t, sid, returnTo)
	if rec.Code != http.StatusFound {
		t.Fatalf("connect start status=%d body=%s, want 302", rec.Code, rec.Body.String())
	}
	u, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse start redirect: %v", err)
	}
	if u.Path != "/login/oauth/authorize" {
		t.Fatalf("start redirect = %q, want GitHub's authorize page", rec.Header().Get("Location"))
	}
	return connectCookieFrom(t, rec), u.Query().Get("state")
}

// connectCookieFrom returns the Connect state cookie a response set.
func connectCookieFrom(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range (&http.Response{Header: rec.Header()}).Cookies() {
		if c.Name == connectStateCookieName && c.Value != "" {
			return c
		}
	}
	t.Fatalf("response set no %s cookie (status=%d, set-cookie=%v)",
		connectStateCookieName, rec.Code, rec.Header()["Set-Cookie"])
	return nil
}

// serveManagedCallback drives the deployment App's callback with the session
// sid ("" for none) and the given cookies.
func (r *bindRig) serveManagedCallback(t *testing.T, sid, query string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", ManagedBindCallbackPath+"?"+query, nil)
	if sid != "" {
		req.AddCookie(&http.Cookie{Name: r.srv.sidCookieName(), Value: sid})
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, req)
	return rec
}

// connectQuery is what GitHub sends back from Connect's authorize.
func connectQuery(state string) string {
	return "code=gh_code&state=" + url.QueryEscape(state)
}

// identityOnDeploymentGitHub reads user's identity row on the deployment's
// GitHub: its login and source, and whether there is one.
func (r *bindRig) identityOnDeploymentGitHub(t *testing.T, user uuid.UUID) (login, source string, ok bool) {
	t.Helper()
	err := r.h.AdminDB.QueryRow(`
		SELECT login, source FROM user_github_identities WHERE user_id = $1 AND github_base_url = $2
	`, user.String(), db.NormalizeGitHubHost(r.ghBase)).Scan(&login, &source)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false
	}
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}
	return login, source, true
}

// assertNoIdentity is the assertion every refused link owes.
func (r *bindRig) assertNoIdentity(t *testing.T, user uuid.UUID) {
	t.Helper()
	if login, source, ok := r.identityOnDeploymentGitHub(t, user); ok {
		t.Errorf("identity row %s/%s written by a refused link, want none", login, source)
	}
}

// identityStatus reads GET …/github/identity for the session sid.
func (r *bindRig) identityStatus(t *testing.T, sid string) githubIdentityStatusResponse {
	t.Helper()
	resp := r.requestWithSid("GET", "/api/orgs/"+r.orgID.String()+"/github/identity", sid)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("identity status = %d, want 200", resp.StatusCode)
	}
	var out githubIdentityStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode identity status: %v", err)
	}
	return out
}

// assertGateError asserts a redirect to the org's Connect gate page
// carrying the error code.
func (r *bindRig) assertGateError(t *testing.T, rec *httptest.ResponseRecorder, code string) {
	t.Helper()
	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d body=%s, want 302 to the gate page", rec.Code, rec.Body.String())
	}
	u, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect: %v", err)
	}
	if u.Path != "/orgs/"+r.orgID.String()+"/connect-github" || u.Query().Get("error") != code {
		t.Errorf("redirect = %q, want the org's gate page with error=%s", rec.Header().Get("Location"), code)
	}
}

func TestDeploymentAppConnect_MemberLinksIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		host func(r *connectRig) string // the org's stored host; "" for none
	}{
		{"org on the deployment's GitHub", func(r *connectRig) string { return r.ghBase }},
		// The admin's case before the bind: the org has no host yet, and
		// links on the App's, which the bind then gives it.
		{"org with no GitHub URL", func(*connectRig) string { return "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh := newFakeGitHub()
			rig := newConnectRig(t, gh)
			rig.setGitHubHost(t, tc.host(rig))

			before := rig.identityStatus(t, rig.memberSID)
			if before.Connected || !before.ConnectAvailable || before.Host != db.NormalizeGitHubHost(rig.ghBase) {
				t.Errorf("status before = %+v, want not connected, connect available, on the deployment's GitHub", before)
			}

			start := rig.startConnect(t, rig.memberSID, rig.returnTo())
			if start.Code != http.StatusFound {
				t.Fatalf("start status=%d body=%s, want 302", start.Code, start.Body.String())
			}
			u, err := url.Parse(start.Header().Get("Location"))
			if err != nil {
				t.Fatalf("parse start redirect: %v", err)
			}
			if got := u.Scheme + "://" + u.Host; got != rig.ghBase || u.Path != "/login/oauth/authorize" {
				t.Errorf("start redirect = %q, want the deployment GitHub's authorize page", start.Header().Get("Location"))
			}
			if got := u.Query().Get("client_id"); got != gh.clientID {
				t.Errorf("client_id = %q, want the deployment App's %q", got, gh.clientID)
			}
			if got, want := u.Query().Get("redirect_uri"), "https://tf.test"+ManagedBindCallbackPath; got != want {
				t.Errorf("redirect_uri = %q, want the App's registered callback %q", got, want)
			}
			state := u.Query().Get("state")
			if state == "" {
				t.Fatal("authorize URL carries no state")
			}
			cookie := connectCookieFrom(t, start)
			if cookie.Path != ManagedBindCallbackPath || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode {
				t.Errorf("state cookie = path %q httponly %v secure %v samesite %v, want the callback's path, HttpOnly, Secure, Lax",
					cookie.Path, cookie.HttpOnly, cookie.Secure, cookie.SameSite)
			}

			out := rig.serveManagedCallback(t, rig.memberSID, connectQuery(state), cookie)
			if out.Code != http.StatusFound || out.Header().Get("Location") != rig.returnTo() {
				t.Fatalf("callback status=%d location=%q body=%s, want 302 to %q",
					out.Code, out.Header().Get("Location"), out.Body.String(), rig.returnTo())
			}
			gh.mu.Lock()
			form := gh.tokenForm
			gh.mu.Unlock()
			if form.Get("client_id") != gh.clientID || form.Get("client_secret") != "deployment_client_secret" ||
				form.Get("redirect_uri") != "https://tf.test"+ManagedBindCallbackPath {
				t.Errorf("code exchange form = %v, want the deployment App's client and callback", form)
			}

			login, source, ok := rig.identityOnDeploymentGitHub(t, rig.member)
			if !ok || login != gh.actorLogin || source != "connect_oauth" {
				t.Errorf("identity = (%q, %q, %v), want (%q, connect_oauth)", login, source, ok, gh.actorLogin)
			}
			var cleared bool
			for _, c := range (&http.Response{Header: out.Header()}).Cookies() {
				if c.Name == connectStateCookieName && c.MaxAge < 0 && c.Path == ManagedBindCallbackPath {
					cleared = true
				}
			}
			if !cleared {
				t.Errorf("callback did not clear the state cookie: %v", out.Header()["Set-Cookie"])
			}

			after := rig.identityStatus(t, rig.memberSID)
			if !after.Connected || after.Login != gh.actorLogin {
				t.Errorf("status after = %+v, want connected as %s", after, gh.actorLogin)
			}
			// Linking an identity is not a bind: the org's host and
			// credential are where they were.
			if got := rig.githubHost(t); got != tc.host(rig) {
				t.Errorf("org host = %q after a link, want %q", got, tc.host(rig))
			}
			rig.assertNothingBound(t)
		})
	}
}

// TestDeploymentAppConnect_AdminLinksThenBinds is the first-login path the
// setup wizard needs: an admin with no linked identity, on an org with no
// GitHub URL, links their identity through Connect and then completes the bind,
// whose identity proof that link satisfies — no token anywhere.
func TestDeploymentAppConnect_AdminLinksThenBinds(t *testing.T) {
	gh := newFakeGitHub()
	rig := newBindRig(t, gh)
	rig.setGitHubHost(t, "")
	rig.unlinkIdentity(t, rig.userID)

	cookie, state := rig.connectStarted(t, rig.sid, "/orgs/"+rig.orgID.String()+"/settings")
	if out := rig.serveManagedCallback(t, rig.sid, connectQuery(state), cookie); out.Code != http.StatusFound {
		t.Fatalf("connect callback status=%d body=%s, want 302", out.Code, out.Body.String())
	}
	if _, _, ok := rig.identityOnDeploymentGitHub(t, rig.userID); !ok {
		t.Fatal("the admin's identity was not linked")
	}

	bindCookie, bindState := rig.namedCeremony(t, gh.accountLogin)
	out := rig.callback(t, bindCookie, namedCallbackQuery(bindState))
	if out.Code != http.StatusFound || out.Header().Get("X-TF-Bind-Outcome") != "" {
		t.Fatalf("bind callback status=%d outcome=%q body=%s, want a completed bind",
			out.Code, out.Header().Get("X-TF-Bind-Outcome"), out.Body.String())
	}
	if n := rig.installationCount(t); n != 1 {
		t.Errorf("installations = %d after the bind, want 1", n)
	}
	if got := rig.githubHost(t); got != rig.ghBase {
		t.Errorf("org host = %q after the bind, want the App's %q", got, rig.ghBase)
	}
}

// TestDeploymentAppConnect_NotOffered covers the orgs the deployment App
// cannot serve: Connect is not advertised and its start answers no_app.
func TestDeploymentAppConnect_NotOffered(t *testing.T) {
	t.Run("org on another GitHub", func(t *testing.T) {
		rig := newConnectRig(t, newFakeGitHub())
		rig.setGitHubHost(t, "https://ghe.other.example")
		if st := rig.identityStatus(t, rig.memberSID); st.ConnectAvailable {
			t.Errorf("connect_available = true for an org off the deployment App's GitHub: %+v", st)
		}
		rig.assertGateError(t, rig.startConnect(t, rig.memberSID, rig.returnTo()), "no_app")
	})
	t.Run("no deployment App", func(t *testing.T) {
		rig := newConnectRig(t, newFakeGitHub())
		rig.srv.deploymentApp = githubapp.DeploymentApp{}
		if st := rig.identityStatus(t, rig.memberSID); st.ConnectAvailable {
			t.Errorf("connect_available = true with no deployment App: %+v", st)
		}
		rig.assertGateError(t, rig.startConnect(t, rig.memberSID, rig.returnTo()), "no_app")
	})
}

// TestDeploymentAppConnect_OwnAppIsPreferred: an org with its own App keeps
// today's flow — its client and its per-org callback — whatever the
// deployment offers.
func TestDeploymentAppConnect_OwnAppIsPreferred(t *testing.T) {
	rig := newConnectRig(t, newFakeGitHub())
	seedGitHubApp(t, rig.authRig, rig.orgID.String(), rig.userID.String(), "Iv1.own_client", "own_secret")

	start := rig.startConnect(t, rig.memberSID, rig.returnTo())
	if start.Code != http.StatusFound {
		t.Fatalf("start status=%d body=%s, want 302", start.Code, start.Body.String())
	}
	u, _ := url.Parse(start.Header().Get("Location"))
	if got := u.Query().Get("client_id"); got != "Iv1.own_client" {
		t.Errorf("client_id = %q, want the org's own App's", got)
	}
	if got, want := u.Query().Get("redirect_uri"), rig.srv.connectCallbackURL(rig.orgID.String()); got != want {
		t.Errorf("redirect_uri = %q, want the per-org callback %q", got, want)
	}
	if c := connectCookieFrom(t, start); c.Path != connectStatePath(rig.orgID.String()) {
		t.Errorf("state cookie path = %q, want the per-org %q", c.Path, connectStatePath(rig.orgID.String()))
	}
}

func TestDeploymentAppConnect_Refusals(t *testing.T) {
	t.Run("state mismatch", func(t *testing.T) {
		rig := newConnectRig(t, newFakeGitHub())
		cookie, _ := rig.connectStarted(t, rig.memberSID, rig.returnTo())
		out := rig.serveManagedCallback(t, rig.memberSID, connectQuery("not-the-cookies-state"), cookie)
		rig.assertGateError(t, out, "state")
		assertConnectCookieKept(t, out)
		rig.assertNoIdentity(t, rig.member)
		if rig.gh.served("/login/oauth/access_token") {
			t.Error("a mismatched state reached the code exchange")
		}
	})

	t.Run("cookie for a different user", func(t *testing.T) {
		rig := newConnectRig(t, newFakeGitHub())
		cookie, state := rig.connectStarted(t, rig.memberSID, rig.returnTo())
		other := rig.seedUser()
		rig.addMember(t, other)
		resp, _ := rig.driveCallback(other)
		out := rig.serveManagedCallback(t, rig.sidFromResp(resp), connectQuery(state), cookie)
		if out.Code != http.StatusFound || !strings.Contains(out.Header().Get("Location"), "error=state") {
			t.Errorf("status=%d location=%q, want a redirect with error=state", out.Code, out.Header().Get("Location"))
		}
		rig.assertNoIdentity(t, rig.member)
		rig.assertNoIdentity(t, other)
	})

	t.Run("user no longer in the org", func(t *testing.T) {
		rig := newConnectRig(t, newFakeGitHub())
		cookie, state := rig.connectStarted(t, rig.memberSID, rig.returnTo())
		if _, err := rig.h.AdminDB.Exec(
			`DELETE FROM org_memberships WHERE user_id = $1 AND org_id = $2`, rig.member, rig.orgID.String()); err != nil {
			t.Fatalf("remove membership: %v", err)
		}
		out := rig.serveManagedCallback(t, rig.memberSID, connectQuery(state), cookie)
		if loc := out.Header().Get("Location"); out.Code != http.StatusFound || (loc != "/" && !strings.Contains(loc, "error=state")) {
			t.Errorf("status=%d location=%q, want a redirect with error=state", out.Code, loc)
		}
		rig.assertNoIdentity(t, rig.member)
	})

	t.Run("API token", func(t *testing.T) {
		rig := newConnectRig(t, newFakeGitHub())
		cookie, state := rig.connectStarted(t, rig.memberSID, rig.returnTo())
		_, plaintext := rig.mintToken(rig.member, rig.orgID, "stray-header")
		req := httptest.NewRequest("GET", ManagedBindCallbackPath+"?"+connectQuery(state), nil)
		req.AddCookie(&http.Cookie{Name: rig.srv.sidCookieName(), Value: rig.memberSID})
		req.AddCookie(cookie)
		req.Header.Set("Authorization", "Bearer "+plaintext)
		out := rig.serve(req)
		assertOutcome(t, out, "session_required")
		if out.Code != http.StatusUnauthorized {
			t.Errorf("status=%d, want 401", out.Code)
		}
		rig.assertNoIdentity(t, rig.member)
	})

	t.Run("org requires SSO", func(t *testing.T) {
		rig := newConnectRig(t, newFakeGitHub())
		ext := &barringLoginExt{}
		rig.srv.loginExt = ext
		cookie, state := rig.connectStarted(t, rig.memberSID, rig.returnTo())
		ext.bar(rig.orgID.String())
		rig.assertGateError(t, rig.serveManagedCallback(t, rig.memberSID, connectQuery(state), cookie), "sso_required")
		rig.assertNoIdentity(t, rig.member)
	})

	t.Run("consent declined", func(t *testing.T) {
		rig := newConnectRig(t, newFakeGitHub())
		cookie, state := rig.connectStarted(t, rig.memberSID, rig.returnTo())
		out := rig.serveManagedCallback(t, rig.memberSID, "error=access_denied&state="+url.QueryEscape(state), cookie)
		rig.assertGateError(t, out, "denied")
		rig.assertNoIdentity(t, rig.member)
	})

	t.Run("org moved to another GitHub", func(t *testing.T) {
		rig := newConnectRig(t, newFakeGitHub())
		cookie, state := rig.connectStarted(t, rig.memberSID, rig.returnTo())
		rig.setGitHubHost(t, "https://ghe.other.example")
		rig.assertGateError(t, rig.serveManagedCallback(t, rig.memberSID, connectQuery(state), cookie), "no_app")
		rig.assertNoIdentity(t, rig.member)
	})
}

// assertConnectCookieKept asserts a response left the Connect state cookie as
// it was: a stale return must not end the Connect whose cookie it is.
func assertConnectCookieKept(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	for _, c := range (&http.Response{Header: rec.Header()}).Cookies() {
		if c.Name == connectStateCookieName {
			t.Errorf("response set the Connect cookie (%q, max-age %d); it belongs to a Connect still in flight", c.Value, c.MaxAge)
		}
	}
}

// TestDeploymentAppConnect_TwoTabs: every Connect through the deployment App
// shares one cookie, so a second start replaces the first's. The first tab's
// return is refused as an expired attempt and leaves the cookie alone, and the
// second tab's return then completes.
func TestDeploymentAppConnect_TwoTabs(t *testing.T) {
	rig := newConnectRig(t, newFakeGitHub())
	_, stateA := rig.connectStarted(t, rig.memberSID, rig.returnTo())
	cookieB, stateB := rig.connectStarted(t, rig.memberSID, rig.returnTo())

	outA := rig.serveManagedCallback(t, rig.memberSID, connectQuery(stateA), cookieB)
	rig.assertGateError(t, outA, "state")
	assertConnectCookieKept(t, outA)
	rig.assertNoIdentity(t, rig.member)
	if rig.gh.served("/login/oauth/access_token") {
		t.Error("the replaced Connect's code was exchanged")
	}

	outB := rig.serveManagedCallback(t, rig.memberSID, connectQuery(stateB), cookieB)
	if outB.Code != http.StatusFound || outB.Header().Get("Location") != rig.returnTo() {
		t.Fatalf("second tab status=%d location=%q body=%s, want 302 to %q",
			outB.Code, outB.Header().Get("Location"), outB.Body.String(), rig.returnTo())
	}
	if _, source, ok := rig.identityOnDeploymentGitHub(t, rig.member); !ok || source != "connect_oauth" {
		t.Errorf("identity source = %q (present %v) after the second tab, want connect_oauth", source, ok)
	}
}

// TestDeploymentAppConnect_StaleReturnLeavesABindAlone: a Connect return that
// cannot complete, in a browser that also holds a bind ceremony, is refused as
// a Connect. It neither spends the bind's record nor clears its cookie, and the
// bind then completes.
func TestDeploymentAppConnect_StaleReturnLeavesABindAlone(t *testing.T) {
	gh := newFakeGitHub()
	rig := newBindRig(t, gh)
	_, staleState := rig.connectStarted(t, rig.sid, "/orgs/"+rig.orgID.String()+"/settings")
	connectCookie, _ := rig.connectStarted(t, rig.sid, "/orgs/"+rig.orgID.String()+"/settings")
	bindCookie, bindState := rig.namedCeremony(t, gh.accountLogin)

	stale := rig.serveManagedCallback(t, rig.sid, connectQuery(staleState), connectCookie, bindCookie)
	rig.assertGateError(t, stale, "state")
	assertConnectCookieKept(t, stale)
	if rig.recordConsumed(t, bindCookie) {
		t.Fatal("a stale Connect return spent the bind ceremony's record")
	}
	for _, c := range (&http.Response{Header: stale.Header()}).Cookies() {
		if c.Name == managedBindCookieName {
			t.Errorf("a stale Connect return touched the bind cookie: %v", c)
		}
	}

	bound := rig.serveManagedCallback(t, rig.sid, namedCallbackQuery(bindState), connectCookie, bindCookie)
	if bound.Code != http.StatusFound || bound.Header().Get("X-TF-Bind-Outcome") != "" {
		t.Fatalf("bind status=%d outcome=%q body=%s, want a completed bind",
			bound.Code, bound.Header().Get("X-TF-Bind-Outcome"), bound.Body.String())
	}
	if n := rig.installationCount(t); n != 1 {
		t.Errorf("installations = %d, want 1", n)
	}
}

// TestDeploymentAppConnect_EmailsPermission: a deployment App without the
// account emails permission is refused by its preflight, at the start and at
// the callback, before anyone is asked to authorize or any code is exchanged.
func TestDeploymentAppConnect_EmailsPermission(t *testing.T) {
	t.Run("start", func(t *testing.T) {
		gh := newFakeGitHub()
		gh.emailsPerm = ""
		rig := newConnectRig(t, gh)
		rig.assertGateError(t, rig.startConnect(t, rig.memberSID, rig.returnTo()), "deployment_app_unavailable")
	})
	t.Run("callback", func(t *testing.T) {
		gh := newFakeGitHub()
		rig := newConnectRig(t, gh)
		cookie, state := rig.connectStarted(t, rig.memberSID, rig.returnTo())
		gh.emailsPerm = ""
		rig.assertGateError(t, rig.serveManagedCallback(t, rig.memberSID, connectQuery(state), cookie), "deployment_app_unavailable")
		if gh.served("/login/oauth/access_token") {
			t.Error("the code was exchanged despite the failed preflight")
		}
		rig.assertNoIdentity(t, rig.member)
	})
}

// TestDeploymentAppConnect_BindInFlightStillCompletes: one browser holding a
// live bind ceremony and a live Connect sends both cookies on every return to
// the shared callback, and each return reaches its own flow — on the bind's
// authorize leg, its install leg, and Connect's own.
func TestDeploymentAppConnect_BindInFlightStillCompletes(t *testing.T) {
	gh := newFakeGitHub()
	rig := newBindRig(t, gh)
	settings := "/orgs/" + rig.orgID.String() + "/settings"

	connectCookie, connectState := rig.connectStarted(t, rig.sid, settings)

	// The authorize leg, for an account with no installation the admin can
	// see, so the ceremony continues onto the install leg.
	gh.userInstallations = nil
	bindCookie, bindState := rig.namedCeremony(t, gh.accountLogin)
	hop := rig.serveManagedCallback(t, rig.sid, namedCallbackQuery(bindState), bindCookie, connectCookie)
	assertOutcome(t, hop, "install_continues")
	installCookie := rig.bindCookie(t, hop)

	// The install leg carries no state at all.
	gh.userInstallations = []int64{gh.installationID}
	bound := rig.serveManagedCallback(t, rig.sid, defaultCallbackQuery(gh.installationID), installCookie, connectCookie)
	if bound.Code != http.StatusFound || bound.Header().Get("X-TF-Bind-Outcome") != "" {
		t.Fatalf("install leg status=%d outcome=%q body=%s, want a completed bind",
			bound.Code, bound.Header().Get("X-TF-Bind-Outcome"), bound.Body.String())
	}
	if n := rig.installationCount(t); n != 1 {
		t.Errorf("installations = %d, want 1", n)
	}

	// And Connect's own return, with a second ceremony's cookie beside it,
	// links the identity and leaves that ceremony unspent.
	rig.unlinkIdentity(t, rig.userID)
	otherBind, _ := rig.namedCeremony(t, gh.accountLogin)
	linked := rig.serveManagedCallback(t, rig.sid, connectQuery(connectState), connectCookie, otherBind)
	if linked.Code != http.StatusFound || linked.Header().Get("Location") != settings {
		t.Fatalf("connect callback status=%d location=%q body=%s, want 302 to %q",
			linked.Code, linked.Header().Get("Location"), linked.Body.String(), settings)
	}
	if _, source, ok := rig.identityOnDeploymentGitHub(t, rig.userID); !ok || source != "connect_oauth" {
		t.Errorf("identity source = %q (present %v), want connect_oauth", source, ok)
	}
	if rig.recordConsumed(t, otherBind) {
		t.Error("Connect's callback spent the bind ceremony's record")
	}
	for _, c := range (&http.Response{Header: linked.Header()}).Cookies() {
		if c.Name == managedBindCookieName {
			t.Errorf("Connect's callback touched the bind cookie: %v", c)
		}
	}
}

// TestGitHubIdentityPAT_MultiModeIs404: multi mode links identity through
// Connect alone, so the token-capture route does not exist there.
func TestGitHubIdentityPAT_MultiModeIs404(t *testing.T) {
	rig := newConnectRig(t, newFakeGitHub())
	req := httptest.NewRequest("POST", "/api/orgs/"+rig.orgID.String()+"/github/identity/pat",
		strings.NewReader(fmt.Sprintf(`{"pat":%q}`, "ghp_member")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", rig.srv.deployCfg.publicURL)
	req.AddCookie(&http.Cookie{Name: rig.srv.sidCookieName(), Value: rig.memberSID})
	out := rig.serve(req)
	if out.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want 404", out.Code, out.Body.String())
	}
	rig.assertNoIdentity(t, rig.member)
	if rig.gh.callCount() != 0 {
		t.Errorf("GitHub was asked %d times by a route that does not exist", rig.gh.callCount())
	}
}
