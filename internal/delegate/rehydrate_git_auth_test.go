package delegate

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/sidecarproto"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// seedRepositoryStore embeds db.RepositoryStore as nil and answers only the
// point reads a rehydrate makes: GetSystem, the seed's read of a checkout's
// repository by row id, and GetByRefSystem, an older manifest's resolution of
// a name. profile nil models a repository with no row; err models a store
// failure.
type seedRepositoryStore struct {
	db.RepositoryStore
	profile  *domain.Repository
	err      error
	gotIDs   []string
	gotNames []string
}

func (s *seedRepositoryStore) GetSystem(_ context.Context, _ string, id string) (*domain.Repository, error) {
	s.gotIDs = append(s.gotIDs, id)
	return s.profile, s.err
}

func (s *seedRepositoryStore) GetByRefSystem(_ context.Context, _ string, ref domain.RepoRef) (*domain.Repository, error) {
	s.gotNames = append(s.gotNames, ref.Slug())
	return s.profile, s.err
}

var _ db.RepositoryStore = (*seedRepositoryStore)(nil)

// proxySandbox is an runSidecar carrying only the git-proxy coordinates —
// everything gitSeedFor reads. Close() is never called on it (the seed path
// doesn't own it) and every other field stays nil.
func proxySandbox(proxyURL, proxyToken string) *runSidecar {
	return &runSidecar{res: &sidecarproto.StartProxiesResult{GitProxyURL: proxyURL, GitProxyToken: proxyToken}}
}

// wantProxyEntries is the git config a proxy-routed rebuild must run under: the
// upstream rewritten onto the run's git proxy, plus the per-run placeholder as
// the Basic password the proxy (not GitHub) authenticates.
func wantProxyEntries(proxyURL, upstream, placeholder string) [][2]string {
	return [][2]string{
		{"url." + proxyURL + "/.insteadOf", upstream + "/"},
		{"http." + proxyURL + "/.extraHeader", "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-run:"+placeholder))},
	}
}

func assertEntries(t *testing.T, got, want [][2]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("git config entries = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("git config entry %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestGitSeedFor_MultiRoutesRebuildThroughRunGitProxy is the wiring the ticket
// is about: on an executor, the seed a rehydrate hands git carries the run's
// own git-proxy routing — the identical pair setupGitHub builds for the first
// clone — so the blobless bare's lazy promisor fetch authenticates instead of
// dying on "could not read Username". Asserts the git config, not an outcome.
func TestGitSeedFor_MultiRoutesRebuildThroughRunGitProxy(t *testing.T) {
	const cloneURL = "https://github.com/acme/widgets.git"
	repos := &seedRepositoryStore{profile: &domain.Repository{Owner: "acme", Repo: "widgets", CloneURL: cloneURL}}
	s := NewSpawner(nil, db.Stores{Repos: repos}, nil, nil, "")

	seed := s.gitSeedFor(context.Background(), "org-1", "repo-widgets", "acme", "widgets", proxySandbox("http://10.42.0.1:4100", "per-run-placeholder"))

	if seed.owner != "acme" || seed.repo != "widgets" {
		t.Errorf("seed repo = %s/%s, want acme/widgets", seed.owner, seed.repo)
	}
	if seed.cloneURL != cloneURL {
		t.Errorf("seed clone URL = %q, want the repository row's %q — a missing bare cannot be seeded without it", seed.cloneURL, cloneURL)
	}
	if len(repos.gotIDs) != 1 || repos.gotIDs[0] != "repo-widgets" || len(repos.gotNames) != 0 {
		t.Errorf("repository lookups = ids %v, names %v; want one by id %q and none by name", repos.gotIDs, repos.gotNames, "repo-widgets")
	}
	assertEntries(t, seed.auth.GitConfigEntries(),
		wantProxyEntries("http://10.42.0.1:4100", "https://github.com", "per-run-placeholder"))

	// The same value setupGitHub's first clone builds — one wiring, two entry
	// points, so a change to either can't silently diverge.
	if want := worktree.CloneAuthViaGitProxy("http://10.42.0.1:4100", cloneHostBase(cloneURL), "per-run-placeholder"); seed.auth != want {
		t.Errorf("seed auth is not the eager clone's CloneAuthViaGitProxy value")
	}
}

// TestGitSeedFor_NoProfileURLStillAuthenticatesViaOrgGitHost: the promisor fetch
// needs auth whether or not a clone URL is on file (the bare it fetches from is
// already here). With no repository row the insteadOf falls back to the org's git
// host base — the upstream the sidecar's proxy relays to — so the rebuild is
// still authenticated; only the seed-a-missing-bare half degrades.
func TestGitSeedFor_NoProfileURLStillAuthenticatesViaOrgGitHost(t *testing.T) {
	s := NewSpawner(nil, db.Stores{Repos: &seedRepositoryStore{profile: nil}}, nil, nil, "")
	s.SetRunCredentialResolvers(&fakeResolver{baseURL: "https://ghe.acme.dev"}, nil, nil)

	seed := s.gitSeedFor(context.Background(), "org-1", "repo-widgets", "acme", "widgets", proxySandbox("http://10.42.0.1:4100", "ph"))

	if seed.cloneURL != "" {
		t.Errorf("seed clone URL = %q, want empty (no repository row to read one from)", seed.cloneURL)
	}
	assertEntries(t, seed.auth.GitConfigEntries(),
		wantProxyEntries("http://10.42.0.1:4100", "https://ghe.acme.dev", "ph"))
}

// TestGitSeedFor_ProfileReadFailureDoesNotStrandTheRebuild: a store error is
// logged, not fatal — the seed still authenticates off the org git host so a
// rehydrate onto an existing bare survives a transient read failure.
func TestGitSeedFor_ProfileReadFailureDoesNotStrandTheRebuild(t *testing.T) {
	s := NewSpawner(nil, db.Stores{Repos: &seedRepositoryStore{err: errors.New("boom")}}, nil, nil, "")
	s.SetRunCredentialResolvers(&fakeResolver{}, nil, nil)

	seed := s.gitSeedFor(context.Background(), "org-1", "repo-widgets", "acme", "widgets", proxySandbox("http://10.42.0.1:4100", "ph"))

	if seed.cloneURL != "" {
		t.Errorf("seed clone URL = %q, want empty after a failed repository read", seed.cloneURL)
	}
	if len(seed.auth.GitConfigEntries()) == 0 {
		t.Error("a failed repository read left the rebuild unauthenticated; the org git host base is the fallback insteadOf")
	}
}

// TestGitSeedFor_UnwiredLocalCarriesCloneURLAndNoCredential pins the fixture
// fallback: without a local channel the clone URL still rides along, but no
// credential is invented. Production dispatch starts the managed channel first.
func TestGitSeedFor_UnwiredLocalCarriesCloneURLAndNoCredential(t *testing.T) {
	const cloneURL = "https://github.com/acme/widgets.git"
	s := NewSpawner(nil, db.Stores{Repos: &seedRepositoryStore{profile: &domain.Repository{CloneURL: cloneURL}}}, nil, nil, "")

	seed := s.gitSeedFor(context.Background(), runmode.LocalDefaultOrgID, "repo-widgets", "acme", "widgets", nil)

	if seed.cloneURL != cloneURL {
		t.Errorf("seed clone URL = %q, want %q", seed.cloneURL, cloneURL)
	}
	if entries := seed.auth.GitConfigEntries(); len(entries) != 0 {
		t.Errorf("unwired local seed injected git config %v; no channel means no invented auth", entries)
	}
}

// TestEnsureWorkspace_ColdRehydrate_HandsGitTheProxyCredential is the end of the
// wire: each checkout a cold rehydrate rebuilds reaches git with the run's proxy
// routing AND the upstream URL from the repository row. Without both, the
// rebuild fetches anonymously and dies.
func TestEnsureWorkspace_ColdRehydrate_HandsGitTheProxyCredential(t *testing.T) {
	f := newSnapshotFixture(t, "task-proxy-auth")
	f.addCheckout(t, "acme/widgets", "default")
	f.snapshot(t, "", domain.ConversationRuntimeNative)
	f.loseRoot(t)

	const cloneURL = "https://github.com/acme/widgets.git"
	f.s.repos = &seedRepositoryStore{profile: &domain.Repository{CloneURL: cloneURL}}

	// Recorded, not run: the proxy address routes nowhere in a test, and the
	// arguments are the whole assertion.
	var got []worktree.CheckoutRestore
	restore := restoreCheckout
	restoreCheckout = func(_ context.Context, r worktree.CheckoutRestore) (worktree.RestoredCheckout, error) {
		got = append(got, r)
		return worktree.RestoredCheckout{Owner: r.Owner, Repo: r.Repo, Path: filepath.Join(r.Root, r.Owner, r.Repo, r.Slug)}, nil
	}
	t.Cleanup(func() { restoreCheckout = restore })

	sandbox := proxySandbox("http://10.42.0.3:4100", "run-placeholder")
	restorer := f.s.checkoutRestorerFor(runmode.LocalDefaultOrgID, "entity-unread", sandbox, nil)
	if _, _, _, err := f.s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, f.conv(""), restorer, nil); err != nil {
		t.Fatalf("ensureWorkspace (cold): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("cold rehydrate rebuilt %d checkouts, want 1", len(got))
	}
	if got[0].CloneURL != cloneURL {
		t.Errorf("restore clone URL = %q, want %q", got[0].CloneURL, cloneURL)
	}
	assertEntries(t, got[0].Auth.GitConfigEntries(),
		wantProxyEntries("http://10.42.0.3:4100", "https://github.com", "run-placeholder"))
}

// TestEnsureWorkspace_ColdRehydrate_SeedsAMissingBare: an executor that never
// ran this repo has no bare at all. With the clone URL threaded through from the
// repository row, the rebuild seeds one and completes.
func TestEnsureWorkspace_ColdRehydrate_SeedsAMissingBare(t *testing.T) {
	f := newSnapshotFixture(t, "task-seed-bare")
	co := f.addCheckout(t, "acme/widgets", "default")
	dirtyCheckout(t, co)
	f.snapshot(t, "", domain.ConversationRuntimeNative)

	// Fresh executor: no run root, and no bare either.
	f.loseRoot(t)
	bareDir, err := worktree.RepoDir(testRepositoryID("acme", "widgets"))
	if err != nil {
		t.Fatalf("RepoDir: %v", err)
	}
	upstream := gitOriginURL(t, bareDir)
	if err := os.RemoveAll(bareDir); err != nil {
		t.Fatalf("rm bare: %v", err)
	}
	f.s.repos = &seedRepositoryStore{profile: &domain.Repository{CloneURL: upstream}}

	restorer := f.s.checkoutRestorerFor(runmode.LocalDefaultOrgID, "entity-unread", nil, nil)
	if _, _, _, err := f.s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, f.conv(""), restorer, nil); err != nil {
		t.Fatalf("ensureWorkspace with no bare on this host: %v", err)
	}
	assertFileContains(t, filepath.Join(co, "committed.txt"), "unpushed commit")
}

// gitOriginURL reads a bare's origin remote — the upstream a fresh executor
// would have to re-clone from.
func gitOriginURL(t *testing.T, bareDir string) string {
	t.Helper()
	return strings.TrimSpace(gitOut(t, bareDir, "config", "--get", "remote.origin.url"))
}
