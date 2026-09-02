package dbtest

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
)

// managedListingItem is one installation as GET /app/installations renders it,
// in the fields the reconcile reads. A zero SuspendedAt renders as null, which
// is how GitHub reports "not suspended".
type managedListingItem struct {
	ID                  int64
	AccountID           int64
	AccountLogin        string
	AccountType         string
	SuspendedAt         time.Time
	SuspendedBy         string
	RepositorySelection string
}

// managedListing renders items as the listing body.
func managedListing(t *testing.T, items ...managedListingItem) string {
	t.Helper()
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		accountType := it.AccountType
		if accountType == "" {
			accountType = "Organization"
		}
		selection := it.RepositorySelection
		if selection == "" {
			selection = domain.RepositorySelectionSelected
		}
		m := map[string]any{
			"id":                   it.ID,
			"account":              map[string]any{"id": it.AccountID, "login": it.AccountLogin, "type": accountType},
			"repository_selection": selection,
			"suspended_at":         nil,
			"suspended_by":         nil,
		}
		if !it.SuspendedAt.IsZero() {
			m["suspended_at"] = it.SuspendedAt.UTC().Format(time.RFC3339)
			m["suspended_by"] = map[string]any{"login": it.SuspendedBy}
		}
		out = append(out, m)
	}
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("render listing: %v", err)
	}
	return string(body)
}

// managedFakeGitHub stands up the App API the cadence pass reads, counting
// listing walks so the suite can assert the one property the deployment-scoped
// shape exists for: N bound workspaces cost one walk. The GHES path mount is
// what ghbase.APIBase derives for any base URL that is not github.com, which
// every httptest server is.
func managedFakeGitHub(t *testing.T, status int, body *string) (baseURL string, calls *atomic.Int64) {
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
		_, _ = w.Write([]byte(*body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL, calls
}

// RunGitHubManagedCadenceConformance is the shared assertion suite for
// RefreshAllManagedInstallations — the cadence pass that converges every
// managed workspace's installation set from one listing of the deployment
// App's installations.
//
// The per-org suite (RunGitHubManagedRefreshConformance) pins what a managed
// refresh may and may not do. This one pins what the CADENCE guarantees on top:
// that every field the listing is the sole writer of actually converges without
// a webhook or a button press, that the whole deployment costs one listing walk
// whatever its tenant count, that an uninstall takes its reach with it, and
// that the classes this pass is not for are never in its read at all.
//
// It takes the same factory as the per-org suite: the seeder's reach hooks
// are what the uninstall case stages and counts the cascade through.
func RunGitHubManagedCadenceConformance(t *testing.T, mk GitHubManagedRefreshFactory) {
	t.Helper()
	ctx := context.Background()

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

	// bind writes the row the bind ceremony would have written, in whatever
	// state the case wants the row found in.
	bind := func(t *testing.T, store db.GitHubAppsStore, inst domain.OrgGitHubAppInstallation) {
		t.Helper()
		if inst.AccountType == "" {
			inst.AccountType = "Organization"
		}
		if _, err := store.UpsertInstallation(ctx, inst); err != nil {
			t.Fatalf("UpsertInstallation (bind %s): %v", inst.InstallationID, err)
		}
	}

	live := func(t *testing.T, store db.GitHubAppsStore, orgID string) map[string]domain.OrgGitHubAppInstallation {
		t.Helper()
		insts, err := store.ListInstallationsForOrgSystem(ctx, orgID)
		if err != nil {
			t.Fatalf("ListInstallationsForOrgSystem: %v", err)
		}
		out := make(map[string]domain.OrgGitHubAppInstallation, len(insts))
		for _, inst := range insts {
			out[inst.InstallationID] = inst
		}
		return out
	}

	// A managed workspace with one bound installation, listed on the given fake
	// GitHub: the fixture every convergence case starts from.
	managedOrg := func(t *testing.T, store db.GitHubAppsStore, seed GitHubManagedRefreshSeeder, base string, bound domain.OrgGitHubAppInstallation) string {
		t.Helper()
		org := seed.Org(t, seed.User(t))
		seed.Class(t, org, domain.GitHubCredentialClassManagedApp, base)
		bound.OrgID = org
		bound.GitHubHost = db.EffectiveGitHubHost(base)
		bind(t, store, bound)
		return org
	}

	t.Run("EveryListingFedFieldConverges", func(t *testing.T) {
		// The guard against the class of miss that produced this pass. Each row
		// is one field the listing is the sole pull-side writer of, found stale
		// on the bound row and reported fresh by GitHub, and the assertion is
		// that the cadence — no webhook, no button — brings the row to what
		// GitHub reports. A field the listing writes that is missing from this
		// table fails the reflection check below, so the next listing-fed field
		// cannot quietly join the push-only set.
		suspendedAt := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
		cases := []struct {
			field  string // the domain field this row pins, for the coverage check
			name   string
			bound  domain.OrgGitHubAppInstallation // the row as found (stale)
			listed managedListingItem              // what GitHub now reports
			check  func(t *testing.T, got domain.OrgGitHubAppInstallation)
		}{
			{
				field: "AccountLogin", name: "an account rename",
				bound:  domain.OrgGitHubAppInstallation{InstallationID: "111", AccountLogin: "acme", AccountID: "1110"},
				listed: managedListingItem{ID: 111, AccountID: 1110, AccountLogin: "acme-renamed"},
				check: func(t *testing.T, got domain.OrgGitHubAppInstallation) {
					if got.AccountLogin != "acme-renamed" {
						t.Errorf("AccountLogin = %q; want %q", got.AccountLogin, "acme-renamed")
					}
				},
			},
			{
				field: "AccountID", name: "an account id learned late",
				bound:  domain.OrgGitHubAppInstallation{InstallationID: "111", AccountLogin: "acme"},
				listed: managedListingItem{ID: 111, AccountID: 1110, AccountLogin: "acme"},
				check: func(t *testing.T, got domain.OrgGitHubAppInstallation) {
					if got.AccountID != "1110" {
						t.Errorf("AccountID = %q; want %q", got.AccountID, "1110")
					}
				},
			},
			{
				field: "AccountType", name: "an account type corrected",
				bound:  domain.OrgGitHubAppInstallation{InstallationID: "111", AccountLogin: "acme", AccountType: "User"},
				listed: managedListingItem{ID: 111, AccountID: 1110, AccountLogin: "acme", AccountType: "Organization"},
				check: func(t *testing.T, got domain.OrgGitHubAppInstallation) {
					if got.AccountType != "Organization" {
						t.Errorf("AccountType = %q; want %q", got.AccountType, "Organization")
					}
				},
			},
			{
				field: "SuspendedAt", name: "a lost unsuspend delivery",
				bound:  domain.OrgGitHubAppInstallation{InstallationID: "111", AccountLogin: "acme", SuspendedAt: suspendedAt, SuspendedBy: "owner"},
				listed: managedListingItem{ID: 111, AccountID: 1110, AccountLogin: "acme"},
				check: func(t *testing.T, got domain.OrgGitHubAppInstallation) {
					if got.Suspended() {
						t.Errorf("still suspended (at %v, by %q); want cleared — GitHub reports no suspension", got.SuspendedAt, got.SuspendedBy)
					}
				},
			},
			{
				field: "SuspendedAt", name: "a lost suspend delivery",
				bound:  domain.OrgGitHubAppInstallation{InstallationID: "111", AccountLogin: "acme"},
				listed: managedListingItem{ID: 111, AccountID: 1110, AccountLogin: "acme", SuspendedAt: suspendedAt, SuspendedBy: "owner"},
				check: func(t *testing.T, got domain.OrgGitHubAppInstallation) {
					if !got.SuspendedAt.Equal(suspendedAt) {
						t.Errorf("SuspendedAt = %v; want %v", got.SuspendedAt, suspendedAt)
					}
				},
			},
			{
				field: "SuspendedBy", name: "the suspending login",
				bound:  domain.OrgGitHubAppInstallation{InstallationID: "111", AccountLogin: "acme", SuspendedAt: suspendedAt, SuspendedBy: ""},
				listed: managedListingItem{ID: 111, AccountID: 1110, AccountLogin: "acme", SuspendedAt: suspendedAt, SuspendedBy: "owner"},
				check: func(t *testing.T, got domain.OrgGitHubAppInstallation) {
					if got.SuspendedBy != "owner" {
						t.Errorf("SuspendedBy = %q; want %q", got.SuspendedBy, "owner")
					}
				},
			},
			{
				field: "RepositorySelection", name: "a grant narrowed all to selected",
				bound:  domain.OrgGitHubAppInstallation{InstallationID: "111", AccountLogin: "acme", RepositorySelection: domain.RepositorySelectionAll},
				listed: managedListingItem{ID: 111, AccountID: 1110, AccountLogin: "acme", RepositorySelection: domain.RepositorySelectionSelected},
				check: func(t *testing.T, got domain.OrgGitHubAppInstallation) {
					if got.RepositorySelection != domain.RepositorySelectionSelected {
						t.Errorf("RepositorySelection = %q; want %q", got.RepositorySelection, domain.RepositorySelectionSelected)
					}
					// The drift rule downstream reads exactly this: a narrowed
					// grant must stop reading as one that cannot drift.
					if got.GrantsEveryRepository() {
						t.Error("GrantsEveryRepository() = true after narrowing; drift would be suppressed as impossible")
					}
				},
			},
		}

		// Every field of the row is either pinned above or named here as one
		// the listing does not converge — the identity the bind wrote
		// (installation, org, host) and the install time the upsert preserves.
		// Anything else is a field somebody added without deciding which set it
		// is in.
		notListingFed := map[string]bool{"InstallationID": true, "OrgID": true, "GitHubHost": true, "InstalledAt": true}
		covered := map[string]bool{}
		for _, tc := range cases {
			covered[tc.field] = true
		}
		rowType := reflect.TypeOf(domain.OrgGitHubAppInstallation{})
		for i := 0; i < rowType.NumField(); i++ {
			name := rowType.Field(i).Name
			if !covered[name] && !notListingFed[name] {
				t.Errorf("OrgGitHubAppInstallation.%s is neither in the convergence table nor declared not listing-fed; decide which", name)
			}
			if covered[name] && notListingFed[name] {
				t.Errorf("OrgGitHubAppInstallation.%s is in both sets", name)
			}
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				store, seed := mk(t)
				body := managedListing(t, tc.listed, managedListingItem{ID: 999, AccountID: 9990, AccountLogin: "stranger"})
				base, calls := managedFakeGitHub(t, http.StatusOK, &body)
				org := managedOrg(t, store, seed, base, tc.bound)

				if err := store.RefreshAllManagedInstallations(ctx, deployment); err != nil {
					t.Fatalf("RefreshAllManagedInstallations: %v", err)
				}
				if got := calls.Load(); got != 1 {
					t.Errorf("listing fetched %d times; want 1", got)
				}
				got, ok := live(t, store, org)["111"]
				if !ok {
					t.Fatal("bound installation 111 is no longer live after the refresh")
				}
				tc.check(t, got)
				if rows := seed.AllInstallationIDs(t, org); len(rows) != 1 {
					t.Errorf("org holds rows %v; want exactly [111] — the stranger must never be written", rows)
				}
			})
		}
	})

	t.Run("OneListingForNWorkspaces", func(t *testing.T) {
		// The property the deployment-scoped shape exists for. Three managed
		// workspaces, each with its own bound installation, converge from ONE
		// walk of GET /app/installations — the shared key's answer is the same
		// whoever asks, so asking once per org would spend one rate budget
		// three times for three identical responses.
		store, seed := mk(t)
		body := managedListing(t,
			managedListingItem{ID: 111, AccountID: 1110, AccountLogin: "one-renamed"},
			managedListingItem{ID: 222, AccountID: 2220, AccountLogin: "two-renamed"},
			managedListingItem{ID: 333, AccountID: 3330, AccountLogin: "three-renamed"},
		)
		base, calls := managedFakeGitHub(t, http.StatusOK, &body)
		orgs := map[string]string{
			"111": managedOrg(t, store, seed, base, domain.OrgGitHubAppInstallation{InstallationID: "111", AccountLogin: "one"}),
			"222": managedOrg(t, store, seed, base, domain.OrgGitHubAppInstallation{InstallationID: "222", AccountLogin: "two"}),
			"333": managedOrg(t, store, seed, base, domain.OrgGitHubAppInstallation{InstallationID: "333", AccountLogin: "three"}),
		}

		if err := store.RefreshAllManagedInstallations(ctx, deployment); err != nil {
			t.Fatalf("RefreshAllManagedInstallations: %v", err)
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("listing walked %d times for 3 managed workspaces; want exactly 1", got)
		}
		for id, org := range orgs {
			rows := live(t, store, org)
			if len(rows) != 1 {
				t.Errorf("org bound to %s holds live rows %v; want exactly its own", id, rows)
				continue
			}
			got := rows[id]
			if want := map[string]string{"111": "one-renamed", "222": "two-renamed", "333": "three-renamed"}[id]; got.AccountLogin != want {
				t.Errorf("installation %s: AccountLogin = %q; want %q", id, got.AccountLogin, want)
			}
		}
	})

	t.Run("UninstallSoftRemovesWithItsReach", func(t *testing.T) {
		// A bound installation the listing no longer reports is an uninstall.
		// The row is soft-removed, and everything that installation reached goes
		// with it — the reachable-repo entries and the scope marker — so the
		// picker never offers reach TF no longer has.
		store, seed := mk(t)
		body := managedListing(t, managedListingItem{ID: 111, AccountID: 1110, AccountLogin: "acme"})
		base, _ := managedFakeGitHub(t, http.StatusOK, &body)
		org := managedOrg(t, store, seed, base, domain.OrgGitHubAppInstallation{InstallationID: "111", AccountLogin: "acme"})
		bind(t, store, domain.OrgGitHubAppInstallation{InstallationID: "222", OrgID: org, AccountLogin: "gone", GitHubHost: db.EffectiveGitHubHost(base)})
		seed.Reach(t, org, "111")
		seed.Reach(t, org, "222")
		if entries, scopes := seed.ReachRows(t, org, "222"); entries != 1 || scopes != 1 {
			t.Fatalf("reach staged for 222 = %d entries, %d scopes; want 1 and 1", entries, scopes)
		}

		if err := store.RefreshAllManagedInstallations(ctx, deployment); err != nil {
			t.Fatalf("RefreshAllManagedInstallations: %v", err)
		}

		rows := live(t, store, org)
		if _, ok := rows["222"]; ok || len(rows) != 1 {
			t.Errorf("live installations = %v; want only 111 — 222 is gone from the listing", rows)
		}
		if all := seed.AllInstallationIDs(t, org); len(all) != 2 {
			t.Errorf("rows = %v; want [111 222] — the uninstall is soft", all)
		}
		if entries, scopes := seed.ReachRows(t, org, "222"); entries != 0 || scopes != 0 {
			t.Errorf("reach left for 222 after the uninstall = %d entries, %d scopes; want none — the cascade must take both", entries, scopes)
		}
		if entries, scopes := seed.ReachRows(t, org, "111"); entries != 1 || scopes != 1 {
			t.Errorf("reach for the still-installed 111 = %d entries, %d scopes; want 1 and 1 untouched", entries, scopes)
		}
	})

	t.Run("NeverDiscovers", func(t *testing.T) {
		// Two managed workspaces, one bound and one not, and a listing carrying
		// an installation neither bound. Nothing is created anywhere: the bound
		// workspace keeps exactly its own row, the unbound one stays empty. Under
		// a shared key the listing is every tenant's, and attributing any of it
		// to a workspace whose admin never proved the link is the one thing this
		// pass may never do.
		store, seed := mk(t)
		body := managedListing(t,
			managedListingItem{ID: 111, AccountID: 1110, AccountLogin: "acme"},
			managedListingItem{ID: 999, AccountID: 9990, AccountLogin: "stranger"},
		)
		base, _ := managedFakeGitHub(t, http.StatusOK, &body)
		bound := managedOrg(t, store, seed, base, domain.OrgGitHubAppInstallation{InstallationID: "111", AccountLogin: "acme"})
		unbound := seed.Org(t, seed.User(t))
		seed.Class(t, unbound, domain.GitHubCredentialClassManagedApp, base)

		if err := store.RefreshAllManagedInstallations(ctx, deployment); err != nil {
			t.Fatalf("RefreshAllManagedInstallations: %v", err)
		}
		if rows := seed.AllInstallationIDs(t, bound); len(rows) != 1 || rows[0] != "111" {
			t.Errorf("bound workspace holds rows %v; want exactly [111]", rows)
		}
		if rows := seed.AllInstallationIDs(t, unbound); len(rows) != 0 {
			t.Errorf("unbound workspace holds rows %v; want none — a refresh creates nothing", rows)
		}
	})

	t.Run("AFailedListingChangesNothing", func(t *testing.T) {
		// "GitHub no longer reports this installation" and "we could not finish
		// asking" must never be the same answer. The pass errors, and every
		// bound row is exactly as it was — still live, still stale.
		store, seed := mk(t)
		body := `{"message":"Bad credentials"}`
		base, _ := managedFakeGitHub(t, http.StatusUnauthorized, &body)
		org := managedOrg(t, store, seed, base, domain.OrgGitHubAppInstallation{InstallationID: "111", AccountLogin: "acme"})

		if err := store.RefreshAllManagedInstallations(ctx, deployment); err == nil {
			t.Fatal("RefreshAllManagedInstallations succeeded on a failed listing; want an error")
		}
		got, ok := live(t, store, org)["111"]
		if !ok || got.AccountLogin != "acme" {
			t.Errorf("row after a failed listing = %+v (live=%v); want [111 acme] untouched", got, ok)
		}
	})

	t.Run("NoBoundManagedWorkspaceAsksNothing", func(t *testing.T) {
		// A deployment whose managed workspaces have bound nothing — and whose
		// other workspaces bring their own key — has no row this pass could
		// write, so there is no request to spend and no deployment App to need.
		// The zero App here pins the ordering: the App is consulted only once
		// there is something to list for.
		store, seed := mk(t)
		body := managedListing(t, managedListingItem{ID: 111, AccountID: 1110, AccountLogin: "acme"})
		base, calls := managedFakeGitHub(t, http.StatusOK, &body)
		unbound := seed.Org(t, seed.User(t))
		seed.Class(t, unbound, domain.GitHubCredentialClassManagedApp, base)
		byo := seed.Org(t, seed.User(t))
		seed.Class(t, byo, domain.GitHubCredentialClassBYOApp, base)
		bind(t, store, domain.OrgGitHubAppInstallation{InstallationID: "555", OrgID: byo, AccountLogin: "own-key", GitHubHost: db.EffectiveGitHubHost(base)})

		if err := store.RefreshAllManagedInstallations(ctx, githubapp.DeploymentApp{}); err != nil {
			t.Fatalf("RefreshAllManagedInstallations with nothing bound: %v; want a no-op", err)
		}
		if got := calls.Load(); got != 0 {
			t.Errorf("listing fetched %d times with no bound managed workspace; want 0", got)
		}
		if _, ok := live(t, store, byo)["555"]; !ok {
			t.Error("the BYO workspace's installation is no longer live; the managed pass must not read it at all")
		}
	})

	t.Run("AByoWorkspaceIsUntouched", func(t *testing.T) {
		// The class gate is the read itself: a workspace with its own App key is
		// not in the managed set, so its bound installation — absent from the
		// deployment App's listing, as it must be, since a different key lists
		// it — is neither refreshed nor removed while the managed workspace
		// beside it converges.
		store, seed := mk(t)
		body := managedListing(t, managedListingItem{ID: 111, AccountID: 1110, AccountLogin: "acme-renamed"})
		base, calls := managedFakeGitHub(t, http.StatusOK, &body)
		managed := managedOrg(t, store, seed, base, domain.OrgGitHubAppInstallation{InstallationID: "111", AccountLogin: "acme"})
		byo := seed.Org(t, seed.User(t))
		seed.Class(t, byo, domain.GitHubCredentialClassBYOApp, base)
		bind(t, store, domain.OrgGitHubAppInstallation{InstallationID: "555", OrgID: byo, AccountLogin: "own-key", GitHubHost: db.EffectiveGitHubHost(base)})

		if err := store.RefreshAllManagedInstallations(ctx, deployment); err != nil {
			t.Fatalf("RefreshAllManagedInstallations: %v", err)
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("listing fetched %d times; want 1", got)
		}
		if got := live(t, store, managed)["111"]; got.AccountLogin != "acme-renamed" {
			t.Errorf("managed row AccountLogin = %q; want %q", got.AccountLogin, "acme-renamed")
		}
		if got, ok := live(t, store, byo)["555"]; !ok || got.AccountLogin != "own-key" {
			t.Errorf("BYO row = %+v (live=%v); want [555 own-key] untouched", got, ok)
		}
	})

	t.Run("AnUnconfiguredDeploymentAppIsRefused", func(t *testing.T) {
		// A managed workspace with a bound installation and no shared key to
		// list it with is an outage, not a quiet skip: the pass errors where an
		// operator can see it, and the row is untouched.
		store, seed := mk(t)
		body := managedListing(t, managedListingItem{ID: 111, AccountID: 1110, AccountLogin: "acme"})
		base, calls := managedFakeGitHub(t, http.StatusOK, &body)
		org := managedOrg(t, store, seed, base, domain.OrgGitHubAppInstallation{InstallationID: "111", AccountLogin: "acme"})

		if err := store.RefreshAllManagedInstallations(ctx, githubapp.DeploymentApp{}); err == nil {
			t.Fatal("RefreshAllManagedInstallations succeeded with no deployment App; want a refusal")
		}
		if got := calls.Load(); got != 0 {
			t.Errorf("listing fetched %d times with no deployment App; want 0", got)
		}
		if _, ok := live(t, store, org)["111"]; !ok {
			t.Error("bound installation is no longer live after a refused pass; want untouched")
		}
	})
}
