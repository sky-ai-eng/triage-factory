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
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/sidecarproto"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// seedRepoStore embeds db.RepoStore as nil and answers only GetSystem — the one
// read the rehydrate's seed resolution makes. profile nil models a repo with no
// row; err models a store failure.
type seedRepoStore struct {
	db.RepoStore
	profile *domain.RepoProfile
	err     error
	gotIDs  []string
}

func (s *seedRepoStore) GetSystem(_ context.Context, _, repoID string) (*domain.RepoProfile, error) {
	s.gotIDs = append(s.gotIDs, repoID)
	return s.profile, s.err
}

var _ db.RepoStore = (*seedRepoStore)(nil)

// proxySandbox is an executorSandbox carrying only the git-proxy coordinates —
// everything gitSeedFor reads. Close() is never called on it (the seed path
// doesn't own it) and every other field stays nil.
func proxySandbox(proxyURL, proxyToken string) *executorSandbox {
	return &executorSandbox{res: &sidecarproto.StartProxiesResult{GitProxyURL: proxyURL, GitProxyToken: proxyToken}}
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
	repos := &seedRepoStore{profile: &domain.RepoProfile{Owner: "acme", Repo: "widgets", CloneURL: cloneURL}}
	s := NewSpawner(nil, db.Stores{Repos: repos}, nil, nil, "")

	seed := s.gitSeedFor(context.Background(), "org-1", "acme", "widgets", proxySandbox("http://10.42.0.1:4100", "per-run-placeholder"))

	if seed.owner != "acme" || seed.repo != "widgets" {
		t.Errorf("seed repo = %s/%s, want acme/widgets", seed.owner, seed.repo)
	}
	if seed.cloneURL != cloneURL {
		t.Errorf("seed clone URL = %q, want the profile's %q — a missing bare cannot be seeded without it", seed.cloneURL, cloneURL)
	}
	if len(repos.gotIDs) != 1 || repos.gotIDs[0] != "acme/widgets" {
		t.Errorf("repo profile lookups = %v, want one for %q", repos.gotIDs, "acme/widgets")
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
// already here). With no profile row the insteadOf falls back to the org's git
// host base — the upstream the sidecar's proxy relays to — so the rebuild is
// still authenticated; only the seed-a-missing-bare half degrades.
func TestGitSeedFor_NoProfileURLStillAuthenticatesViaOrgGitHost(t *testing.T) {
	s := NewSpawner(nil, db.Stores{Repos: &seedRepoStore{profile: nil}}, nil, nil, "")
	s.SetRunCredentialResolvers(&fakeResolver{baseURL: "https://ghe.acme.dev"}, nil, nil)

	seed := s.gitSeedFor(context.Background(), "org-1", "acme", "widgets", proxySandbox("http://10.42.0.1:4100", "ph"))

	if seed.cloneURL != "" {
		t.Errorf("seed clone URL = %q, want empty (no profile row to read one from)", seed.cloneURL)
	}
	assertEntries(t, seed.auth.GitConfigEntries(),
		wantProxyEntries("http://10.42.0.1:4100", "https://ghe.acme.dev", "ph"))
}

// TestGitSeedFor_ProfileReadFailureDoesNotStrandTheRebuild: a store error is
// logged, not fatal — the seed still authenticates off the org git host so a
// rehydrate onto an existing bare survives a transient read failure.
func TestGitSeedFor_ProfileReadFailureDoesNotStrandTheRebuild(t *testing.T) {
	s := NewSpawner(nil, db.Stores{Repos: &seedRepoStore{err: errors.New("boom")}}, nil, nil, "")
	s.SetRunCredentialResolvers(&fakeResolver{}, nil, nil)

	seed := s.gitSeedFor(context.Background(), "org-1", "acme", "widgets", proxySandbox("http://10.42.0.1:4100", "ph"))

	if seed.cloneURL != "" {
		t.Errorf("seed clone URL = %q, want empty after a failed profile read", seed.cloneURL)
	}
	if len(seed.auth.GitConfigEntries()) == 0 {
		t.Error("a failed profile read left the rebuild unauthenticated; the org git host base is the fallback insteadOf")
	}
}

// TestGitSeedFor_LocalCarriesCloneURLAndNoCredential pins the local-mode half of
// the contract: ambient git credentials are local's design, so there is no
// sandbox and no injected credential — but the clone URL still rides along, so a
// local rehydrate whose bare is gone can seed one instead of erroring.
func TestGitSeedFor_LocalCarriesCloneURLAndNoCredential(t *testing.T) {
	const cloneURL = "https://github.com/acme/widgets.git"
	s := NewSpawner(nil, db.Stores{Repos: &seedRepoStore{profile: &domain.RepoProfile{CloneURL: cloneURL}}}, nil, nil, "")

	seed := s.gitSeedFor(context.Background(), runmode.LocalDefaultOrgID, "acme", "widgets", nil)

	if seed.cloneURL != cloneURL {
		t.Errorf("seed clone URL = %q, want %q", seed.cloneURL, cloneURL)
	}
	if entries := seed.auth.GitConfigEntries(); len(entries) != 0 {
		t.Errorf("local seed injected git config %v; local mode must run on the operator's own credentials", entries)
	}
}

// TestGitSeedFor_NonGitRunRootIsZero: a Jira/Slack lazy run-root names no repo,
// so nothing is read and nothing is injected.
func TestGitSeedFor_NonGitRunRootIsZero(t *testing.T) {
	repos := &seedRepoStore{profile: &domain.RepoProfile{CloneURL: "https://github.com/acme/widgets.git"}}
	s := NewSpawner(nil, db.Stores{Repos: repos}, nil, nil, "")

	if seed := s.gitSeedFor(context.Background(), "org-1", "", "", proxySandbox("http://10.42.0.1:4100", "ph")); seed != (gitSeed{}) {
		t.Errorf("seed for a non-git run root = %+v, want the zero value", seed)
	}
	if len(repos.gotIDs) != 0 {
		t.Errorf("repo profile was read for a non-git run root: %v", repos.gotIDs)
	}
}

// TestEnsureWorkspace_ColdRehydrate_HandsGitTheProxyCredential is the end of the
// wire: a cold rehydrate — the designed resume path for a concluded blueprint,
// whose worktree is deliberately torn down — must reach git with the run's
// proxy routing AND the upstream URL. Pre-fix it reached git with neither, and
// `git worktree add` died fetching anonymously.
func TestEnsureWorkspace_ColdRehydrate_HandsGitTheProxyCredential(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)

	const cloneURL = "https://github.com/acme/widgets.git"
	repos := &seedRepoStore{profile: &domain.RepoProfile{CloneURL: cloneURL}}
	s := newStorageSpawner(t)
	s.repos = repos

	const runID = "wt-proxy-auth"
	wtPath, owner, repo := setupTestWorktree(t, runID)
	t.Cleanup(func() { _ = worktree.RemoveAt(wtPath, runID) })
	writeSession(t, wtPath, "sess-proxy", `{"type":"summary"}`)
	if err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, runID, wtPath, "sess-proxy"); err != nil {
		t.Fatalf("snapshotWorkspace: %v", err)
	}
	// The concluded blueprint's worktree is gone — cold rehydrate.
	if err := os.RemoveAll(wtPath); err != nil {
		t.Fatalf("rm worktree: %v", err)
	}

	var (
		gotURL  string
		gotAuth worktree.CloneAuth
		called  bool
	)
	restore := restoreWorkspaceGit
	restoreWorkspaceGit = func(ctx context.Context, o, r, wtDir string, d *worktree.GitDelta, url string, auth worktree.CloneAuth) error {
		called, gotURL, gotAuth = true, url, auth
		return restore(ctx, o, r, wtDir, d, url, auth)
	}
	t.Cleanup(func() { restoreWorkspaceGit = restore })

	sandbox := proxySandbox("http://10.42.0.3:4100", "run-placeholder")
	run := &domain.Conversation{ID: runID, WorktreePath: wtPath, BlueprintRunID: runID}
	seed := s.gitSeedFor(context.Background(), runmode.LocalDefaultOrgID, owner, repo, sandbox)
	if _, _, err := s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, run, seed); err != nil {
		t.Fatalf("ensureWorkspace (cold): %v", err)
	}

	if !called {
		t.Fatal("cold rehydrate never reached the git restore")
	}
	if gotURL != cloneURL {
		t.Errorf("restore clone URL = %q, want %q", gotURL, cloneURL)
	}
	assertEntries(t, gotAuth.GitConfigEntries(),
		wantProxyEntries("http://10.42.0.3:4100", "https://github.com", "run-placeholder"))
}

// TestEnsureWorkspace_ColdRehydrate_SeedsAMissingBare covers the other half the
// wire un-strands: an executor that never ran this repo has no bare at all.
// With the clone URL threaded through, the rebuild seeds one and completes;
// before, it stopped at "bare missing and no clone URL to seed it".
//
// The upstream here is a local bare (so the seed needs no credential and no
// network); the credential half of the same call is pinned by the test above
// and, end to end against an authenticating remote, by
// worktree.TestRestoreWorkspaceGit_BloblessPrivateRepo.
func TestEnsureWorkspace_ColdRehydrate_SeedsAMissingBare(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)

	const runID = "wt-seed-bare"
	wtPath, owner, repo := setupTestWorktree(t, runID)
	t.Cleanup(func() { _ = worktree.RemoveAt(wtPath, runID) })

	writeFile(t, filepath.Join(wtPath, "agent.txt"), "committed by agent")
	gitT(t, wtPath, "add", "agent.txt")
	gitT(t, wtPath, "commit", "-m", "agent work")

	s := newStorageSpawner(t)
	if err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, runID, wtPath, ""); err != nil {
		t.Fatalf("snapshotWorkspace: %v", err)
	}

	// The origin the bare was cloned from is the URL the profile would carry.
	bareDir, err := worktree.RepoDir(owner, repo)
	if err != nil {
		t.Fatalf("RepoDir: %v", err)
	}
	upstream := gitOriginURL(t, bareDir)

	// Fresh executor: no worktree, and no bare either.
	if err := os.RemoveAll(wtPath); err != nil {
		t.Fatalf("rm worktree: %v", err)
	}
	if err := os.RemoveAll(bareDir); err != nil {
		t.Fatalf("rm bare: %v", err)
	}
	s.repos = &seedRepoStore{profile: &domain.RepoProfile{CloneURL: upstream}}

	run := &domain.Conversation{ID: runID, WorktreePath: wtPath, BlueprintRunID: runID}
	seed := s.gitSeedFor(context.Background(), runmode.LocalDefaultOrgID, owner, repo, nil)
	got, _, err := s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, run, seed)
	if err != nil {
		t.Fatalf("ensureWorkspace with no bare on this host: %v", err)
	}
	assertFileContains(t, filepath.Join(got, "agent.txt"), "committed by agent")
}

// gitOriginURL reads a bare's origin remote — the upstream a fresh executor
// would have to re-clone from.
func gitOriginURL(t *testing.T, bareDir string) string {
	t.Helper()
	return strings.TrimSpace(gitOut(t, bareDir, "config", "--get", "remote.origin.url"))
}
