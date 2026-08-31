package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
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
	userInstallations []int64

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

	case path == "/api/v3/user/installations":
		ids := make([]string, 0, len(f.userInstallations))
		for _, id := range f.userInstallations {
			ids = append(ids, fmt.Sprintf(`{"id":%d}`, id))
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

	ghBase := gh.start(t)
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

	return &bindRig{authRig: rig, gh: gh, ghBase: ghBase, orgID: org, userID: user, sid: sid}
}

// connect drives the Connect click and returns the response.
func (r *bindRig) connect(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/orgs/"+r.orgID.String()+"/github/managed/connect", nil)
	req.AddCookie(&http.Cookie{Name: r.srv.sidCookieName(), Value: r.sid})
	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, req)
	return rec
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

// callback drives the GitHub return leg with the given cookie and query.
func (r *bindRig) callback(t *testing.T, cookie *http.Cookie, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", ManagedBindCallbackPath+"?"+query, nil)
	req.AddCookie(&http.Cookie{Name: r.srv.sidCookieName(), Value: r.sid})
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, req)
	return rec
}

// ceremony runs Connect and returns the cookie it minted.
func (r *bindRig) ceremony(t *testing.T) *http.Cookie {
	t.Helper()
	rec := r.connect(t)
	if rec.Code != http.StatusFound {
		t.Fatalf("connect status=%d body=%s, want 302", rec.Code, rec.Body.String())
	}
	return r.bindCookie(t, rec)
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

	rec := rig.connect(t)
	if rec.Code != http.StatusFound {
		t.Fatalf("connect status=%d body=%s, want 302", rec.Code, rec.Body.String())
	}
	if want := rig.ghBase + "/apps/tf-deployment/installations/new"; rec.Header().Get("Location") != want {
		t.Errorf("connect redirect = %q, want the deployment App's install page %q",
			rec.Header().Get("Location"), want)
	}
	cookie := rig.bindCookie(t, rec)

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

			// The preflight gates Connect too, so an App with no members
			// permission never reaches a callback — the refusal is asserted
			// wherever the ceremony actually stops.
			rec := rig.connect(t)
			if rec.Code != http.StatusFound {
				assertOutcome(t, rec, tc.outcome)
				rig.assertNothingBound(t)
				return
			}
			query := tc.query
			if query == "" {
				query = defaultCallbackQuery(4242)
			}
			out := rig.callback(t, rig.bindCookie(t, rec), query)
			assertOutcome(t, out, tc.outcome)
			rig.assertNothingBound(t)
		})
	}
}

func TestManagedBind_NoCookieIsTheUnboundInstall(t *testing.T) {
	// Somebody installed the deployment App from its public page, so GitHub
	// returned them here with no ceremony behind it. That is an ordinary state,
	// not an error: the installation exists and belongs to no workspace.
	gh := newFakeGitHub()
	rig := newBindRig(t, gh)

	out := rig.callback(t, nil, defaultCallbackQuery(4242))
	if out.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — a recordless callback is not an error", out.Code)
	}
	assertOutcome(t, out, "unbound")
	rig.assertNothingBound(t)
	if gh.served("/login/oauth/access_token") {
		t.Error("the code was exchanged for a callback with no ceremony behind it")
	}
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

func TestManagedBind_SessionIsNotTheInitiator(t *testing.T) {
	gh := newFakeGitHub()
	rig := newBindRig(t, gh)
	cookie := rig.ceremony(t)

	// A second admin of the same workspace, arriving with the first one's
	// cookie. The cookie proves a browser; the record proves the person.
	other := rig.seedUser()
	if _, err := rig.h.AdminDB.Exec(
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'admin')`,
		other, rig.orgID.String()); err != nil {
		t.Fatalf("insert org_membership: %v", err)
	}
	resp, _ := rig.driveCallback(other)
	rig.sid = rig.sidFromResp(resp)

	out := rig.callback(t, cookie, defaultCallbackQuery(4242))
	assertOutcome(t, out, "link_expired")
	rig.assertNothingBound(t)
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
	cookie := rig.ceremony(t)

	// The role is read again at the callback rather than trusted from the
	// record, because minutes have passed since the click.
	if _, err := rig.h.AdminDB.Exec(
		`UPDATE org_memberships SET role = 'member' WHERE user_id = $1 AND org_id = $2`,
		admin, rig.orgID.String()); err != nil {
		t.Fatalf("demote admin: %v", err)
	}

	out := rig.callback(t, cookie, defaultCallbackQuery(4242))
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

	rec := rig.connect(t)
	if rec.Code != http.StatusForbidden {
		t.Errorf("connect as a workspace member status=%d, want 403", rec.Code)
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

		rec := rig.connect(t)
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

		assertOutcome(t, rig.connect(t), "credential_pat_in_use")
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
	cookie := rig.bindCookie(t, rig.connect(t))

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
		refuseNoOAuthSetting,
		refuseStaleCeremony,
		refuseNoInstallation,
		refuseNotWorkspaceAdmin,
		refuseIdentityUnproven,
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

	for _, path := range []string{
		"/api/orgs/" + runmode.LocalDefaultOrgID + "/github/managed/connect",
		ManagedBindCallbackPath + "?code=x&installation_id=1",
	} {
		rec := doJSON(t, s, "GET", path, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s in local mode = %d, want 404", path, rec.Code)
		}
	}
}
