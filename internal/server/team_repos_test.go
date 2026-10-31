package server

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

const teamReposRoute = "/api/teams/default/github-repos"

// registerBare mints a registry row for slug on host by name, the way an
// entity create does — a row with no GitHub id — and returns its id. It is
// how these tests hold a registry row without going through the mirror.
func registerBare(t *testing.T, s *Server, host, slug string) string {
	t.Helper()
	row, err := s.allStores.Repos.FindOrCreateByRefSystem(t.Context(), runmode.LocalDefaultOrgID, domain.RepoRefFromSlug(host, slug))
	if err != nil {
		t.Fatalf("register %s on %s: %v", slug, host, err)
	}
	return row.ID
}

// putTracked writes the default team's tracked set by registry id.
func putTracked(t *testing.T, s *Server, ids ...string) *httptest.ResponseRecorder {
	t.Helper()
	if ids == nil {
		ids = []string{}
	}
	return doJSON(t, s, http.MethodPut, teamReposRoute, map[string]any{"repository_ids": ids})
}

// getTracked reads the default team's tracked set.
func getTracked(t *testing.T, s *Server) []trackedRepoJSON {
	t.Helper()
	rec := doJSON(t, s, http.MethodGet, teamReposRoute, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET tracked set = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp teamReposResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode tracked set: %v", err)
	}
	return resp.Repos
}

// trackedNames renders a tracked set as owner/repo for comparison.
func trackedNames(repos []trackedRepoJSON) []string {
	out := make([]string, 0, len(repos))
	for _, r := range repos {
		out = append(out, r.Owner+"/"+r.Repo)
	}
	return out
}

// registerByGitHubID calls POST /api/repos.
func registerByGitHubID(t *testing.T, s *Server, githubID string) *httptest.ResponseRecorder {
	t.Helper()
	return doJSON(t, s, http.MethodPost, "/api/repos", map[string]any{"github_id": githubID})
}

// registeredID calls POST /api/repos and returns the registry id, failing on
// anything but a 200.
func registeredID(t *testing.T, s *Server, githubID string) repoJSON {
	t.Helper()
	rec := registerByGitHubID(t, s, githubID)
	if rec.Code != http.StatusOK {
		t.Fatalf("register GitHub id %s = %d, want 200; body=%s", githubID, rec.Code, rec.Body.String())
	}
	var row repoJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &row); err != nil {
		t.Fatalf("decode registered row: %v", err)
	}
	return row
}

// assertRefusedFields checks rec is a status refusal naming exactly fields,
// one INVALID_FIELD error each, in order.
func assertRefusedFields(t *testing.T, rec *httptest.ResponseRecorder, status int, fields ...string) []string {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, status, rec.Body.String())
	}
	items := errorItems(t, rec)
	if len(items) != len(fields) {
		t.Fatalf("errors = %+v, want one per field %v", items, fields)
	}
	msgs := make([]string, 0, len(items))
	for i, it := range items {
		if it.Field != fields[i] {
			t.Errorf("error %d field = %q, want %q", i, it.Field, fields[i])
		}
		msgs = append(msgs, it.Message)
	}
	return msgs
}

// assertNamesByID checks that a reachability refusal names the repository by
// the id the caller sent and never by its registry name, which the caller may
// not be able to read.
func assertNamesByID(t *testing.T, msg, id, slug string) {
	t.Helper()
	if !strings.Contains(msg, id) || strings.Contains(strings.ToLower(msg), slug) {
		t.Errorf("refusal = %q, want it to name %s and not %s", msg, id, slug)
	}
}

func TestUnreachableIn(t *testing.T) {
	set := reachSet{
		ids:   map[string]domain.ReachableRepository{"11": {Owner: "owner", Repo: "renamed", ExternalID: "11"}},
		slugs: map[string]struct{}{"owner/web": {}},
	}
	repo := func(o, r, id string) domain.Repository {
		return domain.Repository{Owner: o, Repo: r, ExternalID: id}
	}

	t.Run("fail-open when not checked", func(t *testing.T) {
		// checked=false must never reject, regardless of the input — this
		// is the GitHub-unreachable / no-credentials path.
		if got := unreachableIn(reachSet{}, false, []domain.Repository{repo("ghost", "repo", "")}); got[0] {
			t.Error("an unchecked set must not reject")
		}
	})

	t.Run("a row carrying a GitHub id is matched by it, whatever its stored name", func(t *testing.T) {
		got := unreachableIn(set, true, []domain.Repository{repo("owner", "api", "11"), repo("owner", "web", "12")})
		if got[0] {
			t.Error("owner/api carries id 11, which the mirror holds under its new name; it is reachable")
		}
		if !got[1] {
			t.Error("owner/web carries id 12, which the mirror does not hold; a mirror entry under the same name is another repository")
		}
	})

	t.Run("a row with no GitHub id is matched by name, case-insensitively", func(t *testing.T) {
		got := unreachableIn(set, true, []domain.Repository{repo("Owner", "WEB", ""), repo("owner", "ghost", "")})
		if got[0] || !got[1] {
			t.Errorf("unreachable = %v, want [false true]", got)
		}
	})
}

// TestUnreachableViaFanOut_MixedOutcomes drives the cold-path orchestration
// with an injected check covering the four response shapes (200 / 404 / 403 /
// 5xx). 200 passes, 404 and 403 are conclusive rejections, and a 5xx
// (indeterminate) fails open — never rejected. The answer is index-aligned
// with the input regardless of completion order.
func TestUnreachableViaFanOut_MixedOutcomes(t *testing.T) {
	repo := func(o, r string) domain.Repository { return domain.Repository{Owner: o, Repo: r} }

	// Maps each slug to (reachable, conclusive) — the shape CheckRepoAccess
	// returns for 200 / 404 / 403 / 5xx respectively.
	outcomes := map[string][2]bool{
		"owner/ok":      {true, true},   // 200
		"owner/missing": {false, true},  // 404
		"owner/forbid":  {false, true},  // 403
		"owner/flaky":   {false, false}, // 5xx → indeterminate
	}
	check := func(_ context.Context, r domain.Repository) (bool, bool) {
		o := outcomes[r.Slug()]
		return o[0], o[1]
	}

	got := unreachableViaFanOut(context.Background(), []domain.Repository{
		repo("owner", "ok"), repo("owner", "forbid"), repo("owner", "flaky"), repo("owner", "missing"),
	}, 2, check)
	if want := []bool{false, true, false, true}; !equalBools(got, want) {
		t.Errorf("unreachable = %v, want %v", got, want)
	}
}

func equalBools(a, b []bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestTeamReposPut_MirrorIsAuthoritative proves the mirror decides membership:
// with only owner/tracked reachable, a PUT adding owner/ghost's registry row is
// refused even though the per-repo probe (cold path) would have accepted it.
func TestTeamReposPut_MirrorIsAuthoritative(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The enumeration the mirror is built from lists only owner/tracked.
		if r.URL.Path == "/api/v3/user/repos" {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("page") != "1" {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[{"full_name":"owner/tracked","id":11}]`))
			return
		}
		// Every per-repo probe would say reachable — so a cold-path fall-through
		// would wrongly accept owner/ghost. The mirror must win.
		if strings.HasPrefix(r.URL.Path, "/api/v3/repos/") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"full_name":"x"}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(ts.Close)
	seedSourceHost(t, srv, eventsource.KindGitHub, ts.URL)
	if err := integrations.Save(context.Background(), srv.secrets, runmode.LocalDefaultOrgID, auth.Credentials{
		GitHubPAT: "ghp-test",
	}); err != nil {
		t.Fatalf("seed creds: %v", err)
	}
	wireReachRefresh(t, srv).refresh(t, runmode.LocalDefaultOrgID, true)

	host := hostOf(t, srv)
	tracked := registeredID(t, srv, "11")
	ghost := registerBare(t, srv, host, "owner/ghost")

	// Mirrored, and owner/ghost is not in it → 422 on its entry.
	msgs := assertRefusedFields(t, putTracked(t, srv, tracked.ID, ghost), http.StatusUnprocessableEntity, "repository_ids[1]")
	assertNamesByID(t, msgs[0], ghost, "owner/ghost")

	// And a repository that IS mirrored is accepted without a probe.
	if rec := putTracked(t, srv, tracked.ID); rec.Code != http.StatusOK {
		t.Fatalf("reachable PUT against a fresh mirror = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// hostOf reads the org's current GitHub host.
func hostOf(t *testing.T, s *Server) string {
	t.Helper()
	host, err := s.orgGitHubHost(t.Context(), runmode.LocalDefaultOrgID)
	if err != nil || host == "" {
		t.Fatalf("org github host = %q, %v", host, err)
	}
	return host
}

// TestTeamReposPut_ColdPathFailsOpenOn5xx proves the cold-path fan-out keeps
// the documented fail-open posture: when the per-repo probe 5xxs (GitHub
// outage), the write proceeds rather than coupling write availability to
// GitHub's uptime. No cache is warmed, so the PUT takes the cold path.
func TestTeamReposPut_ColdPathFailsOpenOn5xx(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var probed atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/user/repos" {
			// Empty enumeration — irrelevant here, the test never warms the cache.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/v3/repos/") {
			probed.Store(true)
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(ts.Close)
	seedSourceHost(t, srv, eventsource.KindGitHub, ts.URL)
	if err := integrations.Save(context.Background(), srv.secrets, runmode.LocalDefaultOrgID, auth.Credentials{
		GitHubPAT: "ghp-test",
	}); err != nil {
		t.Fatalf("seed creds: %v", err)
	}

	if rec := putTracked(t, srv, registerBare(t, srv, hostOf(t, srv), "owner/whatever")); rec.Code != http.StatusOK {
		t.Fatalf("cold-path 5xx PUT = %d, want 200 (fail-open); body=%s", rec.Code, rec.Body.String())
	}
	if !probed.Load() {
		t.Error("expected the cold path to actually probe the per-repo endpoint")
	}
}

// fakeReposServer stands in for GitHub, serving the subset of endpoints the
// repo-tracking gate touches against a fixed reachable set (the given
// full_names):
//
//   - GET /user/repos — the picker enumeration (cache-warm path). Returns the
//     full_names on page 1, an empty page afterward so the paginator stops.
//   - GET /repos/{owner}/{repo} — the cold-path reachability probe
//     (CheckRepoAccess). 200 when the slug is in the reachable set, 404
//     otherwise — exactly the signal the per-repo fan-out classifies.
func fakeReposServer(t *testing.T, fullNames ...string) *httptest.Server {
	t.Helper()
	reachable := make(map[string]bool, len(fullNames))
	for _, fn := range fullNames {
		reachable[strings.ToLower(fn)] = true
	}
	writeRepoJSON := func(w http.ResponseWriter, fullName string) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ghclient.UserRepo{FullName: fullName})
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Picker enumeration (cache-warm path).
		if r.URL.Path == "/api/v3/user/repos" {
			w.Header().Set("Content-Type", "application/json")
			page := []ghclient.UserRepo{}
			if r.URL.Query().Get("page") == "1" {
				for _, fn := range fullNames {
					page = append(page, ghclient.UserRepo{FullName: fn})
				}
			}
			_ = json.NewEncoder(w).Encode(page)
			return
		}
		// Per-repo reachability probe (cold-path fan-out).
		if slug, ok := strings.CutPrefix(r.URL.Path, "/api/v3/repos/"); ok && slug != "" {
			if reachable[strings.ToLower(slug)] {
				writeRepoJSON(w, slug)
				return
			}
			http.NotFound(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// mutablePATGitHub serves GET /user/repos — the PAT enumeration the mirror is
// built from — over a slug→id map a test can rename and extend between
// refreshes, so a case can stage what GitHub reports changing under TF. Every
// per-repo probe answers 404, so a test that means the mirror to decide fails
// rather than passing on the cold path. Every repository object names its
// owner account, by mutablePATOwnerID.
type mutablePATGitHub struct {
	*httptest.Server
	mu    sync.Mutex
	repos map[string]int64
}

func newMutablePATGitHub(t *testing.T, repos map[string]int64) *mutablePATGitHub {
	t.Helper()
	g := &mutablePATGitHub{repos: maps.Clone(repos)}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/user/repos", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		page := []ghclient.UserRepo{}
		if r.URL.Query().Get("page") == "1" {
			g.mu.Lock()
			for slug, id := range g.repos {
				page = append(page, ghclient.UserRepo{ID: id, FullName: slug, Owner: mutablePATOwner(slug)})
			}
			g.mu.Unlock()
		}
		_ = json.NewEncoder(w).Encode(page)
	})
	mux.HandleFunc("/api/v3/repos/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	// A repository read by its id answers under its current name, and a
	// deleted one — an id nothing is listed under any more — is a 404.
	mux.HandleFunc("/api/v3/repositories/", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/api/v3/repositories/"), 10, 64)
		g.mu.Lock()
		defer g.mu.Unlock()
		for slug, have := range g.repos {
			if err == nil && have == id {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(ghclient.RepoMeta{ID: id, FullName: slug, Owner: mutablePATOwner(slug)})
				return
			}
		}
		http.NotFound(w, r)
	})
	g.Server = httptest.NewServer(mux)
	t.Cleanup(g.Close)
	return g
}

// mutablePATOwnerID is the GitHub account id mutablePATGitHub reports for the
// owner "acme". Other owners are reported with none.
const mutablePATOwnerID = 9001

// mutablePATOwner is the owner object mutablePATGitHub sends for slug.
func mutablePATOwner(slug string) ghclient.RepoOwner {
	if owner, _, _ := strings.Cut(slug, "/"); owner == "acme" {
		return ghclient.RepoOwner{ID: mutablePATOwnerID}
	}
	return ghclient.RepoOwner{}
}

// rename moves a repository to a new name, keeping its id.
func (g *mutablePATGitHub) rename(from, to string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.repos[to] = g.repos[from]
	delete(g.repos, from)
}

// add lists a repository under slug, replacing whatever held the name.
func (g *mutablePATGitHub) add(slug string, id int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.repos[slug] = id
}

// TestTeamReposPut_RejectsUnreachableRepo proves the write-time guard: with
// credentials that can reach owner/tracked but not owner/ghost or owner/gone,
// the PUT accepts the former and refuses each of the latter by its own entry.
func TestTeamReposPut_RejectsUnreachableRepo(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	ts := fakeReposServer(t, "owner/tracked")
	seedSourceHost(t, srv, eventsource.KindGitHub, ts.URL)
	if err := integrations.Save(context.Background(), srv.secrets, runmode.LocalDefaultOrgID, auth.Credentials{
		GitHubPAT: "ghp-test",
	}); err != nil {
		t.Fatalf("seed creds: %v", err)
	}
	host := hostOf(t, srv)
	tracked := registerBare(t, srv, host, "owner/tracked")

	// Reachable repo → accepted.
	if rec := putTracked(t, srv, tracked); rec.Code != http.StatusOK {
		t.Fatalf("reachable repo PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Unreachable repos → refused before any write, one error each.
	ghost, gone := registerBare(t, srv, host, "owner/ghost"), registerBare(t, srv, host, "owner/gone")
	msgs := assertRefusedFields(t, putTracked(t, srv, tracked, ghost, gone),
		http.StatusUnprocessableEntity, "repository_ids[1]", "repository_ids[2]")
	assertNamesByID(t, msgs[0], ghost, "owner/ghost")
	assertNamesByID(t, msgs[1], gone, "owner/gone")
	if got := trackedNames(getTracked(t, srv)); !equalSlugs(got, []string{"owner/tracked"}) {
		t.Errorf("tracked after the refusal = %v, want it unchanged", got)
	}
}

// TestTeamReposPut_StaleTrackedRepoStaysRemovable proves the guard only
// checks NEWLY-ADDED repos: a repo tracked while reachable that later
// becomes unreachable (creds revoked, App uninstalled) must not wedge the
// picker. The user has to be able to save a corrected set — including one
// that still carries the stale repo, or that drops it — without the guard
// refusing a repo it can no longer reach.
func TestTeamReposPut_StaleTrackedRepoStaysRemovable(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)

	// One GitHub host whose answers change between the phases: the access is
	// revoked on the same GitHub, not moved to another one.
	wide := fakeReposServer(t, "owner/stale", "owner/other")
	narrow := fakeReposServer(t, "owner/other", "owner/fresh")
	var current atomic.Pointer[httptest.Server]
	current.Store(wide)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current.Load().Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(gh.Close)

	// Phase 1: creds can reach owner/stale + owner/other → track both.
	seedSourceHost(t, srv, eventsource.KindGitHub, gh.URL)
	if err := integrations.Save(context.Background(), srv.secrets, runmode.LocalDefaultOrgID, auth.Credentials{
		GitHubPAT: "ghp-test",
	}); err != nil {
		t.Fatalf("seed creds: %v", err)
	}
	host := hostOf(t, srv)
	stale, other := registerBare(t, srv, host, "owner/stale"), registerBare(t, srv, host, "owner/other")
	if rec := putTracked(t, srv, stale, other); rec.Code != http.StatusOK {
		t.Fatalf("initial track PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Phase 2: access to owner/stale is revoked — creds now reach only
	// owner/other (and a new owner/fresh the user wants to add).
	current.Store(narrow)

	// Re-saving the current set (still carrying the now-unreachable
	// owner/stale) must succeed — it's already tracked, so it's not
	// reachability-checked.
	if rec := putTracked(t, srv, stale, other); rec.Code != http.StatusOK {
		t.Fatalf("re-save with stale tracked repo = %d, want 200 (already tracked, not re-checked); body=%s", rec.Code, rec.Body.String())
	}

	// Dropping owner/stale while ADDING a reachable owner/fresh must
	// succeed: the only newly-added repo (owner/fresh) is reachable.
	if rec := putTracked(t, srv, other, registerBare(t, srv, host, "owner/fresh")); rec.Code != http.StatusOK {
		t.Fatalf("drop-stale + add-reachable PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Control: a NEWLY-added unreachable repo is still refused — the guard
	// didn't go slack, it just scopes to additions.
	ghost := registerBare(t, srv, host, "owner/ghost")
	msgs := assertRefusedFields(t, putTracked(t, srv, other, ghost),
		http.StatusUnprocessableEntity, "repository_ids[1]")
	assertNamesByID(t, msgs[0], ghost, "owner/ghost")
}

// TestTeamReposPut_FailsOpenWithoutCredentials proves the guard never
// blocks a write when it can't enumerate the reachable set — no
// credentials configured means no clients to probe with, so the save proceeds.
func TestTeamReposPut_FailsOpenWithoutCredentials(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	if rec := putTracked(t, srv, registerBare(t, srv, hostOf(t, srv), "anyone/anything")); rec.Code != http.StatusOK {
		t.Fatalf("no-credential PUT = %d, want 200 (fail-open); body=%s", rec.Code, rec.Body.String())
	}
}

// TestTeamReposPut_RefusesABadBody pins the body contract: the field is
// required (an absent one is not an empty set), every entry is a registry id
// in canonical form named once — each fault reported, 400 — and every
// well-formed id that names no repository on the org's host is refused by its
// own entry, 422. Nothing is written by any of them.
func TestTeamReposPut_RefusesABadBody(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	host := hostOf(t, srv)
	api := registerBare(t, srv, host, "acme/api")
	if rec := putTracked(t, srv, api); rec.Code != http.StatusOK {
		t.Fatalf("seed PUT = %d; body=%s", rec.Code, rec.Body.String())
	}

	rec := doJSON(t, srv, http.MethodPut, teamReposRoute, map[string]any{})
	if rec.Code != http.StatusBadRequest || firstErrorReason(t, rec) != httpx.ReasonMissingField {
		t.Errorf("absent repository_ids = %d %s, want 400 %s", rec.Code, rec.Body.String(), httpx.ReasonMissingField)
	}
	if rec := doJSON(t, srv, http.MethodPut, teamReposRoute, map[string]any{"repos": []string{"acme/api"}}); rec.Code != http.StatusBadRequest {
		t.Errorf("an owner/name body = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	assertRefusedFields(t, putTracked(t, srv, api, "acme/api", strings.ToUpper(api), api),
		http.StatusBadRequest, "repository_ids[1]", "repository_ids[2]", "repository_ids[3]")

	elsewhere := registerBare(t, srv, "https://ghe.example.com", "acme/api")
	msgs := assertRefusedFields(t, putTracked(t, srv, "00000000-0000-4000-8000-0000000000aa", api, elsewhere),
		http.StatusUnprocessableEntity, "repository_ids[0]", "repository_ids[2]")
	for _, m := range msgs {
		if !strings.Contains(m, "not a repository on this organization's GitHub host") {
			t.Errorf("refusal %q should say the id names no repository on the host", m)
		}
	}
	if got := getTracked(t, srv); len(got) != 1 || got[0].ID != api {
		t.Errorf("tracked after the refusals = %+v, want only %s", got, api)
	}
}

// TestTeamRepos_ARenameKeepsTheSameRow pins that the set is addressed by
// registry id end to end: a client that read the set before GitHub renamed a
// repository writes it back after the rename, and the team keeps tracking the
// same registry row, now under the new name.
func TestTeamRepos_ARenameKeepsTheSameRow(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	gh := newMutablePATGitHub(t, map[string]int64{"acme/api": 31})
	seedPATOrg(t, srv, gh.URL)
	wireReachRefresh(t, srv).refresh(t, runmode.LocalDefaultOrgID, true)

	api := registeredID(t, srv, "31")
	if api.GitHubID != "31" || api.Slug != "acme/api" {
		t.Fatalf("registered row = %+v, want acme/api carrying GitHub id 31", api)
	}
	if rec := putTracked(t, srv, api.ID); rec.Code != http.StatusOK {
		t.Fatalf("track = %d; body=%s", rec.Code, rec.Body.String())
	}
	read := getTracked(t, srv)
	if len(read) != 1 || read[0] != (trackedRepoJSON{ID: api.ID, GitHubID: "31", Owner: "acme", Repo: "api"}) {
		t.Fatalf("GET = %+v, want acme/api by id %s and GitHub id 31", read, api.ID)
	}

	if _, err := srv.allStores.Repos.RenameSystem(t.Context(), runmode.LocalDefaultOrgID, domain.RepoRef{
		Host: hostOf(t, srv), Owner: "acme", Repo: "platform-api", ExternalID: "31",
	}); err != nil {
		t.Fatalf("RenameSystem: %v", err)
	}
	rec := putTracked(t, srv, read[0].ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("save after the rename = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var saved teamReposPutResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatalf("decode PUT response: %v", err)
	}
	want := trackedRepoJSON{ID: api.ID, GitHubID: "31", Owner: "acme", Repo: "platform-api"}
	if len(saved.Repos) != 1 || saved.Repos[0] != want {
		t.Errorf("PUT echoed %+v, want %+v", saved.Repos, want)
	}
}

// TestTeamRepos_ARowWithNoGitHubIDSurvivesARoundTrip pins that a tracked
// registry row TF never learned a GitHub id for is still nameable: the GET
// hands it back by registry id with an empty github_id, and writing the GET
// back keeps it.
func TestTeamRepos_ARowWithNoGitHubIDSurvivesARoundTrip(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	bare := registerBare(t, srv, hostOf(t, srv), "acme/legacy")
	if rec := putTracked(t, srv, bare); rec.Code != http.StatusOK {
		t.Fatalf("track = %d; body=%s", rec.Code, rec.Body.String())
	}
	read := getTracked(t, srv)
	if len(read) != 1 || read[0].ID != bare || read[0].GitHubID != "" {
		t.Fatalf("GET = %+v, want %s with no GitHub id", read, bare)
	}
	ids := make([]string, 0, len(read))
	for _, r := range read {
		ids = append(ids, r.ID)
	}
	if rec := putTracked(t, srv, ids...); rec.Code != http.StatusOK {
		t.Fatalf("write back = %d; body=%s", rec.Code, rec.Body.String())
	}
	if again := getTracked(t, srv); len(again) != 1 || again[0].ID != bare {
		t.Errorf("after writing the GET back = %+v, want %s still tracked", again, bare)
	}
}

// TestRepoRegister pins POST /api/repos: the id is validated strictly, the
// mirror supplies the name and refuses an id it does not list, and a
// registration is idempotent — and resolves to the row TF holds when GitHub
// has renamed the repository since. A name TF holds under another id is freed
// by asking GitHub about that id: a deleted repository's row is retired, a
// renamed one's is renamed, and the new repository gets a row of its own.
func TestRepoRegister(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	gh := newMutablePATGitHub(t, map[string]int64{"acme/api": 41, "acme/web": 42})
	seedPATOrg(t, srv, gh.URL)
	refresh := wireReachRefresh(t, srv)
	refresh.refresh(t, runmode.LocalDefaultOrgID, true)

	for _, body := range []map[string]any{{}, {"github_id": ""}} {
		rec := doJSON(t, srv, http.MethodPost, "/api/repos", body)
		if rec.Code != http.StatusBadRequest || firstErrorReason(t, rec) != httpx.ReasonMissingField {
			t.Errorf("POST %v = %d %s, want 400 %s", body, rec.Code, rec.Body.String(), httpx.ReasonMissingField)
		}
	}
	for _, id := range []string{"acme/api", "0", "041", " 41", "-41", "4.1"} {
		assertRefusedFields(t, registerByGitHubID(t, srv, id), http.StatusBadRequest, "github_id")
	}
	assertRefusedFields(t, registerByGitHubID(t, srv, "99"), http.StatusUnprocessableEntity, "github_id")

	api := registeredID(t, srv, "41")
	if api.Slug != "acme/api" || api.GitHubID != "41" || !api.CanEdit {
		t.Errorf("registered = %+v, want acme/api carrying 41, editable by the local admin", api)
	}
	// The mirror entry names the owner account by id, and the row records it.
	if row, err := srv.allStores.Repos.GetSystem(t.Context(), runmode.LocalDefaultOrgID, api.ID); err != nil || row.OwnerExternalID != strconv.Itoa(mutablePATOwnerID) {
		t.Errorf("registered row = %+v, err=%v; want owner id %d from the mirror", row, err, mutablePATOwnerID)
	}
	if again := registeredID(t, srv, "41"); again.ID != api.ID {
		t.Errorf("second registration = %s, want %s", again.ID, api.ID)
	}
	if names, err := srv.allStores.Repos.ListTrackedNamesSystem(t.Context(), runmode.LocalDefaultOrgID, hostOf(t, srv)); err != nil || len(names) != 0 {
		t.Errorf("tracked names after registering = %v, %v; want none — registering is not tracking", names, err)
	}

	// GitHub renames acme/web to acme/website and the mirror sees it before
	// TF's poll applies it: the registration is still the row TF holds.
	web := registeredID(t, srv, "42")
	gh.rename("acme/web", "acme/website")
	refresh.refresh(t, runmode.LocalDefaultOrgID, true)
	if again := registeredID(t, srv, "42"); again.ID != web.ID || again.Slug != "acme/web" {
		t.Errorf("registration after an unapplied rename = %s %s, want %s under its stored name", again.ID, again.Slug, web.ID)
	}

	// acme/api is deleted and a new repository takes the name. The row TF
	// holds for 41 is retired, never re-identified, and 43 is a row of its
	// own.
	gh.add("acme/api", 43)
	refresh.refresh(t, runmode.LocalDefaultOrgID, true)
	recreated := registeredID(t, srv, "43")
	if recreated.ID == api.ID || recreated.Slug != "acme/api" || recreated.GitHubID != "43" {
		t.Errorf("registration of the recreated acme/api = %+v, want a new row carrying 43", recreated)
	}
	if old, err := srv.allStores.Repos.GetSystem(t.Context(), runmode.LocalDefaultOrgID, api.ID); err != nil || old.RetiredAt == nil || old.ExternalID != "41" {
		t.Errorf("deleted acme/api's row = %+v, err=%v; want it retired with its id", old, err)
	}

	// acme/web (stored under its old name, 42) is joined by a new acme/web.
	// GitHub answers for 42 under acme/website, so that row is renamed — not
	// retired — and the new repository registers beside it.
	gh.add("acme/web", 44)
	refresh.refresh(t, runmode.LocalDefaultOrgID, true)
	if fresh := registeredID(t, srv, "44"); fresh.ID == web.ID || fresh.Slug != "acme/web" {
		t.Errorf("registration of the new acme/web = %+v, want a new row", fresh)
	}
	if moved, err := srv.allStores.Repos.GetSystem(t.Context(), runmode.LocalDefaultOrgID, web.ID); err != nil || moved.RetiredAt != nil || moved.Slug() != "acme/website" {
		t.Errorf("acme/web's old row = %+v, err=%v; want it live under acme/website", moved, err)
	}
}

// TestTeamRepos_OperateOnTheCurrentHostOnly pins that a team's tracked set is
// per GitHub host. Once the org points at another host, the GET lists only that
// host's tracking (none yet), a PUT records the new host's set without pruning
// the old host's rows, and pointing the org back shows the old set unchanged.
// No credentials are bound, so the reachability gate fails open and the test is
// about the host alone.
func TestTeamRepos_OperateOnTheCurrentHostOnly(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	srv := newTestServer(t)
	put := func(ids ...string) {
		t.Helper()
		if rec := putTracked(t, srv, ids...); rec.Code != http.StatusOK {
			t.Fatalf("PUT %v = %d, want 200; body=%s", ids, rec.Code, rec.Body.String())
		}
	}
	get := func() []string {
		t.Helper()
		return trackedNames(getTracked(t, srv))
	}

	const ghe = "https://ghe.example.com"
	dotcomAPI := registerBare(t, srv, dbtest.TestGitHubHost, "acme/api")
	put(dotcomAPI, registerBare(t, srv, dbtest.TestGitHubHost, "acme/web"))
	if got := get(); !equalSlugs(got, []string{"acme/api", "acme/web"}) {
		t.Fatalf("tracked on the default host = %v, want [acme/api acme/web]", got)
	}

	setOrgGitHubBase(t, srv, ghe)
	if got := get(); len(got) != 0 {
		t.Fatalf("tracked on the new host = %v, want none: the old host's tracking is not this host's", got)
	}
	// The old host's row is not a repository on this one.
	assertRefusedFields(t, putTracked(t, srv, dotcomAPI), http.StatusUnprocessableEntity, "repository_ids[0]")
	// acme/api again on purpose: the same name on another host is another
	// repository.
	put(registerBare(t, srv, ghe, "acme/api"), registerBare(t, srv, ghe, "ghe/tool"))
	if got := get(); !equalSlugs(got, []string{"acme/api", "ghe/tool"}) {
		t.Errorf("tracked on the new host = %v, want [acme/api ghe/tool]", got)
	}
	onNew, err := srv.allStores.TeamGitHubRepos.ListForTeamSystem(t.Context(), runmode.LocalDefaultTeamID, ghe)
	if err != nil {
		t.Fatalf("ListForTeamSystem: %v", err)
	}
	if len(onNew) != 2 {
		t.Errorf("rows stored on the new host = %+v, want 2", onNew)
	}

	setOrgGitHubBase(t, srv, dbtest.TestGitHubHost)
	if got := get(); !equalSlugs(got, []string{"acme/api", "acme/web"}) {
		t.Errorf("tracked back on the default host = %v, want the original [acme/api acme/web] unpruned", got)
	}
}
