package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
)

// The deployment-App tier, end to end against a fake GitHub.
//
// Every fixture here has a perfectly usable PAT sitting in the secret store,
// and none of these tests is allowed to reach it. That is the assertion the
// whole tier turns on: a managed workspace never chose a PAT, so resolving one
// on its behalf — on any failure arm, for any reason — would act on a
// credential the workspace did not pick. The old "no org_github_apps row means
// PAT" inference would have found that PAT every time.

const (
	deploymentSlug      = "tf-deployment"
	deploymentBotUserID = int64(424242)
)

// deploymentGH is the fake GitHub the deployment App authenticates against: the
// three endpoints tier 2 touches, each with a switch for the failure the
// corresponding arm has to refuse on.
type deploymentGH struct {
	srv *httptest.Server

	// GET /app — the preflight. status 0 means 200.
	appStatus      int
	appPermissions map[string]string
	appCalls       int32

	// GET /users/{login} — the bot account id. status 0 means 200.
	botStatus int
	botCalls  int32

	// POST /app/installations/{id}/access_tokens
	mintCalls int32
	lastAuth  string // Authorization header seen on the /probe call
}

func newDeploymentGH(t *testing.T) *deploymentGH {
	t.Helper()
	g := &deploymentGH{appPermissions: map[string]string{"members": "read", "contents": "write"}}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/v3/app", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&g.appCalls, 1)
		if g.appStatus != 0 {
			w.WriteHeader(g.appStatus)
			_, _ = w.Write([]byte(`{"message":"nope"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          987,
			"slug":        deploymentSlug,
			"client_id":   "Iv1.deployment",
			"owner":       map[string]any{"login": "tf-inc", "type": "Organization"},
			"permissions": g.appPermissions,
		})
	})

	mux.HandleFunc("/api/v3/users/", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&g.botCalls, 1)
		if g.botStatus != 0 {
			w.WriteHeader(g.botStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": deploymentBotUserID})
	})

	mux.HandleFunc("/api/v3/app/installations/", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&g.mintCalls, 1)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_deployment_minted",
			"expires_at": time.Now().Add(time.Hour).UTC(),
		})
	})

	mux.HandleFunc("/api/v3/probe", func(w http.ResponseWriter, r *http.Request) {
		g.lastAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("[]"))
	})

	// The repo-coverage probe ClientForRepo runs: everything is in the grant.
	mux.HandleFunc("/api/v3/repos/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"full_name":"` + strings.TrimPrefix(r.URL.Path, "/api/v3/repos/") + `"}`))
	})

	g.srv = httptest.NewServer(mux)
	// This fake is the deployment's GitHub: the deployment App is registered
	// on it, and every managed org in these tests is on it.
	ghbase.SetDefaultBaseURLForTest(t, g.srv.URL)
	t.Cleanup(g.srv.Close)
	return g
}

// testDeploymentApp builds a configured deployment App. Its key is generated
// per call rather than shared, because nothing here checks a signature and a
// shared one would be a fixture two tests could quietly couple through.
func testDeploymentApp(t *testing.T) githubapp.DeploymentApp {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return githubapp.DeploymentApp{
		AppID:         987,
		PrivateKey:    key,
		WebhookSecret: "whsec_deployment",
		ClientSecret:  "cs_deployment",
	}
}

// newManagedResolver wires a managed-class org: no org_github_apps row (a
// managed org cannot have one), one bound installation, a live PAT in the
// secret store that nothing may reach, and the deployment App.
func newManagedResolver(t *testing.T, base string, app githubapp.DeploymentApp) Resolver {
	t.Helper()
	// The org's GitHub is the deployment's GitHub — the one the deployment App
	// is on — which is the only shape a managed org can have.
	ghbase.SetDefaultBaseURLForTest(t, base)
	return NewResolver(
		&fakeSecrets{vals: map[string]string{integrations.KeyGitHubPAT: "ghp_never_borrow_me"}},
		&fakeApps{app: nil, insts: []domain.OrgGitHubAppInstallation{installOn("acme")}},
		&fakeOrgs{base: base, class: domain.GitHubCredentialClassManagedApp},
		&fakeAgents{},
		nil,
		WithDeploymentApp(app),
	)
}

// TestResolver_ManagedClass_MintsFromDeploymentApp is the tier's positive
// statement: an org with NO registration row of its own resolves a client and a
// token, both minted from the deployment App's key.
func TestResolver_ManagedClass_MintsFromDeploymentApp(t *testing.T) {
	gh := newDeploymentGH(t)
	r := newManagedResolver(t, gh.srv.URL, testDeploymentApp(t))
	ctx := context.Background()

	client, err := r.ClientFor(ctx, "org-1", "acme")
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if _, err := client.Get(ctx, "/probe"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if gh.lastAuth != "Bearer ghs_deployment_minted" {
		t.Errorf("client carried %q; want the token minted from the deployment App", gh.lastAuth)
	}

	tok, err := r.TokenFor(ctx, "org-1", "acme")
	if err != nil {
		t.Fatalf("TokenFor: %v", err)
	}
	if tok.Value != "ghs_deployment_minted" {
		t.Errorf("TokenFor = %q; want the minted installation token", tok.Value)
	}
	if tok.ExpiresAt.IsZero() {
		t.Error("TokenFor returned a token with no expiry; an installation token carries GitHub's ~1h expiry, a PAT is what has none")
	}

	// The preflight is established once and cached, not re-run per resolution.
	if got := atomic.LoadInt32(&gh.appCalls); got != 1 {
		t.Errorf("GET /app called %d times across two resolutions; want 1 — the preflight answer is cached", got)
	}
}

// TestResolver_EveryCredentialClass walks all three classes through one entry
// point, which is where the classes are easiest to read side by side: each one
// resolves a different credential, and two of the three have no registration
// row to tell them apart by.
func TestResolver_EveryCredentialClass(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		class domain.GitHubCredentialClass
		app   *domain.OrgGitHubApp
		want  string // the Authorization header the resolved client carries
	}{
		{"pat", domain.GitHubCredentialClassPAT, nil, "Bearer ghp_never_borrow_me"},
		{"byo app", domain.GitHubCredentialClassBYOApp, activeApp(), "Bearer ghs_deployment_minted"},
		// No row, and the credential is still an App token — which is exactly
		// what a row-presence check could never have worked out.
		{"managed app", domain.GitHubCredentialClassManagedApp, nil, "Bearer ghs_deployment_minted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh := newDeploymentGH(t)
			r := NewResolver(
				&fakeSecrets{vals: map[string]string{
					integrations.KeyGitHubPAT: "ghp_never_borrow_me",
					"pem":                     testPEM(t),
				}},
				&fakeApps{app: tc.app, insts: []domain.OrgGitHubAppInstallation{installOn("acme")}},
				&fakeOrgs{base: gh.srv.URL, class: tc.class},
				&fakeAgents{},
				nil,
				WithDeploymentApp(testDeploymentApp(t)),
			)

			client, err := r.ClientFor(ctx, "org-1", "acme")
			if err != nil {
				t.Fatalf("ClientFor: %v", err)
			}
			if _, err := client.Get(ctx, "/probe"); err != nil {
				t.Fatalf("probe: %v", err)
			}
			if gh.lastAuth != tc.want {
				t.Errorf("client carried %q; want %q", gh.lastAuth, tc.want)
			}
			// Only the managed arm establishes the deployment App's identity.
			wantPreflight := int32(0)
			if tc.class == domain.GitHubCredentialClassManagedApp {
				wantPreflight = 1
			}
			if got := atomic.LoadInt32(&gh.appCalls); got != wantPreflight {
				t.Errorf("GET /app called %d times for class %q; want %d", got, tc.class, wantPreflight)
			}
		})
	}
}

// TestResolver_ManagedClass_EveryEntryPoint walks the whole Resolver surface,
// because a tier that works on ClientFor and not on the scoped mints is a tier
// that fails inside a delegated run instead of at a request.
func TestResolver_ManagedClass_EveryEntryPoint(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		call func(r Resolver) (string, error)
	}{
		{"ClientForRepo", func(r Resolver) (string, error) {
			c, err := r.ClientForRepo(ctx, "org-1", "acme", "widget")
			if err != nil {
				return "", err
			}
			_, err = c.Get(ctx, "/probe")
			return "", err
		}},
		{"TokenForRepoScoped", func(r Resolver) (string, error) {
			tok, err := r.(ScopedResolver).TokenForRepoScoped(ctx, "org-1", "acme", "widget", nil)
			return tok.Value, err
		}},
		{"TokenForReposScoped", func(r Resolver) (string, error) {
			tok, err := r.(ScopedResolver).TokenForReposScoped(ctx, "org-1", "acme", []string{"widget"}, nil)
			return tok.Value, err
		}},
		{"ClientForRepoScoped", func(r Resolver) (string, error) {
			c, ident, err := r.(ScopedRepoResolver).ClientForRepoScoped(ctx, "org-1", "acme", "widget", nil)
			if err != nil {
				return "", err
			}
			if ident != IdentityApp {
				t.Errorf("identity = %v; want IdentityApp — a deployment App token is a bot acting as itself", ident)
			}
			_, err = c.Get(ctx, "/probe")
			return "", err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh := newDeploymentGH(t)
			r := newManagedResolver(t, gh.srv.URL, testDeploymentApp(t))
			tok, err := tc.call(r)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if tok != "" && tok != "ghs_deployment_minted" {
				t.Errorf("%s returned %q; want the minted deployment-App token", tc.name, tok)
			}
			if gh.lastAuth != "" && gh.lastAuth != "Bearer ghs_deployment_minted" {
				t.Errorf("%s built a client carrying %q; want the minted deployment-App token", tc.name, gh.lastAuth)
			}
		})
	}

	t.Run("HasAnyCredential", func(t *testing.T) {
		gh := newDeploymentGH(t)
		r := newManagedResolver(t, gh.srv.URL, testDeploymentApp(t))
		ok, err := r.(ScopedResolver).HasAnyCredential(ctx, "org-1")
		if err != nil || !ok {
			t.Fatalf("HasAnyCredential = (%v, %v); want (true, nil) — the org has a bound installation on the shared App", ok, err)
		}
	})
}

// TestResolver_ManagedClass_NoDeploymentApp_RefusesAndNeverBorrowsThePAT is the
// most important test in the tier.
//
// A deployment that configured no shared App has nothing for a managed org to
// mint from — and the org's secret store still holds a working PAT. Resolving
// it would be the exact inference bug the credential class was added to kill,
// wearing a different hat: the org's rowlessness would once again be read as
// "PAT org".
func TestResolver_ManagedClass_NoDeploymentApp_RefusesAndNeverBorrowsThePAT(t *testing.T) {
	gh := newDeploymentGH(t)
	// The zero App: configured nowhere, which is also what every local-mode
	// process holds.
	r := newManagedResolver(t, gh.srv.URL, githubapp.DeploymentApp{})

	assertManagedRefusal(t, r, gh, "no deployment app configured")

	if !errors.Is(refusalErr(t, r), githubapp.ErrNoDeploymentApp) {
		t.Error("the refusal did not name the missing deployment App; the operator has to be able to tell it from a GitHub outage")
	}
}

// TestResolver_ManagedClass_PreflightFailure_RefusesAndNeverBorrowsThePAT
// asserts each failure arm separately, because they are three different
// operator faults and only a per-arm assertion proves none of them falls
// through.
func TestResolver_ManagedClass_PreflightFailure_RefusesAndNeverBorrowsThePAT(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(gh *deploymentGH)
		cause error
	}{
		{
			// GitHub 401s a JWT whose iss does not match the signing key: the
			// App ID and the key are not a pair.
			name:  "key rejected",
			setup: func(gh *deploymentGH) { gh.appStatus = http.StatusUnauthorized },
			cause: githubapp.ErrDeploymentAppKeyRejected,
		},
		{
			// Any other non-2xx is a statement about GitHub, not a verdict on
			// the configuration — and still refuses.
			name:  "unreachable",
			setup: func(gh *deploymentGH) { gh.appStatus = http.StatusInternalServerError },
			cause: githubapp.ErrDeploymentAppUnreachable,
		},
		{
			// members is the permission the bind ceremony's authority gate reads
			// AND what restricts installation to org owners. An App without it
			// must serve nobody.
			name:  "members permission missing",
			setup: func(gh *deploymentGH) { gh.appPermissions = map[string]string{"contents": "write"} },
			cause: githubapp.ErrDeploymentAppMembersPermission,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh := newDeploymentGH(t)
			tc.setup(gh)
			r := newManagedResolver(t, gh.srv.URL, testDeploymentApp(t))

			assertManagedRefusal(t, r, gh, tc.name)
			if !errors.Is(refusalErr(t, r), tc.cause) {
				t.Errorf("refusal did not name %v; the three faults want three different things from an operator", tc.cause)
			}
		})
	}
}

// refusalErr returns the error ClientFor gives for the fixture's managed org.
func refusalErr(t *testing.T, r Resolver) error {
	t.Helper()
	_, err := r.ClientFor(context.Background(), "org-1", "acme")
	if err == nil {
		t.Fatal("ClientFor resolved something for a managed org with an unusable deployment App")
	}
	return err
}

// assertManagedRefusal pins the shape of every managed-arm refusal: every entry
// point errors, the error is ErrDeploymentAppUnavailable, it is NOT
// ErrNoGitHubCredentials (the workspace has nothing to reconnect — the fault is
// the deployment's), and no credential of any kind comes back.
func assertManagedRefusal(t *testing.T, r Resolver, gh *deploymentGH, when string) {
	t.Helper()
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		call func() (string, error)
	}{
		{"ClientFor", func() (string, error) { _, err := r.ClientFor(ctx, "org-1", "acme"); return "", err }},
		{"ClientForRepo", func() (string, error) { _, err := r.ClientForRepo(ctx, "org-1", "acme", "widget"); return "", err }},
		{"TokenFor", func() (string, error) { tok, err := r.TokenFor(ctx, "org-1", "acme"); return tok.Value, err }},
		{"TokenForRepoScoped", func() (string, error) {
			tok, err := r.(ScopedResolver).TokenForRepoScoped(ctx, "org-1", "acme", "widget", nil)
			return tok.Value, err
		}},
		{"TokenForReposScoped", func() (string, error) {
			tok, err := r.(ScopedResolver).TokenForReposScoped(ctx, "org-1", "acme", []string{"widget"}, nil)
			return tok.Value, err
		}},
		{"ClientForRepoScoped", func() (string, error) {
			_, _, err := r.(ScopedRepoResolver).ClientForRepoScoped(ctx, "org-1", "acme", "widget", nil)
			return "", err
		}},
		{"HasAnyCredential", func() (string, error) {
			ok, err := r.(ScopedResolver).HasAnyCredential(ctx, "org-1")
			if err == nil && ok {
				t.Error("HasAnyCredential said yes for a managed org with no usable deployment App; the PAT in the store is not its credential")
			}
			return "", err
		}},
	} {
		tok, err := tc.call()
		if err == nil {
			t.Errorf("%s (%s): resolved instead of refusing", tc.name, when)
			continue
		}
		if !errors.Is(err, ErrDeploymentAppUnavailable) {
			t.Errorf("%s (%s): err = %v; want ErrDeploymentAppUnavailable", tc.name, when, err)
		}
		if errors.Is(err, ErrNoGitHubCredentials) {
			t.Errorf("%s (%s): the refusal wrapped ErrNoGitHubCredentials; that reads as \"your GitHub is disconnected\" and sends a workspace admin to fix a deployment they do not own", tc.name, when)
		}
		if strings.Contains(tok, "ghp_") {
			t.Errorf("%s (%s): returned the org's PAT (%q) for a managed org — the one thing this tier may never do", tc.name, when, tok)
		}
	}

	// Nothing was minted, on any arm. A refusal that still reached the mint
	// endpoint would mean the preflight is advisory rather than a gate.
	if got := atomic.LoadInt32(&gh.mintCalls); got != 0 {
		t.Errorf("(%s) minted %d times despite refusing", when, got)
	}
}

// TestResolver_ManagedClass_CommitIdentity pins the bot identity a managed
// workspace's commits carry, on both halves of the bot-id question.
//
// The numeric-id form is the only one that links a bot's commits to its account
// on github.com, and the deployment App has no row to hold that id — so it is
// derived during the preflight. When that derivation fails the email degrades
// to the plain form, exactly as an org_github_apps row with a NULL bot_user_id
// does, and never to a failed run.
func TestResolver_ManagedClass_CommitIdentity(t *testing.T) {
	for _, tc := range []struct {
		name      string
		botStatus int
		wantEmail string
	}{
		{"bot lookup succeeds", 0, "424242+tf-deployment[bot]@users.noreply.github.com"},
		{"bot lookup fails", http.StatusNotFound, "tf-deployment[bot]@users.noreply.github.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh := newDeploymentGH(t)
			gh.botStatus = tc.botStatus
			r := newManagedResolver(t, gh.srv.URL, testDeploymentApp(t))

			name, email, ok := r.OrgIdentityFor(context.Background(), "org-1")
			if !ok {
				t.Fatal("OrgIdentityFor reported no identity; a managed org commits as the deployment App's bot")
			}
			if name != "tf-deployment[bot]" {
				t.Errorf("name = %q; want the deployment App's bot login", name)
			}
			if email != tc.wantEmail {
				t.Errorf("email = %q; want %q", email, tc.wantEmail)
			}
		})
	}
}

// TestResolver_ManagedClass_UnusableApp_StampsNoIdentity covers the one entry
// point that cannot return an error. A managed org whose deployment App fails
// its preflight must stamp NOTHING — never the PAT tier's cached login, which
// would be an identity for a credential the workspace is not acting as.
func TestResolver_ManagedClass_UnusableApp_StampsNoIdentity(t *testing.T) {
	gh := newDeploymentGH(t)
	gh.appStatus = http.StatusUnauthorized
	r := NewResolver(
		&fakeSecrets{vals: map[string]string{integrations.KeyGitHubPAT: "ghp_present"}},
		&fakeApps{app: nil},
		&fakeOrgs{base: gh.srv.URL, class: domain.GitHubCredentialClassManagedApp},
		&fakeAgents{agent: &domain.Agent{GitHubOrgLogin: "acme-bot", GitHubOrgEmail: "bot@acme.test"}},
		nil,
		WithDeploymentApp(testDeploymentApp(t)),
	)

	name, email, ok := r.OrgIdentityFor(context.Background(), "org-1")
	if ok || name != "" || email != "" {
		t.Errorf("OrgIdentityFor = (%q, %q, %v); want no identity — the PAT login belongs to a credential this org does not use", name, email, ok)
	}
}

// TestResolver_ManagedClass_PreflightFailureIsCachedBriefly pins both halves of
// the caching trade: a failed preflight is not re-asked on every resolution
// (which is what keeps a GitHub outage from multiplying into one GET /app per
// org per call), and it expires quickly enough that a fixed key does not have
// to wait out the success TTL.
func TestResolver_ManagedClass_PreflightFailureIsCachedBriefly(t *testing.T) {
	gh := newDeploymentGH(t)
	gh.appStatus = http.StatusInternalServerError
	app := testDeploymentApp(t)

	src := newDeploymentAppSource(app)
	now := time.Now()
	src.now = func() time.Time { return now }
	r := NewResolver(
		&fakeSecrets{vals: map[string]string{integrations.KeyGitHubPAT: "ghp_present"}},
		&fakeApps{app: nil, insts: []domain.OrgGitHubAppInstallation{installOn("acme")}},
		&fakeOrgs{base: gh.srv.URL, class: domain.GitHubCredentialClassManagedApp},
		&fakeAgents{},
		nil,
		func(res *resolver) { res.deployment = src },
	)
	ctx := context.Background()

	for range 3 {
		if _, err := r.ClientFor(ctx, "org-1", "acme"); err == nil {
			t.Fatal("ClientFor resolved against a GitHub that never answered")
		}
	}
	if got := atomic.LoadInt32(&gh.appCalls); got != 1 {
		t.Errorf("GET /app called %d times across three failing resolutions; want 1 — a failure is an answer worth caching", got)
	}

	// Past the failure TTL, with GitHub healthy again: the next resolution asks
	// again and succeeds.
	gh.appStatus = 0
	now = now.Add(deploymentAppFailureTTL + time.Second)
	if _, err := r.ClientFor(ctx, "org-1", "acme"); err != nil {
		t.Fatalf("ClientFor after the failure window: %v", err)
	}
	if got := atomic.LoadInt32(&gh.appCalls); got != 2 {
		t.Errorf("GET /app called %d times; want 2 — the cached failure must expire", got)
	}
}

// TestResolver_ManagedClass_NoAppRowIsRead is the structural half of the
// rowless invariant: resolving a managed org must not consult org_github_apps
// for a registration at all. There is no row to find, and a build that looked
// would be one step from inferring PAT when it found nothing.
func TestResolver_ManagedClass_NoAppRowIsRead(t *testing.T) {
	gh := newDeploymentGH(t)
	apps := &countingApps{fakeApps: fakeApps{insts: []domain.OrgGitHubAppInstallation{installOn("acme")}}}
	r := NewResolver(
		&fakeSecrets{vals: map[string]string{integrations.KeyGitHubPAT: "ghp_present"}},
		apps,
		&fakeOrgs{base: gh.srv.URL, class: domain.GitHubCredentialClassManagedApp},
		&fakeAgents{},
		nil,
		WithDeploymentApp(testDeploymentApp(t)),
	)

	if _, err := r.ClientFor(context.Background(), "org-1", "acme"); err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if apps.registrationReads != 0 {
		t.Errorf("read org_github_apps %d times for a managed org; a managed org has no registration row and none can exist", apps.registrationReads)
	}
}

type countingApps struct {
	fakeApps
	registrationReads int
}

func (f *countingApps) GetForOrgSystem(ctx context.Context, orgID string) (*domain.OrgGitHubApp, error) {
	f.registrationReads++
	return f.fakeApps.GetForOrgSystem(ctx, orgID)
}

// TestResolver_BYOPathUnchangedByTierTwo pins the invariant that pays for the
// whole change: an org that brings its own App resolves through the same code
// with the same store reads it always did, and never touches the deployment App
// even when one is configured.
func TestResolver_BYOPathUnchangedByTierTwo(t *testing.T) {
	gh := newDeploymentGH(t)
	orgs := &fakeOrgs{base: gh.srv.URL, class: domain.GitHubCredentialClassBYOApp}
	r := NewResolver(
		&fakeSecrets{vals: map[string]string{"pem": testPEM(t), integrations.KeyGitHubPAT: "ghp_test"}},
		&fakeApps{app: activeApp(), insts: []domain.OrgGitHubAppInstallation{installOn("acme")}},
		orgs,
		&fakeAgents{},
		nil,
		WithDeploymentApp(testDeploymentApp(t)),
	)

	client, err := r.ClientFor(context.Background(), "org-1", "acme")
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if _, err := client.Get(context.Background(), "/probe"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if gh.lastAuth != "Bearer ghs_deployment_minted" {
		// Same fake mint endpoint either way — what matters is which key signed
		// the JWT, and the assertions below are what tell them apart.
		t.Fatalf("client carried %q; want a minted installation token", gh.lastAuth)
	}
	if got := atomic.LoadInt32(&gh.appCalls); got != 0 {
		t.Errorf("GET /app called %d times for a BYO org; the deployment App's preflight is not on its path", got)
	}
	if got := atomic.LoadInt32(&gh.botCalls); got != 0 {
		t.Errorf("the bot-id lookup ran %d times for a BYO org; its bot id comes off its own registration row", got)
	}
	if orgs.settingsReads != 1 {
		t.Errorf("org_settings read %d times; want exactly 1 — tier 2 must not add a read to the BYO path", orgs.settingsReads)
	}
}

// Compile-time proof that the fakes above still satisfy the store interfaces
// the resolver takes, now that one of them is embedded rather than used
// directly.
var _ db.GitHubAppsStore = (*countingApps)(nil)
