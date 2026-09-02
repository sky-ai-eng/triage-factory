package dbtest

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
)

// GitHubManagedRefreshSeeder stages the rows the managed-refresh suite needs.
// None of them has a store method that fits (row creation is a provisioning
// concern, and reading soft-removed rows is deliberately not a store read), so
// each backend implements them against its own schema.
type GitHubManagedRefreshSeeder struct {
	// User inserts a user row and returns its ID.
	User func(t *testing.T) string

	// Org inserts an org row owned by ownerID and returns its ID. ownerID must
	// already exist (see User); backends whose orgs table has no owner column
	// may ignore it.
	Org func(t *testing.T, ownerID string) string

	// Class records the org's GitHub credential class and points its GitHub
	// base URL at baseURL — the two things the managed reconcile reads before it
	// will talk to GitHub at all.
	Class func(t *testing.T, orgID string, class domain.GitHubCredentialClass, baseURL string)

	// AllInstallationIDs returns every installation_id the org holds, REMOVED
	// ROWS INCLUDED, ascending. The suite's central claim is that another
	// tenant's installation is absent rather than present-and-removed, and no
	// store read can distinguish those two — every one of them filters
	// removed_at IS NULL.
	AllInstallationIDs func(t *testing.T, orgID string) []string

	// Reach records that installationID reaches one repository under the
	// managed class — a reachable_repositories entry plus its reachable_scopes
	// marker — written at the table so the suite can stage reach for any org
	// (the SQLite store's own writer admits only the local sentinel org).
	Reach func(t *testing.T, orgID, installationID string)

	// ReachRows counts what Reach wrote that is still there: the installation's
	// reachable_repositories entries and its reachable_scopes markers. Both
	// must go with the installation on an uninstall.
	ReachRows func(t *testing.T, orgID, installationID string) (entries, scopes int)
}

// GitHubManagedRefreshFactory is what a per-backend test file hands to
// RunGitHubManagedRefreshConformance. Each call returns a fresh, isolated
// backend so subtests don't leak rows into one another.
type GitHubManagedRefreshFactory func(t *testing.T) (db.GitHubAppsStore, GitHubManagedRefreshSeeder)

// RunGitHubManagedRefreshConformance is the shared assertion suite for
// RefreshManagedInstallations — the reconcile of a workspace that rides the
// DEPLOYMENT App, the one App key that serves many workspaces.
//
// The invariant it exists to pin is one sentence: for the managed class the
// reconcile REFRESHES; it never DISCOVERS. It may update rows that already
// exist and may never create one. Discovery belongs exclusively to the bind
// ceremony, which is the only thing that can assert an installation is this
// workspace's — the listing cannot, because under a shared key it is every
// tenant's listing.
//
// What that leaves the refresh doing is real and worth keeping: the account
// login and id, the suspension pair and repository_selection converge on bound
// rows from a listing no webhook can be relied on to duplicate, and a bound
// installation GitHub stops reporting is soft-removed. The reachable-repo
// cascade that rides that removal is pinned where it lives, on
// MarkInstallationRemoved, in the reachable-repos suite.
func RunGitHubManagedRefreshConformance(t *testing.T, mk GitHubManagedRefreshFactory) {
	t.Helper()
	ctx := context.Background()

	// One key for the whole suite: generating an RSA key is the most expensive
	// thing here by an order of magnitude, and every subtest wants the same
	// deployment App rather than its own.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate deployment app key: %v", err)
	}
	deployment := githubapp.DeploymentApp{
		AppID:         42,
		PrivateKey:    key,
		WebhookSecret: "wh",
		ClientSecret:  "cs",
	}

	// listing renders GET /app/installations as GitHub does. The three-account
	// answer is the suite's fixture: one installation the org under reconcile
	// bound, and two belonging to other tenants of the same shared App.
	type acct struct {
		id    int64
		login string
	}
	listing := func(accts ...acct) string {
		out := "["
		for i, a := range accts {
			if i > 0 {
				out += ","
			}
			out += fmt.Sprintf(`{"id":%d,"account":{"id":%d,"login":%q,"type":"Organization"},`+
				`"repository_selection":"selected"}`, a.id, a.id*10, a.login)
		}
		return out + "]"
	}

	// fakeGitHub stands up the App API the reconcile reads. It counts requests
	// so a subtest can assert the call was never made, and serves whatever body
	// the caller set. The GHES path mount is what ghbase.APIBase derives for any
	// base URL that is not github.com, which every httptest server is.
	fakeGitHub := func(t *testing.T, status int, body *string) (baseURL string, calls *atomic.Int64) {
		t.Helper()
		calls = &atomic.Int64{}
		mux := http.NewServeMux()
		mux.HandleFunc("GET /api/v3/app/installations", func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if r.Header.Get("Authorization") == "" {
				t.Errorf("listing request carried no App JWT")
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			fmt.Fprint(w, *body)
		})
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		return srv.URL, calls
	}

	// bind writes the row the bind ceremony would have written, which is the
	// only way an installation ever becomes this workspace's.
	bind := func(t *testing.T, store db.GitHubAppsStore, orgID, installationID, host, login string) {
		t.Helper()
		if _, err := store.UpsertInstallation(ctx, domain.OrgGitHubAppInstallation{
			InstallationID: installationID,
			OrgID:          orgID,
			AccountType:    "Organization",
			AccountLogin:   login,
			GitHubHost:     host,
		}); err != nil {
			t.Fatalf("UpsertInstallation (bind %s): %v", installationID, err)
		}
	}

	liveIDs := func(t *testing.T, store db.GitHubAppsStore, orgID string) []string {
		t.Helper()
		insts, err := store.ListInstallationsForOrgSystem(ctx, orgID)
		if err != nil {
			t.Fatalf("ListInstallationsForOrgSystem: %v", err)
		}
		out := make([]string, 0, len(insts))
		for _, inst := range insts {
			out = append(out, inst.InstallationID)
		}
		return out
	}

	t.Run("RefreshesBoundAndCreatesNothing", func(t *testing.T) {
		// The strongest assertion in the suite. The shared key's listing carries
		// three installations and the org bound one of them; the other two must
		// end up ABSENT — not written and then removed by the diff, never
		// written. A row that exists at all is a claim on an installation this
		// workspace never bound, and because the removal diff runs against this
		// org's own set, nothing downstream would ever take it back.
		store, seed := mk(t)
		org := seed.Org(t, seed.User(t))
		body := listing(acct{111, "acme-renamed"}, acct{222, "stranger"}, acct{333, "other-tenant"})
		base, calls := fakeGitHub(t, http.StatusOK, &body)
		seed.Class(t, org, domain.GitHubCredentialClassManagedApp, base)
		host := db.EffectiveGitHubHost(base)
		bind(t, store, org, "111", host, "acme")

		if err := store.RefreshManagedInstallations(ctx, org, deployment); err != nil {
			t.Fatalf("RefreshManagedInstallations: %v", err)
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("listing fetched %d times; want 1", got)
		}

		if got := seed.AllInstallationIDs(t, org); len(got) != 1 || got[0] != "111" {
			t.Fatalf("org holds rows %v; want exactly [111] — a managed reconcile creates nothing, "+
				"and another tenant's installation must be absent rather than present-and-removed", got)
		}
		insts, err := store.ListInstallationsForOrgSystem(ctx, org)
		if err != nil {
			t.Fatalf("ListInstallationsForOrgSystem: %v", err)
		}
		if len(insts) != 1 {
			t.Fatalf("live installations = %d; want 1", len(insts))
		}
		// The refresh half: what a bound row learns from a listing it would
		// otherwise only learn from a webhook nobody re-delivers.
		got := insts[0]
		if got.AccountLogin != "acme-renamed" {
			t.Errorf("AccountLogin = %q; want the renamed %q", got.AccountLogin, "acme-renamed")
		}
		if got.AccountID != "1110" {
			t.Errorf("AccountID = %q; want %q", got.AccountID, "1110")
		}
		if got.RepositorySelection != domain.RepositorySelectionSelected {
			t.Errorf("RepositorySelection = %q; want %q", got.RepositorySelection, domain.RepositorySelectionSelected)
		}
	})

	t.Run("UninstallSoftRemovesABoundInstallation", func(t *testing.T) {
		// Uninstall detection is the other half of what the refresh still does,
		// and the listing is the only place a managed workspace can learn it in
		// local-webhook-free deployments and after a lost delivery alike. The
		// row stays (soft-removed, so history and the reachable-repo cascade both
		// behave), but it stops being live.
		store, seed := mk(t)
		org := seed.Org(t, seed.User(t))
		body := listing(acct{111, "acme"}, acct{999, "stranger"})
		base, _ := fakeGitHub(t, http.StatusOK, &body)
		seed.Class(t, org, domain.GitHubCredentialClassManagedApp, base)
		host := db.EffectiveGitHubHost(base)
		bind(t, store, org, "111", host, "acme")
		bind(t, store, org, "222", host, "gone")

		if err := store.RefreshManagedInstallations(ctx, org, deployment); err != nil {
			t.Fatalf("RefreshManagedInstallations: %v", err)
		}

		if got := liveIDs(t, store, org); len(got) != 1 || got[0] != "111" {
			t.Errorf("live installations = %v; want [111] — 222 is gone from the listing", got)
		}
		all := seed.AllInstallationIDs(t, org)
		if len(all) != 2 || all[0] != "111" || all[1] != "222" {
			t.Errorf("rows = %v; want [111 222] — the uninstall is soft, and the stranger was never written", all)
		}
	})

	t.Run("AFailedListingChangesNothing", func(t *testing.T) {
		// "GitHub no longer reports this installation" and "we could not finish
		// asking" must never be the same answer. A listing that fails is an
		// error, and the mirror is exactly as it was.
		store, seed := mk(t)
		org := seed.Org(t, seed.User(t))
		body := `{"message":"Bad credentials"}`
		base, _ := fakeGitHub(t, http.StatusUnauthorized, &body)
		seed.Class(t, org, domain.GitHubCredentialClassManagedApp, base)
		bind(t, store, org, "111", db.EffectiveGitHubHost(base), "acme")

		if err := store.RefreshManagedInstallations(ctx, org, deployment); err == nil {
			t.Fatal("RefreshManagedInstallations succeeded on a failed listing; want an error")
		}
		if got := liveIDs(t, store, org); len(got) != 1 || got[0] != "111" {
			t.Errorf("live installations = %v after a failed listing; want [111] untouched", got)
		}
	})

	t.Run("NothingBoundAsksGitHubNothing", func(t *testing.T) {
		// A workspace that has not completed a bind has no row to refresh, and
		// creating one is the thing this method may never do — so there is no
		// question for GitHub to answer and no request to spend.
		store, seed := mk(t)
		org := seed.Org(t, seed.User(t))
		body := listing(acct{111, "stranger"})
		base, calls := fakeGitHub(t, http.StatusOK, &body)
		seed.Class(t, org, domain.GitHubCredentialClassManagedApp, base)

		if err := store.RefreshManagedInstallations(ctx, org, deployment); err != nil {
			t.Fatalf("RefreshManagedInstallations: %v", err)
		}
		if got := calls.Load(); got != 0 {
			t.Errorf("listing fetched %d times for a workspace with nothing bound; want 0", got)
		}
		if got := seed.AllInstallationIDs(t, org); len(got) != 0 {
			t.Errorf("org holds rows %v; want none", got)
		}
	})

	t.Run("ANonManagedOrgIsRefused", func(t *testing.T) {
		// Not defensive tidiness. Run against a workspace with its own App key,
		// this would diff that workspace's installations against a listing
		// produced by a key that has never seen them — and soft-remove every one
		// of them. The refusal is loud, and nothing is written.
		store, seed := mk(t)
		org := seed.Org(t, seed.User(t))
		body := listing(acct{999, "stranger"})
		base, calls := fakeGitHub(t, http.StatusOK, &body)
		seed.Class(t, org, domain.GitHubCredentialClassBYOApp, base)
		bind(t, store, org, "111", db.EffectiveGitHubHost(base), "acme")

		if err := store.RefreshManagedInstallations(ctx, org, deployment); err == nil {
			t.Fatal("RefreshManagedInstallations succeeded for a BYO workspace; want a refusal")
		}
		if got := calls.Load(); got != 0 {
			t.Errorf("listing fetched %d times for a BYO workspace; want 0", got)
		}
		if got := liveIDs(t, store, org); len(got) != 1 || got[0] != "111" {
			t.Errorf("live installations = %v; want [111] untouched", got)
		}
	})

	t.Run("AnOrgTheStoreCannotIdentifyIsRefused", func(t *testing.T) {
		// One outcome, two routes. Postgres keys orgs by uuid and guards the id
		// before it reaches a cast that would fail anyway; SQLite keys them by
		// text, where an id naming nothing simply matches no settings row. What
		// must not differ by dialect is the answer: an org this method cannot
		// identify is one it must not reconcile, and it says so rather than
		// quietly doing nothing.
		store, _ := mk(t)
		body := listing(acct{111, "acme"})
		_, calls := fakeGitHub(t, http.StatusOK, &body)

		// A malformed id and a well-formed one naming no org: the first is what
		// separates the dialects, the second is what neither can look up.
		for _, orgID := range []string{"not-a-uuid", "3f2b7c14-0000-4000-8000-000000000000"} {
			if err := store.RefreshManagedInstallations(ctx, orgID, deployment); err == nil {
				t.Errorf("RefreshManagedInstallations(%q) succeeded; want a refusal", orgID)
			}
		}
		if got := calls.Load(); got != 0 {
			t.Errorf("listing fetched %d times for an unidentifiable org; want 0", got)
		}
	})

	t.Run("AnUnconfiguredDeploymentAppIsRefused", func(t *testing.T) {
		// The ordinary state of a deployment whose orgs all bring their own App
		// — and an outage for one that does not. A managed workspace whose shared
		// key has gone missing must fail where someone can see it rather than
		// quietly reconcile nothing.
		store, seed := mk(t)
		org := seed.Org(t, seed.User(t))
		body := listing(acct{111, "acme"})
		base, calls := fakeGitHub(t, http.StatusOK, &body)
		seed.Class(t, org, domain.GitHubCredentialClassManagedApp, base)
		bind(t, store, org, "111", db.EffectiveGitHubHost(base), "acme")

		if err := store.RefreshManagedInstallations(ctx, org, githubapp.DeploymentApp{}); err == nil {
			t.Fatal("RefreshManagedInstallations succeeded with no deployment App; want a refusal")
		}
		if got := calls.Load(); got != 0 {
			t.Errorf("listing fetched %d times with no deployment App; want 0", got)
		}
		if got := liveIDs(t, store, org); len(got) != 1 {
			t.Errorf("live installations = %v; want [111] untouched", got)
		}
	})
}
