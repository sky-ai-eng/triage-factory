package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/github/grantmirror"
	"github.com/sky-ai-eng/triage-factory/internal/github/reachcache"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The picker and the team-repos write gate both read the reachable-repo mirror.
// These tests pin its contract: one shape and one freshness rule on both
// credential classes, no GitHub call while the mirror is fresh, a first-ever
// open that says "not looked yet" rather than "no repositories", and a write
// gate that decides the same way whichever process served the picker.

// reachHarness wires the production refresh path into a test server and runs it
// SYNCHRONOUSLY on trigger, so a test can assert on what one picker open
// produced without racing a background goroutine. Everything else — the class
// resolution, the TTL gate, the App/PAT split — is the real code.
type reachHarness struct {
	refresher *reachcache.Refresher
	triggers  atomic.Int32
	forced    atomic.Int32
}

func wireReachRefresh(t *testing.T, s *Server) *reachHarness {
	t.Helper()
	h := &reachHarness{}
	// One class resolver for the writer and the reads, exactly as the app wires
	// it: the two keying an org differently is a mirror that reads as empty for
	// an org that has one.
	classes := reachcache.NewClassResolver(s.orgs, s.githubApps)
	grant := grantmirror.NewReconciler(s.githubApps, s.reachableRepos, s.ghResolver, classes, s.deploymentApp)
	h.refresher = reachcache.NewRefresher(classes, s.reachableRepos, s.ghResolver, grant.RunOrg, nil)
	s.SetReachTrigger(func(orgID string, force bool) {
		h.triggers.Add(1)
		if force {
			h.forced.Add(1)
		}
		if _, err := h.refresher.RunOrg(context.Background(), orgID, force); err != nil {
			t.Logf("reach refresh(org=%s, force=%v): %v", orgID, force, err)
		}
	})
	return h
}

// refresh drives one refresh pass directly, standing in for the out-of-band one
// a real trigger would run.
func (h *reachHarness) refresh(t *testing.T, orgID string, force bool) bool {
	t.Helper()
	wrote, err := h.refresher.RunOrg(context.Background(), orgID, force)
	if err != nil {
		t.Fatalf("reach refresh(org=%s, force=%v): %v", orgID, force, err)
	}
	return wrote
}

// countingGitHub is a fake GitHub that answers the two enumerations and counts
// every request, so a test can assert an upstream call did NOT happen — which is
// the only way to state "the picker read reaches GitHub zero times".
type countingGitHub struct {
	*httptest.Server
	userRepos      atomic.Int32
	installRepos   atomic.Int32
	tokenMints     atomic.Int32
	failEnumerator atomic.Bool
}

// newCountingPATGitHub serves GET /user/repos with the given slugs, one page,
// each carrying its 1-based position as its GitHub id.
func newCountingPATGitHub(t *testing.T, slugs ...string) *countingGitHub {
	t.Helper()
	c := &countingGitHub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/user/repos", func(w http.ResponseWriter, r *http.Request) {
		c.userRepos.Add(1)
		if c.failEnumerator.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		page := []map[string]any{}
		if r.URL.Query().Get("page") == "1" {
			for i, slug := range slugs {
				page = append(page, map[string]any{"full_name": slug, "language": "Go", "id": i + 1})
			}
		}
		_ = json.NewEncoder(w).Encode(page)
	})
	// The write gate's cold path probes per repo; answering 404 for everything
	// keeps a test that expects the HOT path honest — if the gate silently fell
	// through, a reachable slug would be rejected and the test would fail.
	mux.HandleFunc("/api/v3/repos/", func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	})
	c.Server = httptest.NewServer(mux)
	t.Cleanup(c.Close)
	return c
}

// newCountingAppGitHub serves the App tier: token mints plus
// GET /installation/repositories scoped to the presented token.
func newCountingAppGitHub(t *testing.T, reposByInstall map[string][]string) *countingGitHub {
	t.Helper()
	c := &countingGitHub{}
	mux := http.NewServeMux()
	// Existence first: the grant refresh reconciles the installation set before
	// it reads any grant, so a stub that answered nothing here would soft-remove
	// the installation the fixture just seeded. Rendered up front so a bad
	// fixture fails the test on the test's own goroutine rather than the
	// server's, where t.Fatalf cannot fail anything.
	installations := stubInstallations(t, reposByInstall)
	mux.HandleFunc("GET /api/v3/app/installations", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(installations)
	})
	mux.HandleFunc("/api/v3/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		c.tokenMints.Add(1)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_111",
			"expires_at": time.Now().Add(time.Hour).UTC(),
		})
	})
	mux.HandleFunc("/api/v3/installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		c.installRepos.Add(1)
		names := reposByInstall["111"]
		repos := make([]map[string]any, 0, len(names))
		if r.URL.Query().Get("page") == "" || r.URL.Query().Get("page") == "1" {
			for _, n := range names {
				repos = append(repos, map[string]any{"full_name": n, "language": "Go"})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total_count":  len(names),
			"repositories": repos,
		})
	})
	c.Server = httptest.NewServer(mux)
	t.Cleanup(c.Close)
	return c
}

// seedPATOrg points the local org at ts and stores a PAT, which is the whole of
// what a PAT-class org is.
func seedPATOrg(t *testing.T, s *Server, url string) {
	t.Helper()
	seedSourceHost(t, s, eventsource.KindGitHub, url)
	if err := integrations.Save(context.Background(), s.secrets, runmode.LocalDefaultOrgID, auth.Credentials{
		GitHubPAT: "ghp-test",
	}); err != nil {
		t.Fatalf("seed creds: %v", err)
	}
}

// ageMirror pushes the mirror back by d, which is how a test reaches a staleness
// the TTL is measured in hours against. Both halves move together: the scope
// rows are what the staleness gate reads, and the entries carry the same instant
// so a test never observes the two disagreeing.
func ageMirror(t *testing.T, database *sql.DB, d time.Duration) {
	t.Helper()
	at := time.Now().Add(-d).UTC()
	if _, err := database.Exec(`UPDATE reachable_scopes SET refreshed_at = ?`, at); err != nil {
		t.Fatalf("age reachable scopes: %v", err)
	}
	if _, err := database.Exec(`UPDATE reachable_repositories SET observed_at = ?`, at); err != nil {
		t.Fatalf("age reachable repositories: %v", err)
	}
}

// responseKeys is the sorted key set of a JSON object body — what "the same
// schema" is actually asserted on.
func responseKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, raw)
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// TestPickerSchemaIsIdenticalAcrossCredentialClasses: served from the mirror,
// the PAT and App pickers answer the same keys, a real total_count and each
// option's GitHub id, without a GitHub call. The key sets are compared directly.
func TestPickerSchemaIsIdenticalAcrossCredentialClasses(t *testing.T) {
	keyring.MockInit()

	patSrv := newTestServer(t)
	patGH := newCountingPATGitHub(t, "acme/api", "acme/web")
	seedPATOrg(t, patSrv, patGH.URL)
	wireReachRefresh(t, patSrv).refresh(t, runmode.LocalDefaultOrgID, true)

	appSrv := newTestServer(t)
	appGH := newCountingAppGitHub(t, map[string][]string{"111": {"acme/api", "acme/web"}})
	seedApp(t, appSrv, appGH.Server, []domain.OrgGitHubAppInstallation{
		{InstallationID: "111", AccountType: "Organization", AccountLogin: "acme"},
	})
	wireReachRefresh(t, appSrv).refresh(t, runmode.LocalDefaultOrgID, true)

	// Both reads are served from the mirror, so neither may touch GitHub. Counted
	// on both tiers rather than one: "zero upstream calls" is a property of the
	// contract, and a tier that quietly kept proxying would still answer the right
	// shape.
	patSpent, appSpent := patGH.userRepos.Load(), appGH.installRepos.Load()
	patRec := doJSON(t, patSrv, http.MethodPost, "/api/github/repos/list", map[string]any{})
	appRec := doJSON(t, appSrv, http.MethodPost, "/api/github/repos/list", map[string]any{})
	if got := patGH.userRepos.Load(); got != patSpent {
		t.Errorf("pat picker read made %d upstream calls; want zero of its own", got-patSpent)
	}
	if got := appGH.installRepos.Load(); got != appSpent {
		t.Errorf("app picker read made %d upstream calls; want zero of its own", got-appSpent)
	}
	if patRec.Code != http.StatusOK || appRec.Code != http.StatusOK {
		t.Fatalf("pat=%d app=%d, want 200 on both; pat body=%s app body=%s",
			patRec.Code, appRec.Code, patRec.Body.String(), appRec.Body.String())
	}

	patKeys, appKeys := responseKeys(t, patRec.Body.Bytes()), responseKeys(t, appRec.Body.Bytes())
	if len(patKeys) != len(appKeys) {
		t.Fatalf("pat keys %v vs app keys %v; want identical", patKeys, appKeys)
	}
	for i := range patKeys {
		if patKeys[i] != appKeys[i] {
			t.Fatalf("pat keys %v vs app keys %v; want identical", patKeys, appKeys)
		}
	}

	var patPage, appPage pickerPage
	if err := json.Unmarshal(patRec.Body.Bytes(), &patPage); err != nil {
		t.Fatalf("decode pat page: %v", err)
	}
	if err := json.Unmarshal(appRec.Body.Bytes(), &appPage); err != nil {
		t.Fatalf("decode app page: %v", err)
	}
	if patPage.TotalCount != 2 || appPage.TotalCount != 2 {
		t.Errorf("total_count pat=%d app=%d; want 2 on both — a real count is the point of sourcing this locally",
			patPage.TotalCount, appPage.TotalCount)
	}
	if patPage.Status != githubRepoStatusReady || appPage.Status != githubRepoStatusReady {
		t.Errorf("status pat=%q app=%q; want %q on both", patPage.Status, appPage.Status, githubRepoStatusReady)
	}
	// Each option carries the GitHub id a client registers it by.
	if len(patPage.Items) != 2 || patPage.Items[0].GitHubID != "1" || patPage.Items[1].GitHubID != "2" {
		t.Errorf("pat items = %+v; want acme/api and acme/web carrying GitHub ids 1 and 2", patPage.Items)
	}
}

// TestPickerReadinessSchemaMatchesTheReadyOne: a first-ever open answers
// status discovering with items [] (not null), under the same keys as the ready
// answer, so a client renders off `status` rather than an empty array.
func TestPickerReadinessSchemaMatchesTheReadyOne(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	gh := newCountingPATGitHub(t, "acme/api")
	seedPATOrg(t, srv, gh.URL)

	discovering := doJSON(t, srv, http.MethodPost, "/api/github/repos/list", map[string]any{})
	if discovering.Code != http.StatusOK {
		t.Fatalf("empty-mirror list = %d, want 200; body=%s", discovering.Code, discovering.Body.String())
	}
	var page pickerPage
	if err := json.Unmarshal(discovering.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.Status != githubRepoStatusDiscovering {
		t.Fatalf("status = %q on a first-ever open; want %q", page.Status, githubRepoStatusDiscovering)
	}
	if page.Items == nil {
		t.Error("items = null on the readiness response; want [] — a client iterates the field without a nil check")
	}

	wireReachRefresh(t, srv).refresh(t, runmode.LocalDefaultOrgID, true)
	ready := doJSON(t, srv, http.MethodPost, "/api/github/repos/list", map[string]any{})
	if ready.Code != http.StatusOK {
		t.Fatalf("ready list = %d, want 200; body=%s", ready.Code, ready.Body.String())
	}

	before, after := responseKeys(t, discovering.Body.Bytes()), responseKeys(t, ready.Body.Bytes())
	if len(before) != len(after) {
		t.Fatalf("discovering keys %v vs ready keys %v; want identical", before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("discovering keys %v vs ready keys %v; want identical", before, after)
		}
	}
}

// TestReachRefreshFreshnessContractIsOneContract: both credential classes
// refresh an empty mirror, skip a fresh one without a GitHub call, refresh one
// aged past the TTL, and refresh past the TTL when forced.
func TestReachRefreshFreshnessContractIsOneContract(t *testing.T) {
	keyring.MockInit()

	type tier struct {
		name         string
		srv          *Server
		reach        *reachHarness
		enumerations func() int32
	}

	patSrv := newTestServer(t)
	patGH := newCountingPATGitHub(t, "acme/api")
	seedPATOrg(t, patSrv, patGH.URL)

	appSrv := newTestServer(t)
	appGH := newCountingAppGitHub(t, map[string][]string{"111": {"acme/api"}})
	seedApp(t, appSrv, appGH.Server, []domain.OrgGitHubAppInstallation{
		{InstallationID: "111", AccountType: "Organization", AccountLogin: "acme"},
	})

	tiers := []tier{
		{name: "pat", srv: patSrv, reach: wireReachRefresh(t, patSrv), enumerations: patGH.userRepos.Load},
		{name: "byo_app", srv: appSrv, reach: wireReachRefresh(t, appSrv), enumerations: appGH.installRepos.Load},
	}

	for _, tr := range tiers {
		t.Run(tr.name, func(t *testing.T) {
			org := runmode.LocalDefaultOrgID

			// Stale (nothing mirrored) → the pass writes.
			if !tr.reach.refresh(t, org, false) {
				t.Fatal("an empty mirror must refresh: there is nothing to serve")
			}
			afterFirst := tr.enumerations()

			// Fresh → the pass is a no-op and costs no upstream call.
			if tr.reach.refresh(t, org, false) {
				t.Error("a fresh mirror refreshed again; the TTL gate must skip it")
			}
			if got := tr.enumerations(); got != afterFirst {
				t.Errorf("%d enumerations after a TTL-skipped pass; want the %d already spent", got, afterFirst)
			}

			// Force → bypasses the TTL and spends the call.
			if !tr.reach.refresh(t, org, true) {
				t.Error("force must refresh a mirror the TTL considers fresh")
			}
			if got := tr.enumerations(); got <= afterFirst {
				t.Errorf("%d enumerations after a forced pass; want more than the %d before it", got, afterFirst)
			}

			// Aged past the TTL → stale again, no force needed.
			ageMirror(t, tr.srv.db, reachcache.TTL+time.Hour)
			forced := tr.enumerations()
			if !tr.reach.refresh(t, org, false) {
				t.Error("a mirror older than the TTL must refresh without a force")
			}
			if got := tr.enumerations(); got <= forced {
				t.Errorf("%d enumerations after an aged mirror refreshed; want more than the %d before it", got, forced)
			}
		})
	}
}

// TestTeamReposPut_GateReadsTheMirrorFromAnotherPod: the picker is served by
// one server and the PUT by another over the same database, and the write gate
// decides from the mirror the picker filled. The stub answers 404 for every
// per-repo probe, so a gate that fell to its cold path would refuse the PUT.
func TestTeamReposPut_GateReadsTheMirrorFromAnotherPod(t *testing.T) {
	keyring.MockInit()
	picker := newTestServer(t)
	gh := newCountingPATGitHub(t, "owner/tracked")
	seedPATOrg(t, picker, gh.URL)

	// Pod A serves the picker. The first open finds nothing mirrored, kicks the
	// refresh, and answers the readiness state; the next reads the fresh mirror
	// without reaching GitHub, and no ordinary open forces a refresh.
	reach := wireReachRefresh(t, picker)
	if page := fetchPickerPage(t, picker, map[string]any{}); page.Status != githubRepoStatusDiscovering {
		t.Fatalf("first open on pod A = %q; want %q", page.Status, githubRepoStatusDiscovering)
	}
	spent := gh.userRepos.Load()
	if spent == 0 {
		t.Fatal("the first open never enumerated; the readiness state must be accompanied by an actual refresh")
	}
	if page := fetchPickerPage(t, picker, map[string]any{}); page.TotalCount != 1 || page.Status != githubRepoStatusReady {
		t.Fatalf("pod A second open = %q, total_count %d; want %q and 1", page.Status, page.TotalCount, githubRepoStatusReady)
	}
	if got := gh.userRepos.Load(); got != spent {
		t.Errorf("%d upstream enumerations after a fresh read; want the %d the first open cost", got, spent)
	}
	if reach.forced.Load() != 0 {
		t.Errorf("%d forced refreshes from ordinary opens; want 0", reach.forced.Load())
	}

	// Pod B: a second process over the same database, which has never served a
	// picker read and therefore never warmed anything of its own.
	writer := New(picker.db, sqlitestore.New(picker.db))

	// Registration reads the mirror too: owner/tracked is GitHub id 1 there.
	tracked := registeredID(t, writer, "1")
	if rec := putTracked(t, writer, tracked.ID); rec.Code != http.StatusOK {
		t.Fatalf("reachable PUT on pod B = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	ghost := registerBare(t, writer, hostOf(t, writer), "owner/ghost")
	msgs := assertRefusedFields(t, putTracked(t, writer, tracked.ID, ghost), http.StatusUnprocessableEntity, "repository_ids[1]")
	assertNamesByID(t, msgs[0], ghost, "owner/ghost")
}

// TestTeamReposPut_StaleMirrorFallsToTheColdPath: past reachableGateMaxAge the
// mirror stops deciding writes and the per-repo probe takes over. The stub
// accepts every probe, so a gate still trusting the stale mirror would refuse.
func TestTeamReposPut_StaleMirrorFallsToTheColdPath(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var probed atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/user/repos" {
			w.Header().Set("Content-Type", "application/json")
			page := []map[string]any{}
			if r.URL.Query().Get("page") == "1" {
				page = append(page, map[string]any{"full_name": "owner/mirrored"})
			}
			_ = json.NewEncoder(w).Encode(page)
			return
		}
		if slug, ok := strings.CutPrefix(r.URL.Path, "/api/v3/repos/"); ok && slug != "" {
			probed.Store(true)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"full_name": "x"})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(ts.Close)
	seedPATOrg(t, srv, ts.URL)
	wireReachRefresh(t, srv).refresh(t, runmode.LocalDefaultOrgID, true)

	// Fresh mirror: a repository it does not hold is refused without any probe.
	absent := registerBare(t, srv, hostOf(t, srv), "owner/absent")
	if rec := putTracked(t, srv, absent); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("fresh-mirror PUT of an absent repository = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if probed.Load() {
		t.Error("the fresh mirror should have decided the write; nothing should have probed GitHub")
	}

	// Aged past what the gate trusts: the probe decides instead, and it says yes.
	ageMirror(t, srv.db, reachableGateMaxAge+time.Hour)
	if rec := putTracked(t, srv, absent); rec.Code != http.StatusOK {
		t.Fatalf("stale-mirror PUT = %d, want 200 (the probe decides); body=%s", rec.Code, rec.Body.String())
	}
	if !probed.Load() {
		t.Error("a stale mirror must fall to the per-slug probe rather than deciding on rows it no longer vouches for")
	}
}

// TestPickerOptionsNameTheirRegistryRow: an option names the registry row that
// carries its GitHub id. A row under the same name with another GitHub id is
// another repository, so that option, like one with no row, names none.
func TestPickerOptionsNameTheirRegistryRow(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	gh := newCountingPATGitHub(t, "acme/api", "acme/web", "acme/cli")
	seedPATOrg(t, srv, gh.URL)
	api := seedUntrackedRepo(t, srv, "acme", "api")
	web := seedUntrackedRepo(t, srv, "acme", "web")
	for id, external := range map[string]string{api: "1", web: "99"} {
		execSQL(t, srv.db, `UPDATE repositories SET external_id = ? WHERE id = ?`, external, id)
	}
	wireReachRefresh(t, srv).refresh(t, runmode.LocalDefaultOrgID, true)

	page := fetchPickerPage(t, srv, map[string]any{})
	want := map[string]string{"acme/api": api, "acme/web": "", "acme/cli": ""}
	if len(page.Items) != len(want) {
		t.Fatalf("items = %+v; want %d options", page.Items, len(want))
	}
	for _, item := range page.Items {
		if item.RepositoryID != want[item.FullName] {
			t.Errorf("%s repository_id = %q; want %q", item.FullName, item.RepositoryID, want[item.FullName])
		}
	}
}

// TestPickerRejectsATokenFromAnotherFilter: the search runs in the store and
// narrows both the page and the total, and a page token minted under one search
// cannot address a page of another.
func TestPickerRejectsATokenFromAnotherFilter(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	gh := newCountingPATGitHub(t, "acme/api", "acme/web", "acme/cli")
	seedPATOrg(t, srv, gh.URL)
	wireReachRefresh(t, srv).refresh(t, runmode.LocalDefaultOrgID, true)

	first := fetchPickerPage(t, srv, map[string]any{"page_size": 1})
	if first.NextPageToken == "" || first.TotalCount != 3 {
		t.Fatalf("first page = total %d, next token %q; want 3 and a token with a page size of one", first.TotalCount, first.NextPageToken)
	}
	spent := gh.userRepos.Load()
	filtered := fetchPickerPage(t, srv, map[string]any{"q": "cli"})
	if filtered.TotalCount != 1 || len(filtered.Items) != 1 {
		t.Errorf("filtered page = %d items / total %d; want 1 and 1 — the total is what the filter matches",
			len(filtered.Items), filtered.TotalCount)
	}
	if got := gh.userRepos.Load(); got != spent {
		t.Errorf("%d upstream requests after a filtered read; want the %d the refresh spent", got, spent)
	}
	rec := doJSON(t, srv, http.MethodPost, "/api/github/repos/list", map[string]any{
		"q": "cli", "page_token": first.NextPageToken,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("token reused under a different filter = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestGitHubReposRefreshForcesPastTheTTL pins the explicit control the TTL
// requires: without it, a repository granted since the last refresh stays
// invisible for the whole window.
func TestGitHubReposRefreshForcesPastTheTTL(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	gh := newCountingPATGitHub(t, "acme/api")
	seedPATOrg(t, srv, gh.URL)
	reach := wireReachRefresh(t, srv)
	reach.refresh(t, runmode.LocalDefaultOrgID, false)
	spent := gh.userRepos.Load()

	rec := doJSON(t, srv, http.MethodPost, "/api/github/repos/refresh", map[string]any{})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("refresh = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if reach.forced.Load() != 1 {
		t.Errorf("%d forced refreshes; want exactly 1", reach.forced.Load())
	}
	if got := gh.userRepos.Load(); got <= spent {
		t.Errorf("%d enumerations after the refresh control; want more than the %d before it", got, spent)
	}
}

// seedManagedOrg makes the local org one that rides the deployment's
// shared App: the class, and the installations it bound. There is deliberately
// no org_github_apps row — that table is one row per org with a UNIQUE app_id,
// so an org on a shared App cannot hold one, which is the whole reason the
// class is a stated column rather than an inference from row presence.
func seedManagedOrg(t *testing.T, s *Server, installationIDs ...string) {
	t.Helper()
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	if _, err := s.orgs.SetGitHubCredentialClass(ctx, org, domain.GitHubCredentialClassManagedApp); err != nil {
		t.Fatalf("set github credential class: %v", err)
	}
	for i, id := range installationIDs {
		if _, err := s.githubApps.UpsertInstallation(ctx, domain.OrgGitHubAppInstallation{
			InstallationID: id,
			OrgID:          org,
			AccountType:    "Organization",
			AccountLogin:   fmt.Sprintf("acct-%d", i),
			GitHubHost:     seedRepoHost(t, s),
		}); err != nil {
			t.Fatalf("upsert installation %s: %v", id, err)
		}
	}
}

// An org on the deployment's shared App reads the picker from the mirror like
// any other class: status ready, a real count, the mirrored rows in order.
func TestPickerServesAManagedOrgFromTheMirror(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	seedManagedOrg(t, srv, "111")
	if err := srv.reachableRepos.ReplaceForInstallationSystem(context.Background(),
		runmode.LocalDefaultOrgID, domain.GitHubCredentialClassManagedApp, seedRepoHost(t, srv), "111",
		[]domain.ReachableRepository{
			{OrgID: runmode.LocalDefaultOrgID, Owner: "acme", Repo: "api"},
			{OrgID: runmode.LocalDefaultOrgID, Owner: "acme", Repo: "web"},
		}); err != nil {
		t.Fatalf("seed the mirror: %v", err)
	}

	rec := doJSON(t, srv, http.MethodPost, "/api/github/repos/list", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("picker = %d for a managed org; want 200; body=%s", rec.Code, rec.Body.String())
	}
	var page pickerPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode page: %v; body=%s", err, rec.Body.String())
	}
	if page.Status != githubRepoStatusReady {
		t.Errorf("status = %q; want %q", page.Status, githubRepoStatusReady)
	}
	if page.TotalCount != 2 || len(page.Items) != 2 {
		t.Fatalf("page = %d items / total_count %d; want 2 and 2", len(page.Items), page.TotalCount)
	}
	if page.Items[0].FullName != "acme/api" || page.Items[1].FullName != "acme/web" {
		t.Errorf("items = %v; want acme/api then acme/web", page.Items)
	}
}

// A managed org that has bound nothing gets the 409 that names the missing
// installation, not the PAT tier's "GitHub is not connected", which reads as
// "add a token" and cannot help an App org.
func TestPickerManagedOrgWithNothingBoundIsNotToldToAddAToken(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	seedManagedOrg(t, srv)

	rec := doJSON(t, srv, http.MethodPost, "/api/github/repos/list", map[string]any{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("picker = %d for a managed org with nothing bound; want 409; body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "not installed on any account") {
		t.Errorf("refusal = %s; want the one that names the installation", body)
	}
}
