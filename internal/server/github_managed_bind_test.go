package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The bind ceremony's end-to-end tests, against a fake GitHub.
//
// Every refusal arm gets its own case, and each one asserts the same two
// things: no installation row was written, and the org's credential class did
// not move. A ceremony that refuses and still leaves a binding behind is the
// failure this whole file exists to prevent, so "it returned an error" is never
// the assertion.

// bindTestKey is the RSA key the fake deployment App signs its JWTs with.
// Generated once for the package — key generation is the slowest thing in these
// tests and nothing here cares which key it is.
var (
	bindTestKeyOnce sync.Once
	bindTestKey     *rsa.PrivateKey
)

func deploymentTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	bindTestKeyOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate deployment app key: %v", err)
		}
		bindTestKey = key
	})
	return bindTestKey
}

// fakeGitHub serves every endpoint the ceremony touches. Each field is the
// answer one step gets, so a subtest bends exactly the arm it is about and
// leaves the rest of the flow real.
type fakeGitHub struct {
	// The deployment App's own identity, as GET /app reports it.
	slug        string
	clientID    string
	membersPerm string

	// The installation GET /app/installations/{id} reports, and whether it
	// exists at all.
	installationID   int64
	accountType      string
	accountLogin     string
	accountID        int64
	installationGone bool

	// The OAuth exchange and the whoami it proves.
	tokenExchangeStatus int
	actorLogin          string
	actorID             int64

	// The association gate's answer: the ids GET /user/installations reports.
	// Each rides with its account's login — the named-account leg looks the
	// account up in this listing — which is the fake's own installation's
	// login for that id and "acct-<id>" for any other unless
	// userInstallationAccounts says otherwise.
	userInstallations        []int64
	userInstallationAccounts map[int64]string

	// The authority gate's answer for an organization target.
	membershipStatus int
	membershipRole   string
	membershipState  string

	// calls records the paths served, so a test can prove a step was (or was
	// not) reached.
	mu    sync.Mutex
	calls []string
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{
		slug:                "tf-deployment",
		clientID:            "Iv1.deployment_client",
		membersPerm:         "read",
		installationID:      4242,
		accountType:         "Organization",
		accountLogin:        "acme",
		accountID:           700,
		tokenExchangeStatus: http.StatusOK,
		actorLogin:          "octocat",
		actorID:             99,
		userInstallations:   []int64{4242},
		membershipStatus:    http.StatusOK,
		membershipRole:      "admin",
		membershipState:     "active",
	}
}

func (f *fakeGitHub) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return srv.URL
}

// callCount is how many requests the fake has served, for a test whose claim
// is that GitHub was never asked at all.
func (f *fakeGitHub) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeGitHub) served(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == path {
			return true
		}
	}
	return false
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.URL.Path)
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	path := r.URL.Path
	switch {
	// The OAuth token endpoint lives on the WEB host, not under /api/v3.
	case path == "/login/oauth/access_token":
		if f.tokenExchangeStatus != http.StatusOK {
			w.WriteHeader(f.tokenExchangeStatus)
			fmt.Fprint(w, `{"error":"bad_verification_code"}`)
			return
		}
		fmt.Fprint(w, `{"access_token":"ghu_proof","token_type":"bearer"}`)

	case path == "/api/v3/app":
		fmt.Fprintf(w, `{"id":12345,"slug":%q,"client_id":%q,"owner":{"login":"tf","type":"Organization"},"permissions":{"members":%q}}`,
			f.slug, f.clientID, f.membersPerm)

	case strings.HasPrefix(path, "/api/v3/app/installations/"):
		if f.installationGone {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		fmt.Fprintf(w, `{"id":%d,"account":{"id":%d,"login":%q,"type":%q},"repository_selection":"all","created_at":"2026-01-02T03:04:05Z"}`,
			f.installationID, f.accountID, f.accountLogin, f.accountType)

	case path == "/api/v3/user":
		fmt.Fprintf(w, `{"id":%d,"login":%q}`, f.actorID, f.actorLogin)

	// The public account lookup the authorize leg uses to turn a named login
	// into the id GitHub's install URL takes. Two accounts exist: the fake's
	// installation target and the actor's own; anyone else is a 404.
	case strings.HasPrefix(path, "/api/v3/users/"):
		login := strings.TrimPrefix(path, "/api/v3/users/")
		switch {
		case strings.EqualFold(login, f.accountLogin):
			fmt.Fprintf(w, `{"id":%d,"login":%q,"type":%q}`, f.accountID, f.accountLogin, f.accountType)
		case strings.EqualFold(login, f.actorLogin):
			fmt.Fprintf(w, `{"id":%d,"login":%q,"type":"User"}`, f.actorID, f.actorLogin)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
		}

	// The PAT bind's identity capture reads the token owner's verified primary
	// email after the whoami. Only the door-guard tests reach it, by binding a
	// PAT against the deployment's GitHub.
	case path == "/api/v3/user/emails":
		fmt.Fprintf(w, `[{"email":"%s@example.test","primary":true,"verified":true}]`, f.actorLogin)

	case path == "/api/v3/user/installations":
		ids := make([]string, 0, len(f.userInstallations))
		for _, id := range f.userInstallations {
			login, ok := f.userInstallationAccounts[id]
			switch {
			case ok:
			case id == f.installationID:
				login = f.accountLogin
			default:
				login = fmt.Sprintf("acct-%d", id)
			}
			ids = append(ids, fmt.Sprintf(`{"id":%d,"account":{"login":%q}}`, id, login))
		}
		fmt.Fprintf(w, `{"total_count":%d,"installations":[%s]}`, len(ids), strings.Join(ids, ","))

	case strings.Contains(path, "/memberships/"):
		if f.membershipStatus != http.StatusOK {
			w.WriteHeader(f.membershipStatus)
			fmt.Fprint(w, `{"message":"nope"}`)
			return
		}
		fmt.Fprintf(w, `{"state":%q,"role":%q}`, f.membershipState, f.membershipRole)

	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `{"message":"unexpected path %s"}`, path)
	}
}

// bindRig is an authRig with an org whose GitHub is the fake, an admin session,
// and a deployment App configured on the server.
type bindRig struct {
	*authRig
	gh     *fakeGitHub
	ghBase string
	orgID  uuid.UUID
	userID uuid.UUID
	sid    string
}

func newBindRig(t *testing.T, gh *fakeGitHub) *bindRig {
	t.Helper()
	rig := newAuthRig(t)

	// HTTPS public URL before the session is minted, so the Secure-cookie
	// assertions below see a production-shaped deployment (and the sid cookie
	// keeps whichever name that implies).
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	rig.srv.SetDeployConfig("https://tf.test", key)

	user := rig.seedUser()
	org, _ := rig.seedOrg(user, "bind-org-"+uuid.NewString()[:8])
	resp, _ := rig.driveCallback(user)
	sid := rig.sidFromResp(resp)

	// The fake GitHub is the deployment's GitHub: the deployment App is on it,
	// and the workspace seeded below is pointed at it. A test that wants a
	// workspace on some OTHER GitHub re-points the workspace, never the
	// deployment.
	ghBase := gh.start(t)
	ghbase.SetDefaultBaseURLForTest(t, ghBase)
	if _, err := rig.h.AdminDB.Exec(`
		INSERT INTO org_event_sources (org_id, kind, base_url)
		VALUES ($1, 'github', $2)
		ON CONFLICT (org_id, kind) DO UPDATE SET base_url = $2
	`, org.String(), ghBase); err != nil {
		t.Fatalf("seed org_event_sources: %v", err)
	}

	// The deployment App the ceremony mints and exchanges with. Set on the
	// field the handlers read; the resolver holds its own copy from the
	// environment (the zero App in a test), which nothing in this flow uses.
	rig.srv.deploymentApp = githubapp.DeploymentApp{
		AppID:         12345,
		PrivateKey:    deploymentTestKey(t),
		WebhookSecret: "whsec",
		ClientSecret:  "deployment_client_secret",
	}

	out := &bindRig{authRig: rig, gh: gh, ghBase: ghBase, orgID: org, userID: user, sid: sid}
	// The admin's GitHub identity, as a whoami under their own credential
	// captured it: the account the fake's OAuth exchange will say authorized.
	// The ceremony's identity proof compares the two by id.
	out.linkIdentity(t, user, gh.actorLogin, gh.actorID)
	return out
}

// linkIdentity records a user's GitHub identity on the deployment's GitHub,
// the way the per-user Connect capture does.
func (r *bindRig) linkIdentity(t *testing.T, user uuid.UUID, login string, id int64) {
	t.Helper()
	ghUserID := any(fmt.Sprint(id))
	if id == 0 {
		ghUserID = nil
	}
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO user_github_identities (user_id, github_base_url, login, github_user_id, source, verified_at)
		VALUES ($1, $2, $3, $4, 'connect_oauth', now())
		ON CONFLICT (user_id, github_base_url) DO UPDATE
		   SET login = EXCLUDED.login, github_user_id = EXCLUDED.github_user_id
	`, user.String(), db.NormalizeGitHubHost(r.ghBase), login, ghUserID); err != nil {
		t.Fatalf("link github identity: %v", err)
	}
}

// unlinkIdentity removes a user's GitHub identity on the deployment's GitHub.
func (r *bindRig) unlinkIdentity(t *testing.T, user uuid.UUID) {
	t.Helper()
	if _, err := r.h.AdminDB.Exec(`
		DELETE FROM user_github_identities WHERE user_id = $1 AND github_base_url = $2
	`, user.String(), db.NormalizeGitHubHost(r.ghBase)); err != nil {
		t.Fatalf("unlink github identity: %v", err)
	}
}

// sibling returns a second workspace on the SAME server, with its own admin
// session and its GitHub pointed at the same fake — what a race between two
// tenants over one installation needs, and something two separate rigs (two
// servers) could not express.
func (r *bindRig) sibling(t *testing.T, slug string) *bindRig {
	t.Helper()
	user := r.seedUser()
	org, _ := r.seedOrg(user, slug+"-"+uuid.NewString()[:8])
	resp, _ := r.driveCallback(user)
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO org_event_sources (org_id, kind, base_url)
		VALUES ($1, 'github', $2)
		ON CONFLICT (org_id, kind) DO UPDATE SET base_url = $2
	`, org.String(), r.ghBase); err != nil {
		t.Fatalf("seed org_event_sources: %v", err)
	}
	out := &bindRig{
		authRig: r.authRig, gh: r.gh, ghBase: r.ghBase,
		orgID: org, userID: user, sid: r.sidFromResp(resp),
	}
	out.linkIdentity(t, user, r.gh.actorLogin, r.gh.actorID)
	return out
}

// connectAccount drives the named-account start and returns the response.
func (r *bindRig) connectAccount(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/orgs/"+r.orgID.String()+"/github/managed/connect-account", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", r.srv.deployCfg.publicURL)
	req.AddCookie(&http.Cookie{Name: r.srv.sidCookieName(), Value: r.sid})
	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, req)
	return rec
}

// namedCeremony starts the named-account leg for login and returns the cookie
// it minted and the state its authorize URL carries.
func (r *bindRig) namedCeremony(t *testing.T, login string) (*http.Cookie, string) {
	t.Helper()
	rec := r.connectAccount(t, fmt.Sprintf(`{"account":%q}`, login))
	if rec.Code != http.StatusOK {
		t.Fatalf("connect-account status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var body struct {
		AuthorizeURL string `json:"authorize_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode connect-account body: %v", err)
	}
	u, err := url.Parse(body.AuthorizeURL)
	if err != nil {
		t.Fatalf("authorize_url %q: %v", body.AuthorizeURL, err)
	}
	return r.bindCookie(t, rec), u.Query().Get("state")
}

// namedCallbackQuery is what GitHub sends back from the OAuth authorize leg:
// a code and the state the authorize request carried, and no installation.
func namedCallbackQuery(state string) string {
	return "code=gh_code&state=" + url.QueryEscape(state)
}

// start drives the one start there is, for the fake's account, and returns
// the response — for the cases about what the start itself refuses.
func (r *bindRig) start(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	return r.connectAccount(t, fmt.Sprintf(`{"account":%q}`, r.gh.accountLogin))
}

// hopToInstall drives the ceremony up to GitHub's install page: the start for
// login, then the authorize-leg callback with the named account absent from
// the person's installations, which continues onto the install leg. It
// asserts the hop and returns the install leg's cookie. The fake's listing is
// emptied for the authorize callback and restored afterwards, so a test that
// shaped it for the install leg's association gate keeps its shape there.
func (r *bindRig) hopToInstall(t *testing.T, login string) *http.Cookie {
	t.Helper()
	saved := r.gh.userInstallations
	r.gh.userInstallations = nil
	defer func() { r.gh.userInstallations = saved }()

	cookie, state := r.namedCeremony(t, login)
	out := r.callback(t, cookie, namedCallbackQuery(state))
	if out.Code != http.StatusFound || out.Header().Get("X-TF-Bind-Outcome") != "install_continues" {
		t.Fatalf("authorize callback for an account without the App: status=%d outcome=%q body=%s, want the hop to GitHub's install page",
			out.Code, out.Header().Get("X-TF-Bind-Outcome"), out.Body.String())
	}
	return r.bindCookie(t, out)
}

// ceremony drives the ceremony onto the install leg for the fake's account and
// returns the cookie the install callback needs.
func (r *bindRig) ceremony(t *testing.T) *http.Cookie {
	t.Helper()
	return r.hopToInstall(t, r.gh.accountLogin)
}

// bindCookie returns the ceremony cookie a Connect response set, failing when
// there is none.
func (r *bindRig) bindCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range (&http.Response{Header: rec.Header()}).Cookies() {
		if c.Name == managedBindCookieName && c.Value != "" {
			return c
		}
	}
	t.Fatalf("connect set no %s cookie (status=%d, set-cookie=%v)",
		managedBindCookieName, rec.Code, rec.Header()["Set-Cookie"])
	return nil
}

// callback drives the GitHub return leg with the given cookie and query, from
// a signed-in browser — the shape a ceremony coming back has.
func (r *bindRig) callback(t *testing.T, cookie *http.Cookie, query string) *httptest.ResponseRecorder {
	t.Helper()
	return r.serveCallback(t, cookie, query, r.sid)
}

// callbackSignedOut drives the same URL from a browser with NO Triage Factory
// session, which is the ordinary shape of a GitHub-initiated install: GitHub's
// public install page is reachable by anyone the account owner points at it,
// with no reason ever to have visited TF. Attaching a session by default is how
// that case hid — every assertion below about the signed-out path goes through
// here on purpose.
func (r *bindRig) callbackSignedOut(t *testing.T, cookie *http.Cookie, query string) *httptest.ResponseRecorder {
	t.Helper()
	return r.serveCallback(t, cookie, query, "")
}

func (r *bindRig) serveCallback(t *testing.T, cookie *http.Cookie, query, sid string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", ManagedBindCallbackPath+"?"+query, nil)
	if sid != "" {
		req.AddCookie(&http.Cookie{Name: r.srv.sidCookieName(), Value: sid})
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, req)
	return rec
}

// defaultCallbackQuery is what GitHub sends back on a successful install with
// the OAuth-during-installation setting enabled: a code and an installation id,
// together, at one callback.
func defaultCallbackQuery(installationID int64) string {
	return fmt.Sprintf("code=gh_code&installation_id=%d&setup_action=install", installationID)
}

// credentialClass reads the org's stored GitHub credential class. A workspace
// that has never had one written has no org_settings row at all, which reads
// back as "" — the state every refusal below must leave it in.
func (r *bindRig) credentialClass(t *testing.T) string {
	t.Helper()
	var class sql.NullString
	err := r.h.AdminDB.QueryRow(
		`SELECT github_credential_class FROM org_settings WHERE org_id = $1`, r.orgID.String(),
	).Scan(&class)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("read github_credential_class: %v", err)
	}
	return class.String
}

// installationCount counts the org's live installation rows.
func (r *bindRig) installationCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := r.h.AdminDB.QueryRow(`
		SELECT count(*) FROM org_github_app_installations
		 WHERE org_id = $1 AND removed_at IS NULL
	`, r.orgID.String()).Scan(&n); err != nil {
		t.Fatalf("count installations: %v", err)
	}
	return n
}

// assertNothingBound is the assertion every refusal owes: no row, no class
// change. A refusal that still writes is the bug the ceremony exists to
// prevent, and a status code alone would not catch it.
func (r *bindRig) assertNothingBound(t *testing.T) {
	t.Helper()
	if n := r.installationCount(t); n != 0 {
		t.Errorf("%d installation rows written by a refused bind, want 0", n)
	}
	if class := r.credentialClass(t); class == string(domain.GitHubCredentialClassManagedApp) {
		t.Errorf("credential class = %q after a refused bind; the class must not move", class)
	}
}

// assertOutcome checks the refusal code the page carries.
func assertOutcome(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	if got := rec.Header().Get("X-TF-Bind-Outcome"); got != want {
		t.Errorf("outcome = %q, want %q (status=%d body=%s)", got, want, rec.Code, rec.Body.String())
	}
}

// --- Happy paths -----------------------------------------------------------

func TestManagedBind_OrganizationTarget(t *testing.T) {
	gh := newFakeGitHub()
	rig := newBindRig(t, gh)

	// The account has no installation yet, so the authorize leg continues
	// onto GitHub's install page with the account preselected.
	gh.userInstallations = nil
	first, state := rig.namedCeremony(t, "acme")
	hop := rig.callback(t, first, namedCallbackQuery(state))
	if hop.Code != http.StatusFound {
		t.Fatalf("authorize callback status=%d outcome=%q body=%s, want 302",
			hop.Code, hop.Header().Get("X-TF-Bind-Outcome"), hop.Body.String())
	}
	assertOutcome(t, hop, "install_continues")
	if want := rig.ghBase + "/apps/tf-deployment/installations/new/permissions?target_id=700"; hop.Header().Get("Location") != want {
		t.Errorf("hop = %q, want the install page with the named account preselected %q",
			hop.Header().Get("Location"), want)
	}
	if !gh.served("/api/v3/users/acme") {
		t.Error("the account's id was not resolved under the person's token")
	}
	cookie := rig.bindCookie(t, hop)
	if cookie.Value == first.Value {
		t.Error("the install leg rides the authorize leg's nonce; the hop must mint a fresh record")
	}
	rig.assertNothingBound(t)

	// GitHub installed on the account and came back.
	gh.userInstallations = []int64{4242}
	out := rig.callback(t, cookie, defaultCallbackQuery(4242))
	if out.Code != http.StatusFound {
		t.Fatalf("callback status=%d outcome=%q body=%s, want 302",
			out.Code, out.Header().Get("X-TF-Bind-Outcome"), out.Body.String())
	}
	if want := "/orgs/" + rig.orgID.String() + "/settings#github-app"; out.Header().Get("Location") != want {
		t.Errorf("callback redirect = %q, want %q", out.Header().Get("Location"), want)
	}

	// Both gates ran. The association read is GitHub's prescribed check and the
	// membership read is the half it does not prescribe; a bind that skipped
	// either would still redirect, so their absence has to be an assertion.
	if !gh.served("/api/v3/user/installations") {
		t.Error("the association gate did not run")
	}
	if !gh.served("/api/v3/orgs/acme/memberships/octocat") {
		t.Error("the authority gate did not run")
	}

	var accountLogin, accountType, accountID, host string
	if err := rig.h.AdminDB.QueryRow(`
		SELECT account_login, account_type, account_id, github_host
		  FROM org_github_app_installations
		 WHERE org_id = $1 AND installation_id = '4242'
	`, rig.orgID.String()).Scan(&accountLogin, &accountType, &accountID, &host); err != nil {
		t.Fatalf("read installation row: %v", err)
	}
	if accountLogin != "acme" || accountType != "Organization" || accountID != "700" {
		t.Errorf("installation row = (%q, %q, %q), want (acme, Organization, 700) — "+
			"the facts must come from the App's own read, not the association listing",
			accountLogin, accountType, accountID)
	}
	if host != rig.ghBase {
		t.Errorf("github_host = %q, want %q", host, rig.ghBase)
	}
	if class := rig.credentialClass(t); class != string(domain.GitHubCredentialClassManagedApp) {
		t.Errorf("credential class = %q, want managed_app", class)
	}
}

func TestManagedBind_UserTarget(t *testing.T) {
	gh := newFakeGitHub()
	// Installed on the authorizing user's own personal account: the authority
	// gate is an identity comparison and GitHub is not asked anything.
	gh.accountType = "User"
	gh.accountLogin = "octocat"
	gh.accountID = 99
	rig := newBindRig(t, gh)

	out := rig.callback(t, rig.ceremony(t), defaultCallbackQuery(4242))
	if out.Code != http.StatusFound {
		t.Fatalf("callback status=%d outcome=%q, want 302", out.Code, out.Header().Get("X-TF-Bind-Outcome"))
	}
	if gh.served("/api/v3/orgs/octocat/memberships/octocat") {
		t.Error("a user-target bind asked GitHub about an organization membership")
	}
	if rig.installationCount(t) != 1 {
		t.Error("no installation row written for a user-target bind")
	}
	if class := rig.credentialClass(t); class != string(domain.GitHubCredentialClassManagedApp) {
		t.Errorf("credential class = %q, want managed_app", class)
	}
}

func TestManagedBind_UserTargetOnSomeoneElsesAccount(t *testing.T) {
	gh := newFakeGitHub()
	// Installed on a DIFFERENT personal account than the one authorizing.
	// Nobody administers another person's account, so there is no arm to pass.
	gh.accountType = "User"
	gh.accountLogin = "victim"
	gh.accountID = 7
	rig := newBindRig(t, gh)

	out := rig.callback(t, rig.ceremony(t), defaultCallbackQuery(4242))
	assertOutcome(t, out, "not_account_admin")
	rig.assertNothingBound(t)
}

// TestManagedBind_ContractorIsRefused is the case that justifies this ticket's
// complexity, and it is named so nobody deletes it as redundant.
//
// A read-only contractor with :read on ONE repository inside Acme's
// installation sees that whole installation in GET /user/installations — so
// they pass GitHub's own prescribed association check. Without the authority
// gate they could bind Acme into a workspace they control, where their agents
// would then act on Acme's repositories.
func TestManagedBind_ContractorIsRefused(t *testing.T) {
	gh := newFakeGitHub()
	gh.membershipRole = "member" // association passes; authority does not
	rig := newBindRig(t, gh)

	out := rig.callback(t, rig.ceremony(t), defaultCallbackQuery(4242))

	if !gh.served("/api/v3/user/installations") {
		t.Error("the contractor did not even reach the association gate; the test proves nothing")
	}
	assertOutcome(t, out, "not_account_admin")
	if !strings.Contains(out.Body.String(), "acme") {
		t.Errorf("the refusal does not name the account the caller must administer: %s", out.Body.String())
	}
	rig.assertNothingBound(t)
}

// --- Refusal arms ----------------------------------------------------------

func TestManagedBind_Refusals(t *testing.T) {
	// Each case bends one thing about the world and asserts the refusal it
	// earns. The shared assertion is assertNothingBound: refusing is only half
	// the contract.
	cases := []struct {
		name    string
		bend    func(gh *fakeGitHub)
		query   string
		outcome string
	}{
		{
			name:    "code absent — the App is missing the OAuth-during-installation setting",
			query:   "installation_id=4242&setup_action=install",
			outcome: "missing_oauth_setting",
		},
		{
			name:    "installation_id absent",
			query:   "code=gh_code&setup_action=install",
			outcome: "no_installation",
		},
		{
			name:    "install went to an owner for approval",
			query:   "setup_action=request",
			outcome: "install_requested",
		},
		{
			name:    "token exchange fails",
			bend:    func(gh *fakeGitHub) { gh.tokenExchangeStatus = http.StatusBadRequest },
			outcome: "identity_unproven",
		},
		{
			name:    "the App cannot read the installation — a spoofed id looks like this",
			bend:    func(gh *fakeGitHub) { gh.installationGone = true },
			outcome: "installation_unreadable",
		},
		{
			name:    "installation absent from the user's installations",
			bend:    func(gh *fakeGitHub) { gh.userInstallations = []int64{11} },
			outcome: "not_your_installation",
		},
		{
			name:    "role is member",
			bend:    func(gh *fakeGitHub) { gh.membershipRole = "member" },
			outcome: "not_account_admin",
		},
		{
			name:    "role is billing_manager",
			bend:    func(gh *fakeGitHub) { gh.membershipRole = "billing_manager" },
			outcome: "not_account_admin",
		},
		{
			name:    "role lookup 403s — the members permission is gone",
			bend:    func(gh *fakeGitHub) { gh.membershipStatus = http.StatusForbidden },
			outcome: "verification_failed",
		},
		{
			name:    "role lookup errors",
			bend:    func(gh *fakeGitHub) { gh.membershipStatus = http.StatusInternalServerError },
			outcome: "verification_failed",
		},
		{
			name:    "role lookup 404s",
			bend:    func(gh *fakeGitHub) { gh.membershipStatus = http.StatusNotFound },
			outcome: "verification_failed",
		},
		{
			name:    "the deployment App lost the members permission",
			bend:    func(gh *fakeGitHub) { gh.membersPerm = "" },
			outcome: "deployment_app_unavailable",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gh := newFakeGitHub()
			if tc.bend != nil {
				tc.bend(gh)
			}
			rig := newBindRig(t, gh)

			// The preflight gates the start too, so an App with no members
			// permission never reaches a callback; and the identity proof
			// runs on the authorize leg first, so a failed exchange stops
			// there. The refusal is asserted wherever the ceremony actually
			// stops.
			rec := rig.start(t)
			if rec.Code != http.StatusOK {
				assertOutcome(t, rec, tc.outcome)
				rig.assertNothingBound(t)
				return
			}
			saved := gh.userInstallations
			gh.userInstallations = nil
			first := rig.bindCookie(t, rec)
			var body struct {
				AuthorizeURL string `json:"authorize_url"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode start: %v", err)
			}
			u, _ := url.Parse(body.AuthorizeURL)
			hop := rig.callback(t, first, namedCallbackQuery(u.Query().Get("state")))
			gh.userInstallations = saved
			if hop.Header().Get("X-TF-Bind-Outcome") != "install_continues" {
				assertOutcome(t, hop, tc.outcome)
				rig.assertNothingBound(t)
				return
			}
			query := tc.query
			if query == "" {
				query = defaultCallbackQuery(4242)
			}
			out := rig.callback(t, rig.bindCookie(t, hop), query)
			assertOutcome(t, out, tc.outcome)
			rig.assertNothingBound(t)
		})
	}
}

// TestManagedBind_WorkspaceOnAnotherGitHubIsRefused: the deployment App is on
// one GitHub, so a workspace whose github_base_url names another cannot bind
// it. The refusal names both hosts and points at bringing your own App; it
// fires at Connect, before any preflight, so the deployment's key is never
// presented to a GitHub that has not seen it — the fake counts zero calls.
// Nothing is written and the class does not move.
func TestManagedBind_WorkspaceOnAnotherGitHubIsRefused(t *testing.T) {
	gh := newFakeGitHub()
	rig := newBindRig(t, gh)
	if _, err := rig.h.AdminDB.Exec(`
		UPDATE org_event_sources SET base_url = $2 WHERE org_id = $1 AND kind = 'github'
	`, rig.orgID.String(), "https://ghe.example.com/"); err != nil {
		t.Fatalf("re-point the workspace: %v", err)
	}

	rec := rig.start(t)
	assertOutcome(t, rec, "wrong_github_host")
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"https://ghe.example.com", rig.ghBase, "Bring your own App"} {
		if !strings.Contains(body, want) {
			t.Errorf("refusal does not name %q:\n%s", want, body)
		}
	}
	if n := gh.callCount(); n != 0 {
		t.Errorf("GitHub served %d requests for a workspace on another host; want 0 — no preflight may be issued", n)
	}
	rig.assertNothingBound(t)
}

func TestManagedBind_NoCookieIsTheUnboundInstall(t *testing.T) {
	// Somebody installed the deployment App from its public page, so GitHub
	// returned them here with no ceremony behind it. That is an ordinary state,
	// not an error: the installation exists and belongs to no workspace, and
	// the answer is the recovery page — a redirect into the SPA, which is where
	// the Connect button that finishes the job lives.
	gh := newFakeGitHub()
	rig := newBindRig(t, gh)

	assertUnbound := func(t *testing.T, out *httptest.ResponseRecorder, wantLocation string) {
		t.Helper()
		if out.Code != http.StatusFound {
			t.Errorf("status = %d body=%s, want 302 — a recordless callback is not an error",
				out.Code, out.Body.String())
		}
		if got := out.Header().Get("Location"); got != wantLocation {
			t.Errorf("Location = %q, want %q", got, wantLocation)
		}
		assertOutcome(t, out, "unbound")
		if cc := out.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", cc)
		}
		rig.assertNothingBound(t)
		// The branch reads nothing and asks GitHub nothing: no exchange of the
		// code, no read of the installation. Whatever the query claims, it is
		// not acted on until a ceremony this deployment started comes back.
		if n := gh.callCount(); n != 0 {
			t.Errorf("GitHub served %d requests for a callback with no ceremony behind it; want 0", n)
		}
		// Nothing about the installation reaches the response — not the id GitHub
		// sent, and not the account it targets, which this branch never learns.
		// The only place an unbound installation's login may appear is the
		// operator log.
		rendered := out.Body.String() + fmt.Sprint(out.Header())
		for _, leak := range []string{"4242", gh.accountLogin, "gh_code"} {
			if strings.Contains(rendered, leak) {
				t.Errorf("recovery outcome carries %q; an unbound installation is described to no tenant surface: %s", leak, rendered)
			}
		}
	}

	// The case this branch actually exists for, and the one a session-attaching
	// test helper hides: the installer has NO Triage Factory session. Nothing
	// on this path resolves an identity, so a blanket 401 in front of it would
	// answer the one person it is for with a JSON error and a dead-ended tab.
	// The SPA route they are sent to handles sign-in with a return target.
	t.Run("signed_out", func(t *testing.T) {
		assertUnbound(t, rig.callbackSignedOut(t, nil, defaultCallbackQuery(4242)), ManagedInstallRecoveryPath)
	})

	// The same answer for a TF admin who happens to be signed in — the outcome
	// is about the missing ceremony, not about who is looking.
	t.Run("signed_in", func(t *testing.T) {
		assertUnbound(t, rig.callback(t, nil, defaultCallbackQuery(4242)), ManagedInstallRecoveryPath)
	})

	// An install REQUEST from the public page: GitHub parked it with an owner
	// and sent the requester here with no installation. The page it lands on
	// has to say "requested", because "installed, now connect it" would send
	// them to press a button that cannot find anything yet.
	t.Run("requested", func(t *testing.T) {
		assertUnbound(t, rig.callbackSignedOut(t, nil, "setup_action=request"),
			ManagedInstallRecoveryPath+managedInstallRecoveryRequested)
	})

	// The operator's signal, and the one place the branch leaves a trace: a
	// single line naming the installation GitHub sent back. It names it as the
	// unsigned claim it is, and carries neither the code nor a login.
	t.Run("operator_log", func(t *testing.T) {
		var logbuf bytes.Buffer
		restore := logging.SetOutput(&logbuf)
		defer restore()

		rig.callbackSignedOut(t, nil, defaultCallbackQuery(4242))

		lines := strings.Split(strings.TrimSpace(logbuf.String()), "\n")
		if len(lines) != 1 {
			t.Fatalf("recordless callback logged %d lines, want exactly 1:\n%s", len(lines), logbuf.String())
		}
		line := lines[0]
		if !strings.Contains(line, "level=INFO") {
			t.Errorf("the unbound-install line is not at INFO, the level a self-hoster reads by default: %s", line)
		}
		if !strings.Contains(line, "installation=4242") {
			t.Errorf("the line does not name the installation: %s", line)
		}
		if strings.Contains(line, "gh_code") {
			t.Errorf("the line carries the OAuth code: %s", line)
		}
		if strings.Contains(line, "level=ERROR") || strings.Contains(line, "level=WARN") {
			t.Errorf("an ordinary state logged as a fault: %s", line)
		}
	})
}

// TestManagedBind_RefusedBindIsLoggedForTheOperator pins the other operator
// signal: a ceremony that came back and did not land leaves one WARN line
// naming the installation and, once the App has read it, the account it
// targets — so a self-hoster can tell "went to the wrong workspace" from
// "never connected" without a payload, a code, or a token ever being logged.
func TestManagedBind_RefusedBindIsLoggedForTheOperator(t *testing.T) {
	gh := newFakeGitHub()
	gh.membershipRole = "member" // association passes; authority does not
	rig := newBindRig(t, gh)
	cookie := rig.ceremony(t)

	var logbuf bytes.Buffer
	restore := logging.SetOutput(&logbuf)
	out := rig.callback(t, cookie, defaultCallbackQuery(4242))
	restore()
	assertOutcome(t, out, "not_account_admin")

	var refusedLine string
	for _, line := range strings.Split(logbuf.String(), "\n") {
		if strings.Contains(line, "managed bind refused") {
			if refusedLine != "" {
				t.Fatalf("a refused bind logged twice:\n%s", logbuf.String())
			}
			refusedLine = line
		}
	}
	if refusedLine == "" {
		t.Fatalf("no refused-bind line in the log:\n%s", logbuf.String())
	}
	for _, want := range []string{"level=WARN", "reason=not_account_admin", "installation=4242", "account=" + gh.accountLogin} {
		if !strings.Contains(refusedLine, want) {
			t.Errorf("refused-bind line lacks %q: %s", want, refusedLine)
		}
	}
	for _, leak := range []string{"gh_code", "ghu_proof", "deployment_client_secret"} {
		if strings.Contains(logbuf.String(), leak) {
			t.Errorf("the log carries %q, which is a secret or a proof: %s", leak, logbuf.String())
		}
	}
}

// TestManagedBind_CompletingWithoutASessionIsRefused pins the other half of the
// split. The no-cookie door is open to anyone; the door that WRITES is not.
// Someone bearing a bind cookie is about to have a credential bound into a
// workspace, so that branch stays behind withSession and answers 401 without
// one — the ordinary authentication failure, not a bind.
func TestManagedBind_CompletingWithoutASessionIsRefused(t *testing.T) {
	gh := newFakeGitHub()
	rig := newBindRig(t, gh)
	cookie := rig.ceremony(t)

	out := rig.callbackSignedOut(t, cookie, defaultCallbackQuery(4242))
	if out.Code != http.StatusUnauthorized {
		t.Errorf("status = %d body=%s, want 401 — completing a bind requires a session",
			out.Code, out.Body.String())
	}
	rig.assertNothingBound(t)

	// The record must survive: nothing authenticated happened, so nothing was
	// spent, and signing in and starting again has to work.
	if rig.recordConsumed(t, cookie) {
		t.Error("an unauthenticated callback spent the pending-bind record")
	}
}

// recordConsumed reads whether the record behind cookie has been spent.
func (r *bindRig) recordConsumed(t *testing.T, cookie *http.Cookie) bool {
	t.Helper()
	var consumed sql.NullTime
	if err := r.h.AdminDB.QueryRow(
		`SELECT consumed_at FROM github_pending_binds WHERE nonce_hash = $1`, hashBindNonce(cookie.Value),
	).Scan(&consumed); err != nil {
		t.Fatalf("read pending bind: %v", err)
	}
	return consumed.Valid
}

func TestManagedBind_ExpiredRecord(t *testing.T) {
	gh := newFakeGitHub()
	rig := newBindRig(t, gh)
	cookie := rig.ceremony(t)

	// Age the record past its expiry. The store refuses it; the handler never
	// gets a chance to decide otherwise.
	if _, err := rig.h.AdminDB.Exec(
		`UPDATE github_pending_binds SET expires_at = now() - interval '1 minute' WHERE org_id = $1`,
		rig.orgID.String()); err != nil {
		t.Fatalf("age pending bind: %v", err)
	}

	out := rig.callback(t, cookie, defaultCallbackQuery(4242))
	assertOutcome(t, out, "link_expired")
	rig.assertNothingBound(t)
}

func TestManagedBind_ConsumedRecord(t *testing.T) {
	gh := newFakeGitHub()
	rig := newBindRig(t, gh)
	cookie := rig.ceremony(t)

	if out := rig.callback(t, cookie, defaultCallbackQuery(4242)); out.Code != http.StatusFound {
		t.Fatalf("first callback status=%d outcome=%q, want 302", out.Code, out.Header().Get("X-TF-Bind-Outcome"))
	}
	// Replaying the same cookie must not spend the record twice — the browser
	// back button, or a copied URL.
	out := rig.callback(t, cookie, defaultCallbackQuery(4242))
	assertOutcome(t, out, "link_expired")

	// And the replay must not have touched what the first bind wrote: still
	// one installation, still the same class.
	if n := rig.installationCount(t); n != 1 {
		t.Errorf("%d installation rows after replaying a spent cookie, want the 1 the first bind wrote", n)
	}
	if class := rig.credentialClass(t); class != string(domain.GitHubCredentialClassManagedApp) {
		t.Errorf("credential class = %q after a replay, want it unchanged at managed_app", class)
	}
}

func TestManagedBind_UnknownNonce(t *testing.T) {
	gh := newFakeGitHub()
	rig := newBindRig(t, gh)

	// A cookie that names no record: the planting attack's shape, where an
	// attacker supplies their own code + installation_id to a signed-in admin
	// but cannot make the victim's browser hold a nonce TF minted.
	out := rig.callback(t, &http.Cookie{Name: managedBindCookieName, Value: "not-a-real-nonce"},
		defaultCallbackQuery(4242))
	assertOutcome(t, out, "link_expired")
	rig.assertNothingBound(t)
	if gh.served("/login/oauth/access_token") {
		t.Error("a callback with an unknown nonce still exchanged the code")
	}
}

// TestManagedBind_SessionIsNotTheInitiator covers a browser carrying somebody
// else's still-live ceremony cookie — a shared machine, since the cookie is
// HttpOnly, SameSite=Lax and path-scoped and so is not reachable cross-site.
// The cookie proves a browser; the record proves the person.
func TestManagedBind_SessionIsNotTheInitiator(t *testing.T) {
	// signIn re-points the rig's session at another user and returns the rig.
	signIn := func(t *testing.T, rig *bindRig, user uuid.UUID) {
		t.Helper()
		resp, _ := rig.driveCallback(user)
		rig.sid = rig.sidFromResp(resp)
	}

	// Being an admin of the same workspace is not enough: the record names a
	// PERSON, and a colleague who did not start this ceremony did not start it.
	t.Run("another_admin_of_the_same_workspace", func(t *testing.T) {
		rig := newBindRig(t, newFakeGitHub())
		cookie := rig.ceremony(t)

		other := rig.seedUser()
		if _, err := rig.h.AdminDB.Exec(
			`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'admin')`,
			other, rig.orgID.String()); err != nil {
			t.Fatalf("insert org_membership: %v", err)
		}
		signIn(t, rig, other)

		out := rig.callback(t, cookie, defaultCallbackQuery(4242))
		assertOutcome(t, out, "link_expired")
		rig.assertNothingBound(t)
	})

	// The disclosure case: a signed-in user with no relationship to the
	// ceremony's workspace at all. The refusal must not name it — a back-link
	// into its settings would hand out an org id they never asked for, which is
	// the same thing the bound-elsewhere refusal is careful never to do.
	t.Run("an_unrelated_user_is_told_nothing_about_the_workspace", func(t *testing.T) {
		rig := newBindRig(t, newFakeGitHub())
		cookie := rig.ceremony(t)

		// Their own workspace, so they have a session; no membership in the
		// one whose ceremony the cookie belongs to.
		outsider := rig.seedUser()
		rig.seedOrg(outsider, "outsider-org-"+uuid.NewString()[:8])
		signIn(t, rig, outsider)

		out := rig.callback(t, cookie, defaultCallbackQuery(4242))
		assertOutcome(t, out, "link_expired")
		rig.assertNothingBound(t)
		if body := out.Body.String(); strings.Contains(body, rig.orgID.String()) {
			t.Errorf("the refusal names the ceremony's workspace to an unrelated user: %s", body)
		}
	})
}

func TestManagedBind_RoleRevokedMidCeremony(t *testing.T) {
	gh := newFakeGitHub()
	rig := newBindRig(t, gh)

	// A second admin starts the ceremony — the org's owner cannot be demoted
	// (an org must retain one), and demotion is the point of the test.
	admin := rig.seedUser()
	if _, err := rig.h.AdminDB.Exec(
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'admin')`,
		admin, rig.orgID.String()); err != nil {
		t.Fatalf("insert org_membership: %v", err)
	}
	resp, _ := rig.driveCallback(admin)
	rig.sid = rig.sidFromResp(resp)
	rig.linkIdentity(t, admin, gh.actorLogin, gh.actorID)
	demote := func() {
		if _, err := rig.h.AdminDB.Exec(
			`UPDATE org_memberships SET role = 'member' WHERE user_id = $1 AND org_id = $2`,
			admin, rig.orgID.String()); err != nil {
			t.Fatalf("demote admin: %v", err)
		}
	}
	promote := func() {
		if _, err := rig.h.AdminDB.Exec(
			`UPDATE org_memberships SET role = 'admin' WHERE user_id = $1 AND org_id = $2`,
			admin, rig.orgID.String()); err != nil {
			t.Fatalf("promote admin: %v", err)
		}
	}

	// The role is read again at every callback rather than trusted from the
	// record, because minutes have passed since the click — on the install
	// leg, whose record was minted by a callback the person was still an
	// admin for...
	cookie := rig.ceremony(t)
	demote()
	out := rig.callback(t, cookie, defaultCallbackQuery(4242))
	assertOutcome(t, out, "not_workspace_admin")
	rig.assertNothingBound(t)

	// ...and on the authorize leg.
	promote()
	first, state := rig.namedCeremony(t, "acme")
	demote()
	out = rig.callback(t, first, namedCallbackQuery(state))
	assertOutcome(t, out, "not_workspace_admin")
	rig.assertNothingBound(t)
}

func TestManagedBind_ConnectRequiresWorkspaceAdmin(t *testing.T) {
	gh := newFakeGitHub()
	rig := newBindRig(t, gh)

	member := rig.seedUser()
	if _, err := rig.h.AdminDB.Exec(
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`,
		member, rig.orgID.String()); err != nil {
		t.Fatalf("insert org_membership: %v", err)
	}
	resp, _ := rig.driveCallback(member)
	rig.sid = rig.sidFromResp(resp)

	rec := rig.start(t)
	if rec.Code != http.StatusForbidden {
		t.Errorf("start as a workspace member status=%d, want 403", rec.Code)
	}
	var n int
	if err := rig.h.AdminDB.QueryRow(`SELECT count(*) FROM github_pending_binds`).Scan(&n); err != nil {
		t.Fatalf("count pending binds: %v", err)
	}
	if n != 0 {
		t.Errorf("%d pending-bind records written for a non-admin, want 0", n)
	}
}

func TestManagedBind_InstallationBoundElsewhere(t *testing.T) {
	gh := newFakeGitHub()
	rig := newBindRig(t, gh)

	// Another workspace already holds this installation on this host.
	otherOwner := rig.seedUser()
	otherOrg, _ := rig.seedOrg(otherOwner, "other-org")
	if _, err := rig.h.AdminDB.Exec(`
		INSERT INTO org_github_app_installations
			(installation_id, org_id, account_type, account_login, github_host)
		VALUES ('4242', $1, 'Organization', 'acme', $2)
	`, otherOrg.String(), rig.ghBase); err != nil {
		t.Fatalf("seed foreign installation: %v", err)
	}

	out := rig.callback(t, rig.ceremony(t), defaultCallbackQuery(4242))
	assertOutcome(t, out, "bound_elsewhere")
	rig.assertNothingBound(t)

	// The refusal must not disclose the other workspace, by id or by slug.
	body := out.Body.String()
	if strings.Contains(body, otherOrg.String()) || strings.Contains(body, "other-org") {
		t.Errorf("the refusal names the other workspace: %s", body)
	}
}

// TestManagedBind_OrgAlreadyHoldsItsOwnApp covers both evaluations of the
// one-credential-slot rule: the advisory one at the Connect click, which exists
// so nobody is sent to GitHub to complete an install that cannot land, and the
// authoritative one inside the write lock, which is what actually holds the
// invariant when the credential appears mid-ceremony.
func TestManagedBind_OrgAlreadyHoldsItsOwnApp(t *testing.T) {
	seedApp := func(t *testing.T, rig *bindRig) {
		t.Helper()
		if _, err := rig.h.AdminDB.Exec(`
			INSERT INTO org_github_apps
				(org_id, app_id, slug, client_id, client_secret_ref, pem_ref, webhook_secret_ref, active)
			VALUES ($1, '999', 'byo-app', 'Iv1.byo', 'ref-secret', 'ref-pem', '', true)
		`, rig.orgID.String()); err != nil {
			t.Fatalf("seed byo app: %v", err)
		}
	}

	t.Run("advisory_at_connect", func(t *testing.T) {
		rig := newBindRig(t, newFakeGitHub())
		seedApp(t, rig)

		rec := rig.start(t)
		assertOutcome(t, rec, "credential_app_in_use")
		var n int
		if err := rig.h.AdminDB.QueryRow(`SELECT count(*) FROM github_pending_binds`).Scan(&n); err != nil {
			t.Fatalf("count pending binds: %v", err)
		}
		if n != 0 {
			t.Errorf("%d pending-bind records written by a refused Connect, want 0", n)
		}
	})

	t.Run("authoritative_at_write", func(t *testing.T) {
		rig := newBindRig(t, newFakeGitHub())
		// The registration lands AFTER the ceremony starts, which is the window
		// the advisory check cannot see and the lock exists to close.
		cookie := rig.ceremony(t)
		seedApp(t, rig)

		out := rig.callback(t, cookie, defaultCallbackQuery(4242))
		assertOutcome(t, out, "credential_app_in_use")
		rig.assertNothingBound(t)
	})
}

func TestManagedBind_OrgAlreadyHoldsAPAT(t *testing.T) {
	seedPAT := func(t *testing.T, rig *bindRig) {
		t.Helper()
		if err := rig.srv.tx.WithTx(context.Background(), rig.orgID.String(), rig.userID.String(),
			func(tx db.TxStores) error {
				return tx.Secrets.Put(context.Background(), rig.orgID.String(),
					integrations.KeyGitHubPAT, "ghp_live", "test PAT")
			}); err != nil {
			t.Fatalf("seed org PAT: %v", err)
		}
	}

	t.Run("advisory_at_connect", func(t *testing.T) {
		rig := newBindRig(t, newFakeGitHub())
		seedPAT(t, rig)

		assertOutcome(t, rig.start(t), "credential_pat_in_use")
		rig.assertNothingBound(t)
		var n int
		if err := rig.h.AdminDB.QueryRow(`SELECT count(*) FROM github_pending_binds`).Scan(&n); err != nil {
			t.Fatalf("count pending binds: %v", err)
		}
		if n != 0 {
			t.Errorf("%d pending-bind records written by a refused Connect, want 0", n)
		}
	})

	t.Run("authoritative_at_write", func(t *testing.T) {
		rig := newBindRig(t, newFakeGitHub())
		cookie := rig.ceremony(t)
		seedPAT(t, rig)

		out := rig.callback(t, cookie, defaultCallbackQuery(4242))
		assertOutcome(t, out, "credential_pat_in_use")
		rig.assertNothingBound(t)
	})
}

// TestManagedBind_BearerTokenCannotComplete pins the substitute for a check
// this route structurally cannot make.
//
// withSession treats a Bearer API token as the cookie's peer: any Authorization
// header sends the request down that branch, so the identity the handler reads
// would be the TOKEN's owner. Every other org-admin route defends the token's
// sealed org with tokenScopeAllows against the {org_id} in its path — and this
// route has no org in its path, by construction, so there is nothing for that
// gate to attach to.
//
// The case that makes it concrete: one person administers two workspaces,
// starts a ceremony for THIS one in a browser, and completes it on a request
// that also carries a token sealed to the OTHER. Identity would resolve off the
// token while the cookie decides the org — a credential bound outside the scope
// its token was minted for.
func TestManagedBind_BearerTokenCannotComplete(t *testing.T) {
	gh := newFakeGitHub()
	rig := newBindRig(t, gh)
	cookie := rig.ceremony(t)

	// The same person also administers a second workspace and holds a LIVE
	// token sealed to it. Both halves matter: a bogus header would be refused
	// by the middleware and would prove nothing about this route.
	otherOrg, _ := rig.seedOrg(rig.userID, "second-org-"+uuid.NewString()[:8])
	_, plaintext := rig.mintToken(rig.userID, otherOrg, "stray-header")

	req := httptest.NewRequest("GET", ManagedBindCallbackPath+"?"+defaultCallbackQuery(4242), nil)
	req.AddCookie(&http.Cookie{Name: rig.srv.sidCookieName(), Value: rig.sid})
	req.AddCookie(cookie)
	// A proxy or tool stamping a stray Authorization header is all it takes:
	// any such header sends withSession down the token branch, and from there
	// the identity the handler reads is the token's.
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rec := httptest.NewRecorder()
	rig.srv.mux.ServeHTTP(rec, req)

	// The specific refusal, not merely "not a redirect": an unrelated 500 would
	// also fail to bind, and would also pass a test that only checked that.
	assertOutcome(t, rec, "session_required")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d body=%s, want 401", rec.Code, rec.Body.String())
	}
	rig.assertNothingBound(t)

	// And the ceremony survives: nothing was proven, so nothing was spent, and
	// the admin can finish in the browser they started in.
	if rig.recordConsumed(t, cookie) {
		t.Error("a token-authenticated callback spent the pending-bind record")
	}
}

// --- Idempotence and concurrency -------------------------------------------

func TestManagedBind_RebindingTheSameInstallationIsIdempotent(t *testing.T) {
	gh := newFakeGitHub()
	rig := newBindRig(t, gh)

	if out := rig.callback(t, rig.ceremony(t), defaultCallbackQuery(4242)); out.Code != http.StatusFound {
		t.Fatalf("first bind status=%d outcome=%q, want 302", out.Code, out.Header().Get("X-TF-Bind-Outcome"))
	}
	// A second full ceremony for the installation this workspace already holds
	// is a re-bind, not a collision: the uniqueness refusal is about ANOTHER
	// workspace.
	if out := rig.callback(t, rig.ceremony(t), defaultCallbackQuery(4242)); out.Code != http.StatusFound {
		t.Fatalf("re-bind status=%d outcome=%q, want 302 (idempotent)",
			out.Code, out.Header().Get("X-TF-Bind-Outcome"))
	}
	if n := rig.installationCount(t); n != 1 {
		t.Errorf("%d installation rows after re-binding the same installation, want 1", n)
	}
}

func TestManagedBind_SecondAccountIsAdditive(t *testing.T) {
	// A workspace may bind several installations — one per GitHub account, which
	// is what an enterprise-owned App produces. The class flips on the first
	// bind and every later one runs the whole ceremony again.
	gh := newFakeGitHub()
	rig := newBindRig(t, gh)

	if out := rig.callback(t, rig.ceremony(t), defaultCallbackQuery(4242)); out.Code != http.StatusFound {
		t.Fatalf("first bind status=%d outcome=%q, want 302", out.Code, out.Header().Get("X-TF-Bind-Outcome"))
	}

	// A second installation, on a different account the same admin also
	// administers.
	gh.installationID = 5150
	gh.accountLogin = "acme-labs"
	gh.accountID = 701
	gh.userInstallations = []int64{4242, 5150}

	if out := rig.callback(t, rig.ceremony(t), defaultCallbackQuery(5150)); out.Code != http.StatusFound {
		t.Fatalf("second bind status=%d outcome=%q, want 302", out.Code, out.Header().Get("X-TF-Bind-Outcome"))
	}
	if n := rig.installationCount(t); n != 2 {
		t.Errorf("%d installation rows after binding two accounts, want 2 — later binds are additive", n)
	}
	if !gh.served("/api/v3/orgs/acme-labs/memberships/octocat") {
		t.Error("the second bind skipped the authority gate; every bind runs the full ceremony")
	}
}

// TestManagedBind_TwoWorkspacesRacingOneInstallation is the cross-tenant half
// of uniqueness, and it is the case a lock keyed by the ORG cannot catch: the
// two racers hold different org keys, so an org-keyed lock lets both read
// "nobody owns this" and both write, landing one GitHub account in two
// workspaces.
//
// The sequential refusal next door passes with or without the installation
// lock. This is the test that does not.
func TestManagedBind_TwoWorkspacesRacingOneInstallation(t *testing.T) {
	gh := newFakeGitHub()
	first := newBindRig(t, gh)
	second := first.sibling(t, "rival-org")

	// Both admins administer the same GitHub account and both complete a real
	// ceremony, so every gate passes for both. Only uniqueness separates them.
	racers := []*bindRig{first, second}
	cookies := make([]*http.Cookie, len(racers))
	for i, rig := range racers {
		cookies[i] = rig.ceremony(t)
	}

	var (
		start sync.WaitGroup
		done  sync.WaitGroup
		mu    sync.Mutex
		wins  int
	)
	start.Add(1)
	for i, rig := range racers {
		done.Add(1)
		go func(rig *bindRig, cookie *http.Cookie) {
			defer done.Done()
			req := httptest.NewRequest("GET", ManagedBindCallbackPath+"?"+defaultCallbackQuery(4242), nil)
			req.AddCookie(&http.Cookie{Name: rig.srv.sidCookieName(), Value: rig.sid})
			req.AddCookie(cookie)
			rec := httptest.NewRecorder()
			start.Wait()
			rig.srv.mux.ServeHTTP(rec, req)
			mu.Lock()
			defer mu.Unlock()
			if rec.Code == http.StatusFound {
				wins++
			}
		}(rig, cookies[i])
	}
	start.Done()
	done.Wait()

	if wins != 1 {
		t.Errorf("%d of %d workspaces bound the same installation, want exactly 1", wins, len(racers))
	}

	// The assertion that matters is the row count, not the status: one
	// installation may be live in exactly one workspace, whatever either
	// caller was told.
	var holders int
	if err := first.h.AdminDB.QueryRow(`
		SELECT count(*) FROM org_github_app_installations
		 WHERE github_host = $1 AND installation_id = '4242' AND removed_at IS NULL
	`, first.ghBase).Scan(&holders); err != nil {
		t.Fatalf("count holders: %v", err)
	}
	if holders != 1 {
		t.Errorf("installation 4242 is live in %d workspaces, want 1 — "+
			"the uniqueness check must serialize on the installation, not on the org", holders)
	}
}

func TestManagedBind_ConcurrentCallbacksElectOneWinner(t *testing.T) {
	gh := newFakeGitHub()
	rig := newBindRig(t, gh)
	cookie := rig.ceremony(t)

	const racers = 2
	var (
		start sync.WaitGroup
		done  sync.WaitGroup
		mu    sync.Mutex
		wins  int
	)
	start.Add(1)
	recs := make([]*httptest.ResponseRecorder, racers)
	for i := 0; i < racers; i++ {
		done.Add(1)
		go func(i int) {
			defer done.Done()
			req := httptest.NewRequest("GET", ManagedBindCallbackPath+"?"+defaultCallbackQuery(4242), nil)
			req.AddCookie(&http.Cookie{Name: rig.srv.sidCookieName(), Value: rig.sid})
			req.AddCookie(cookie)
			rec := httptest.NewRecorder()
			start.Wait()
			rig.srv.mux.ServeHTTP(rec, req)
			mu.Lock()
			defer mu.Unlock()
			recs[i] = rec
			if rec.Code == http.StatusFound {
				wins++
			}
		}(i)
	}
	start.Done()
	done.Wait()

	if wins != 1 {
		for i, rec := range recs {
			t.Logf("callback %d: status=%d outcome=%q", i, rec.Code, rec.Header().Get("X-TF-Bind-Outcome"))
		}
		t.Errorf("%d of %d concurrent callbacks succeeded, want exactly 1 — the record is single-use", wins, racers)
	}
	if n := rig.installationCount(t); n != 1 {
		t.Errorf("%d installation rows after a concurrent callback race, want 1", n)
	}
}

// --- The cookie ------------------------------------------------------------

func TestManagedBind_CookieAttributes(t *testing.T) {
	gh := newFakeGitHub()
	rig := newBindRig(t, gh)
	cookie := rig.bindCookie(t, rig.start(t))

	if !cookie.HttpOnly {
		t.Error("the bind cookie is not HttpOnly; script must never read the nonce")
	}
	if !cookie.Secure {
		t.Error("the bind cookie is not Secure on an https deployment")
	}
	// SameSite=Lax, and this assertion is the point of the test.
	//
	// The callback is a top-level GET navigation arriving from github.com. Lax
	// sends a cookie on exactly that; STRICT DROPS IT. Under Strict the cookie
	// would never arrive, every bind would refuse as a stale ceremony, and the
	// symptom would point at the record rather than at the cookie. Anyone
	// "hardening" this to Strict breaks the whole flow, so the value is pinned
	// here on purpose.
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("the bind cookie is SameSite=%v, want Lax — Strict drops it on the return from github.com "+
			"and every bind would fail as a stale ceremony", cookie.SameSite)
	}
	if cookie.Path != ManagedBindCallbackPath {
		t.Errorf("cookie path = %q, want the callback path %q", cookie.Path, ManagedBindCallbackPath)
	}
	if cookie.MaxAge <= 0 || time.Duration(cookie.MaxAge)*time.Second > db.GitHubPendingBindTTL {
		t.Errorf("cookie MaxAge = %ds, want a positive value no longer than the record's TTL (%s)",
			cookie.MaxAge, db.GitHubPendingBindTTL)
	}

	// The stored record holds the hash and never the nonce, so a database read
	// yields nothing that can complete a bind.
	var stored string
	if err := rig.h.AdminDB.QueryRow(
		`SELECT nonce_hash FROM github_pending_binds WHERE org_id = $1`, rig.orgID.String()).Scan(&stored); err != nil {
		t.Fatalf("read pending bind: %v", err)
	}
	if stored == cookie.Value {
		t.Error("the pending-bind record stores the nonce itself; it must store only its hash")
	}
	if stored != hashBindNonce(cookie.Value) {
		t.Error("the stored hash is not the hash of the cookie's nonce")
	}
}

// --- The closed refusal set ------------------------------------------------

// TestManagedBind_RefusalSetIsClosedAndDistinct holds the property the set is
// for. Every outcome the ceremony can produce is a named member with its own
// code and its own sentence, so a new failure mode has to declare itself rather
// than arriving wearing somebody else's explanation — and no member leaks an
// unfilled format placeholder into a page.
func TestManagedBind_RefusalSetIsClosedAndDistinct(t *testing.T) {
	set := []bindRefusal{
		refuseNoDeploymentApp,
		refuseWrongGitHub.withHosts("https://ghe.example.com", "https://github.com"),
		refuseNoOAuthSetting,
		refuseStaleCeremony,
		refuseNoInstallation,
		refuseSessionRequired,
		refuseNotWorkspaceAdmin,
		refuseIdentityUnproven,
		refuseIdentityNotLinked,
		refuseIdentityMismatch.withAccount("octocat"),
		refuseStateMismatch,
		refuseAccountNotConnectable.withAccount("acme"),
		refuseAccountMismatch.withAccounts("other-org", "acme"),
		refuseInstallationUnreadable,
		refuseNotAssociated,
		refuseNotAccountAdmin.withAccount("acme"),
		refuseGatesUndetermined,
		refuseBoundElsewhere,
		refuseOwnAppInTheWay,
		refusePATInTheWay,
		refuseInstallPending,
	}

	codes := make(map[string]bool, len(set))
	messages := make(map[string]bool, len(set))
	for _, r := range set {
		if r.code == "" || r.message == "" || r.status == 0 {
			t.Errorf("incomplete refusal %+v", r)
		}
		if strings.Contains(r.message, "%") {
			t.Errorf("refusal %q renders an unfilled placeholder: %q", r.code, r.message)
		}
		if codes[r.code] {
			t.Errorf("duplicate refusal code %q", r.code)
		}
		if messages[r.message] {
			t.Errorf("two refusals share the copy %q; each outcome owes the reader its own sentence", r.message)
		}
		codes[r.code], messages[r.message] = true, true
	}
}

// --- The identity proof ------------------------------------------------------

// TestManagedBind_PlantedCodeIsRefused is the confused deputy the ceremony
// exists to stop, in the one shape the other four proofs do not cover on their
// own: the victim admin has a live ceremony (they clicked Connect and have not
// finished), and an attacker who installed the App on THEIR OWN account has a
// code and an installation_id they never exchanged. The victim's browser loads
// that URL. Cookie, record, session and role all hold — they are the victim's —
// and both GitHub gates pass, because they are asked about the attacker's
// token against the attacker's installation. Only the identity proof says no:
// the account that authorized is not the one linked to the admin.
func TestManagedBind_PlantedCodeIsRefused(t *testing.T) {
	gh := newFakeGitHub()
	rig := newBindRig(t, gh)
	cookie := rig.ceremony(t)

	// The attacker's world: the code exchanges for THEIR token, the
	// installation is on THEIR org, they can see it and they own it.
	gh.actorLogin, gh.actorID = "mallory", 666
	gh.installationID, gh.accountLogin, gh.accountID = 9999, "mallory-corp", 800
	gh.userInstallations = []int64{9999}

	out := rig.callback(t, cookie, defaultCallbackQuery(9999))
	assertOutcome(t, out, "identity_mismatch")
	if out.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", out.Code)
	}
	rig.assertNothingBound(t)
	// The proof runs before either GitHub gate, so the attacker's
	// installation is never even read.
	if gh.served("/api/v3/app/installations/9999") {
		t.Error("the attacker's installation was read; the identity proof must come first")
	}
	if !strings.Contains(out.Body.String(), "@octocat") {
		t.Errorf("the refusal must name the admin's own linked login; body=%s", out.Body.String())
	}
}

// TestManagedBind_IdentityMustBeLinked: with nothing linked the proof cannot
// run, and a proof that cannot run refuses. The copy points at the cure.
func TestManagedBind_IdentityMustBeLinked(t *testing.T) {
	gh := newFakeGitHub()
	rig := newBindRig(t, gh)

	// On the install leg: linked when the hop was taken, unlinked by the
	// time GitHub comes back.
	cookie := rig.ceremony(t)
	rig.unlinkIdentity(t, rig.userID)
	out := rig.callback(t, cookie, defaultCallbackQuery(4242))
	assertOutcome(t, out, "identity_not_linked")
	rig.assertNothingBound(t)

	// On the authorize leg: nothing linked, so nobody is sent anywhere —
	// the hop to GitHub's install page is offered only to a proven person.
	first, state := rig.namedCeremony(t, "acme")
	out = rig.callback(t, first, namedCallbackQuery(state))
	assertOutcome(t, out, "identity_not_linked")
	rig.assertNothingBound(t)
	if out.Code == http.StatusFound {
		t.Error("an unlinked person was sent on to GitHub's install page")
	}
}

// TestManagedBind_IdentityWithoutIDIsNotComparable: a linked row captured
// before numeric ids were recorded carries a renameable login and nothing
// else. Comparing logins is a comparison that can be arranged, so the row is
// as good as absent until it is recaptured.
func TestManagedBind_IdentityWithoutIDIsNotComparable(t *testing.T) {
	gh := newFakeGitHub()
	rig := newBindRig(t, gh)
	rig.linkIdentity(t, rig.userID, gh.actorLogin, 0)

	first, state := rig.namedCeremony(t, "acme")
	out := rig.callback(t, first, namedCallbackQuery(state))
	assertOutcome(t, out, "identity_not_linked")
	rig.assertNothingBound(t)
}

// TestManagedBind_IdentityComparesByIDNotLogin: the linked login is stale — the
// admin renamed on GitHub — but the id is theirs, and that is what counts.
func TestManagedBind_IdentityComparesByIDNotLogin(t *testing.T) {
	gh := newFakeGitHub()
	rig := newBindRig(t, gh)
	rig.linkIdentity(t, rig.userID, "octocat-old-name", gh.actorID)
	cookie := rig.ceremony(t)

	out := rig.callback(t, cookie, defaultCallbackQuery(4242))
	if out.Code != http.StatusFound {
		t.Fatalf("callback status=%d outcome=%q, want 302 — a renamed login must not refuse an id that matches",
			out.Code, out.Header().Get("X-TF-Bind-Outcome"))
	}
	if n := rig.installationCount(t); n != 1 {
		t.Errorf("%d installation rows, want 1", n)
	}
}

// --- The named-account leg ----------------------------------------------------

// TestManagedBind_NamedAccount_Organization is the leg for an account that
// already has the App: no install page, no installation_id on the query — the
// admin names the account, GitHub's OAuth authorize proves who they are, and
// the installation is found among the ones that person can see.
func TestManagedBind_NamedAccount_Organization(t *testing.T) {
	gh := newFakeGitHub()
	rig := newBindRig(t, gh)

	rec := rig.connectAccount(t, `{"account":"Acme"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("connect-account status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var body struct {
		AuthorizeURL string `json:"authorize_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	u, err := url.Parse(body.AuthorizeURL)
	if err != nil || !strings.HasPrefix(body.AuthorizeURL, rig.ghBase+"/login/oauth/authorize?") {
		t.Fatalf("authorize_url = %q, want GitHub's OAuth authorize on the deployment's GitHub", body.AuthorizeURL)
	}
	q := u.Query()
	if q.Get("client_id") != gh.clientID {
		t.Errorf("client_id = %q, want the deployment App's %q", q.Get("client_id"), gh.clientID)
	}
	if q.Get("redirect_uri") != "https://tf.test"+ManagedBindCallbackPath {
		t.Errorf("redirect_uri = %q, want the callback", q.Get("redirect_uri"))
	}
	if q.Get("scope") != "" {
		t.Errorf("scope = %q, want none — a GitHub App's user token carries the App's permissions", q.Get("scope"))
	}
	// The name rides the record, never the URL GitHub sees.
	if strings.Contains(strings.ToLower(body.AuthorizeURL), "acme") {
		t.Errorf("the account login leaked into the authorize URL: %s", body.AuthorizeURL)
	}
	cookie := rig.bindCookie(t, rec)
	state := q.Get("state")
	if state == "" {
		t.Fatal("authorize URL carries no state")
	}
	if state == cookie.Value {
		t.Error("state is the raw nonce; it must be the hash, so the URL bar never holds the bearer capability")
	}

	out := rig.callback(t, cookie, namedCallbackQuery(state))
	if out.Code != http.StatusFound {
		t.Fatalf("callback status=%d outcome=%q body=%s, want 302",
			out.Code, out.Header().Get("X-TF-Bind-Outcome"), out.Body.String())
	}
	if !gh.served("/api/v3/user/installations") {
		t.Error("the installation was not looked up among the user's own")
	}
	if !gh.served("/api/v3/app/installations/4242") {
		t.Error("the App did not read the installation it is about to persist")
	}
	if !gh.served("/api/v3/orgs/acme/memberships/octocat") {
		t.Error("the authority gate did not run")
	}
	var accountLogin, accountID string
	if err := rig.h.AdminDB.QueryRow(`
		SELECT account_login, account_id FROM org_github_app_installations
		 WHERE org_id = $1 AND installation_id = '4242'
	`, rig.orgID.String()).Scan(&accountLogin, &accountID); err != nil {
		t.Fatalf("read installation row: %v", err)
	}
	if accountLogin != "acme" || accountID != "700" {
		t.Errorf("installation row = (%q, %q), want the App's own read (acme, 700), not the admin's spelling", accountLogin, accountID)
	}
	if class := rig.credentialClass(t); class != string(domain.GitHubCredentialClassManagedApp) {
		t.Errorf("credential class = %q, want managed_app", class)
	}
}

func TestManagedBind_NamedAccount_UserTarget(t *testing.T) {
	gh := newFakeGitHub()
	gh.accountType, gh.accountLogin, gh.accountID = "User", "octocat", 99
	rig := newBindRig(t, gh)
	cookie, state := rig.namedCeremony(t, "octocat")

	out := rig.callback(t, cookie, namedCallbackQuery(state))
	if out.Code != http.StatusFound {
		t.Fatalf("callback status=%d outcome=%q, want 302", out.Code, out.Header().Get("X-TF-Bind-Outcome"))
	}
	if gh.served("/api/v3/orgs/octocat/memberships/octocat") {
		t.Error("a user target asks GitHub nothing; the account administers itself")
	}
}

// TestManagedBind_NamedAccount_NotInTheListingContinuesToInstall: an account
// with no installation and an account whose installation this person cannot
// see look identical in the listing the answer comes from, and both get the
// same answer — on to GitHub's install page for that account, under a fresh
// record, where GitHub decides what to show. TF says nothing about which it
// was, and reads nothing under the App's key.
func TestManagedBind_NamedAccount_NotInTheListingContinuesToInstall(t *testing.T) {
	for _, tc := range []struct {
		name string
		bend func(gh *fakeGitHub)
	}{
		{"the account has no installation", func(gh *fakeGitHub) {
			gh.installationGone = true
			gh.userInstallations = nil
		}},
		{"the account has one this person cannot see", func(gh *fakeGitHub) {
			gh.userInstallations = []int64{11}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh := newFakeGitHub()
			tc.bend(gh)
			rig := newBindRig(t, gh)
			first, state := rig.namedCeremony(t, "acme")

			out := rig.callback(t, first, namedCallbackQuery(state))
			assertOutcome(t, out, "install_continues")
			if out.Code != http.StatusFound {
				t.Fatalf("status = %d body=%s, want 302", out.Code, out.Body.String())
			}
			if want := rig.ghBase + "/apps/tf-deployment/installations/new/permissions?target_id=700"; out.Header().Get("Location") != want {
				t.Errorf("Location = %q, want %q", out.Header().Get("Location"), want)
			}
			rig.assertNothingBound(t)
			if gh.served("/api/v3/app/installations/4242") {
				t.Error("the App read an installation the person is not associated with")
			}

			// Exactly one new record, for the install leg, on the named
			// account; the authorize leg's own is spent. And exactly one
			// ceremony cookie on the response, carrying the new nonce.
			if !rig.recordConsumed(t, first) {
				t.Error("the authorize leg's record was not spent by the hop")
			}
			var leg, account string
			if err := rig.h.AdminDB.QueryRow(`
				SELECT leg, account_login FROM github_pending_binds
				 WHERE org_id = $1 AND consumed_at IS NULL
			`, rig.orgID.String()).Scan(&leg, &account); err != nil {
				t.Fatalf("read the install-leg record: %v", err)
			}
			if leg != domain.GitHubBindLegInstall || account != "acme" {
				t.Errorf("hop minted (leg %q, account %q), want (install, acme)", leg, account)
			}
			var set []*http.Cookie
			for _, c := range (&http.Response{Header: out.Header()}).Cookies() {
				if c.Name == managedBindCookieName {
					set = append(set, c)
				}
			}
			if len(set) != 1 || set[0].Value == "" || set[0].Value == first.Value {
				t.Errorf("hop response carries %d ceremony cookies (%v); want exactly one, fresh", len(set), set)
			}
		})
	}
}

// TestManagedBind_NamedAccount_UnknownLoginIsRefused: a login that names no
// GitHub account is the one definitive no the hop has — account existence is
// public — and it discloses nothing about accounts that do exist.
func TestManagedBind_NamedAccount_UnknownLoginIsRefused(t *testing.T) {
	gh := newFakeGitHub()
	rig := newBindRig(t, gh)
	first, state := rig.namedCeremony(t, "nobody-here")

	out := rig.callback(t, first, namedCallbackQuery(state))
	assertOutcome(t, out, "account_not_connectable")
	if out.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", out.Code)
	}
	rig.assertNothingBound(t)
	var n int
	if err := rig.h.AdminDB.QueryRow(`SELECT count(*) FROM github_pending_binds WHERE org_id = $1 AND consumed_at IS NULL`, rig.orgID.String()).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d unspent records after a refused hop (%v), want 0", n, err)
	}
}

// TestManagedBind_InstallLeg_AccountMismatch: the install leg binds only an
// installation on the account that was named. The preselection can be changed
// on the way through GitHub, and the person may well own both accounts — it
// still is not the connection they asked for.
func TestManagedBind_InstallLeg_AccountMismatch(t *testing.T) {
	gh := newFakeGitHub()
	rig := newBindRig(t, gh)
	cookie := rig.ceremony(t) // named acme

	gh.accountLogin, gh.accountID = "other-org", 900
	gh.userInstallations = []int64{4242}
	out := rig.callback(t, cookie, defaultCallbackQuery(4242))
	assertOutcome(t, out, "account_mismatch")
	if out.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", out.Code)
	}
	for _, want := range []string{"other-org", "acme"} {
		if !strings.Contains(out.Body.String(), want) {
			t.Errorf("the refusal must name both accounts; body lacks %q", want)
		}
	}
	rig.assertNothingBound(t)
}

// TestManagedBind_HopRequiresIdentity: the install page is offered only once
// the person is proven to be the linked account. A code that is somebody
// else's stops here, with no second record and no redirect.
func TestManagedBind_HopRequiresIdentity(t *testing.T) {
	gh := newFakeGitHub()
	gh.userInstallations = nil
	rig := newBindRig(t, gh)
	first, state := rig.namedCeremony(t, "acme")
	gh.actorLogin, gh.actorID = "mallory", 666

	out := rig.callback(t, first, namedCallbackQuery(state))
	assertOutcome(t, out, "identity_mismatch")
	if out.Code == http.StatusFound {
		t.Error("an unproven person was sent on to GitHub's install page")
	}
	rig.assertNothingBound(t)
	var n int
	if err := rig.h.AdminDB.QueryRow(`SELECT count(*) FROM github_pending_binds WHERE org_id = $1 AND consumed_at IS NULL`, rig.orgID.String()).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d unspent records after a refused authorize leg (%v), want 0", n, err)
	}
}

func TestManagedBind_NamedAccount_Refusals(t *testing.T) {
	cases := []struct {
		name string
		// bend shapes the fake before the rig links the admin's identity to
		// its actor; afterRig shapes it after, for a case about the actor
		// being somebody else.
		bend     func(gh *fakeGitHub)
		afterRig func(gh *fakeGitHub)
		query    func(state string) string
		outcome  string
	}{
		{
			name:    "state is not this ceremony's — a code from somebody else's authorize",
			query:   func(string) string { return namedCallbackQuery("not-our-state") },
			outcome: "state_mismatch",
		},
		{
			name:    "state absent",
			query:   func(string) string { return "code=gh_code" },
			outcome: "state_mismatch",
		},
		{
			name:    "the person declined on GitHub's authorize page — no code",
			query:   func(state string) string { return "state=" + url.QueryEscape(state) + "&error=access_denied" },
			outcome: "identity_unproven",
		},
		{
			name:    "token exchange fails",
			bend:    func(gh *fakeGitHub) { gh.tokenExchangeStatus = http.StatusBadRequest },
			outcome: "identity_unproven",
		},
		{
			name:     "the authorizing account is not the admin's linked one",
			afterRig: func(gh *fakeGitHub) { gh.actorLogin, gh.actorID = "mallory", 666 },
			outcome:  "identity_mismatch",
		},
		{
			name:    "the person sees the installation but is only a member",
			bend:    func(gh *fakeGitHub) { gh.membershipRole = "member" },
			outcome: "not_account_admin",
		},
		{
			name:    "billing manager is not an admin",
			bend:    func(gh *fakeGitHub) { gh.membershipRole = "billing_manager" },
			outcome: "not_account_admin",
		},
		{
			name:    "the membership read is refused — the App lost members:read",
			bend:    func(gh *fakeGitHub) { gh.membershipStatus = http.StatusForbidden },
			outcome: "verification_failed",
		},
		{
			name: "the App's read names a different account than the lookup found",
			bend: func(gh *fakeGitHub) {
				gh.userInstallationAccounts = map[int64]string{4242: "acme"}
				gh.accountLogin = "someone-else"
			},
			outcome: "installation_unreadable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gh := newFakeGitHub()
			if tc.bend != nil {
				tc.bend(gh)
			}
			rig := newBindRig(t, gh)
			if tc.afterRig != nil {
				tc.afterRig(gh)
			}
			cookie, state := rig.namedCeremony(t, "acme")
			query := namedCallbackQuery(state)
			if tc.query != nil {
				query = tc.query(state)
			}
			out := rig.callback(t, cookie, query)
			assertOutcome(t, out, tc.outcome)
			rig.assertNothingBound(t)
		})
	}
}

// TestManagedBind_NamedAccount_QueryInstallationIsIgnored: the record decides
// the leg. A callback into a named ceremony that also carries an
// installation_id — the shape a planted install-leg URL has — binds the named
// account the person can see, never the id on the query string.
func TestManagedBind_NamedAccount_QueryInstallationIsIgnored(t *testing.T) {
	gh := newFakeGitHub()
	rig := newBindRig(t, gh)
	cookie, state := rig.namedCeremony(t, "acme")

	out := rig.callback(t, cookie, namedCallbackQuery(state)+"&installation_id=9999&setup_action=install")
	if out.Code != http.StatusFound {
		t.Fatalf("callback status=%d outcome=%q, want 302", out.Code, out.Header().Get("X-TF-Bind-Outcome"))
	}
	if gh.served("/api/v3/app/installations/9999") {
		t.Error("the query string's installation_id was read; the named leg must ignore it")
	}
	var n int
	if err := rig.h.AdminDB.QueryRow(`
		SELECT count(*) FROM org_github_app_installations WHERE org_id = $1 AND installation_id = '4242'
	`, rig.orgID.String()).Scan(&n); err != nil || n != 1 {
		t.Errorf("named account's installation rows = %d (%v), want 1", n, err)
	}
}

func TestManagedBind_NamedAccount_BoundElsewhere(t *testing.T) {
	gh := newFakeGitHub()
	rig := newBindRig(t, gh)
	other := rig.sibling(t, "other")
	if out := other.callback(t, other.ceremony(t), defaultCallbackQuery(4242)); out.Code != http.StatusFound {
		t.Fatalf("seed the other workspace's bind: %d", out.Code)
	}

	cookie, state := rig.namedCeremony(t, "acme")
	out := rig.callback(t, cookie, namedCallbackQuery(state))
	assertOutcome(t, out, "bound_elsewhere")
	rig.assertNothingBound(t)
	if strings.Contains(out.Body.String(), other.orgID.String()) {
		t.Error("the refusal named the other workspace")
	}
}

// TestManagedBind_NamedAccount_Start pins the start's own contract: the
// account is validated as a login before it goes anywhere, the request must
// come from the admin's own page, and a workspace that already holds a
// credential is refused here before anyone is sent to GitHub.
func TestManagedBind_NamedAccount_Start(t *testing.T) {
	t.Run("MalformedLoginIsABadField", func(t *testing.T) {
		rig := newBindRig(t, newFakeGitHub())
		for _, bad := range []string{`{"account":""}`, `{"account":"-acme"}`, `{"account":"acme-"}`,
			`{"account":"a--b"}`, `{"account":"acme/../x"}`, `{"account":"` + strings.Repeat("a", 40) + `"}`,
			`{"account":"a b"}`, `{}`} {
			rec := rig.connectAccount(t, bad)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s → %d, want 400", bad, rec.Code)
			}
			if !strings.Contains(rec.Body.String(), `"field":"account"`) {
				t.Errorf("%s → body lacks the field name: %s", bad, rec.Body.String())
			}
		}
		if rec := rig.connectAccount(t, `{"account":"acme","extra":1}`); rec.Code != http.StatusBadRequest {
			t.Errorf("unknown field → %d, want 400", rec.Code)
		}
		// The longest login GitHub allows, with single hyphens inside it, is
		// a login: the cap is 39 and the hyphen rule is about runs.
		if rec := rig.connectAccount(t, `{"account":"`+strings.Repeat("ab-", 12)+`abc"}`); rec.Code != http.StatusOK {
			t.Errorf("a 39-character login with single hyphens → %d body=%s, want 200", rec.Code, rec.Body.String())
		}
	})

	t.Run("RequiresTheAdminsOwnOrigin", func(t *testing.T) {
		rig := newBindRig(t, newFakeGitHub())
		req := httptest.NewRequest("POST", "/api/orgs/"+rig.orgID.String()+"/github/managed/connect-account",
			strings.NewReader(`{"account":"acme"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "https://attacker.test")
		req.AddCookie(&http.Cookie{Name: rig.srv.sidCookieName(), Value: rig.sid})
		rec := httptest.NewRecorder()
		rig.srv.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("cross-origin start = %d, want 403; a page the admin did not load must not choose the account", rec.Code)
		}
		var n int
		if err := rig.h.AdminDB.QueryRow(`SELECT count(*) FROM github_pending_binds WHERE org_id = $1`, rig.orgID.String()).Scan(&n); err != nil || n != 0 {
			t.Errorf("%d pending binds minted by a refused start (%v), want 0", n, err)
		}
	})

	t.Run("RequiresWorkspaceAdmin", func(t *testing.T) {
		rig := newBindRig(t, newFakeGitHub())
		member := rig.seedUser()
		if _, err := rig.h.AdminDB.Exec(
			`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`,
			member, rig.orgID.String()); err != nil {
			t.Fatalf("insert org_membership: %v", err)
		}
		resp, _ := rig.driveCallback(member)
		req := httptest.NewRequest("POST", "/api/orgs/"+rig.orgID.String()+"/github/managed/connect-account",
			strings.NewReader(`{"account":"acme"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", rig.srv.deployCfg.publicURL)
		req.AddCookie(&http.Cookie{Name: rig.srv.sidCookieName(), Value: rig.sidFromResp(resp)})
		rec := httptest.NewRecorder()
		rig.srv.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("member start = %d, want 403", rec.Code)
		}
	})

	t.Run("RefusesAWorkspaceThatHoldsAPAT", func(t *testing.T) {
		gh := newFakeGitHub()
		rig := newBindRig(t, gh)
		if err := rig.srv.tx.WithTx(context.Background(), rig.orgID.String(), rig.userID.String(),
			func(tx db.TxStores) error {
				return tx.Secrets.Put(context.Background(), rig.orgID.String(), integrations.KeyGitHubPAT, "ghp_live", "test PAT")
			}); err != nil {
			t.Fatalf("seed org PAT: %v", err)
		}
		rec := rig.connectAccount(t, `{"account":"acme"}`)
		if rec.Code != http.StatusConflict {
			t.Errorf("start with a PAT bound = %d body=%s, want 409", rec.Code, rec.Body.String())
		}
		assertOutcome(t, rec, "credential_pat_in_use")
		if !strings.Contains(rec.Body.String(), `"reason":"CREDENTIAL_PAT_IN_USE"`) {
			t.Errorf("JSON refusal lacks the upper-cased reason: %s", rec.Body.String())
		}
		var n int
		if err := rig.h.AdminDB.QueryRow(`SELECT count(*) FROM github_pending_binds WHERE org_id = $1`, rig.orgID.String()).Scan(&n); err != nil || n != 0 {
			t.Errorf("%d pending binds minted by a refused start (%v), want 0", n, err)
		}
	})
}

// --- Mode gate -------------------------------------------------------------

func TestManagedBind_LocalModeIs404(t *testing.T) {
	// A distributed local binary ships no shared App key, so the ceremony does
	// not exist there — and a route that does not exist in this deployment mode
	// answers like one that does not exist at all.
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	s.SetDeployConfig("http://localhost:3000", key)

	for _, rt := range []struct{ method, path string }{
		{"POST", "/api/orgs/" + runmode.LocalDefaultOrgID + "/github/managed/connect-account"},
		{"GET", ManagedBindCallbackPath + "?code=x&installation_id=1"},
	} {
		rec := doJSON(t, s, rt.method, rt.path, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s in local mode = %d, want 404", rt.method, rt.path, rec.Code)
		}
	}
}
