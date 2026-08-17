package credprovision

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
)

// --- fakes (embed the interface so unexercised methods compile-satisfy and
// panic only if resolveGitHub unexpectedly reaches for them) ---

// scopedCall records one TokenForRepoScoped invocation so a test can assert
// which repos the brain minted for (and that it never minted outside the
// authorized set).
type scopedCall struct {
	owner, repo string
	perms       map[string]string
}

// reposScopedCall records one TokenForReposScoped mint — the gh-channel's
// single team-set-scoped token over a list of repos under one owner.
type reposScopedCall struct {
	owner string
	repos []string
	perms map[string]string
}

// fakeScopedResolver stands in for the production ghclient.Resolver +
// ScopedResolver: it returns a canned token from every scoped mint and
// records the calls, so a test can prove the brain-side provisioner mints
// one repo-scoped installation token per authorized repo (App) — or falls
// through to the PAT — without a real GitHub App, secret store, or key
// material.
type fakeScopedResolver struct {
	ghclient.Resolver
	base       string
	name       string
	email      string
	hasCred    bool
	token      githubapp.Token
	errFor     map[string]error // keyed by "owner/repo" (single) or "owner" (repos-scoped); returned instead of token
	calls      []scopedCall
	reposCalls []reposScopedCall
}

func (f *fakeScopedResolver) BaseURLFor(context.Context, string) (string, error) {
	return f.base, nil
}

func (f *fakeScopedResolver) OrgIdentityFor(context.Context, string) (string, string, bool) {
	return f.name, f.email, f.name != ""
}

func (f *fakeScopedResolver) HasAnyCredential(context.Context, string) (bool, error) {
	return f.hasCred, nil
}

func (f *fakeScopedResolver) TokenForRepoScoped(_ context.Context, _, owner, repo string, perms map[string]string) (githubapp.Token, error) {
	f.calls = append(f.calls, scopedCall{owner: owner, repo: repo, perms: perms})
	if err := f.errFor[owner+"/"+repo]; err != nil {
		return githubapp.Token{}, err
	}
	return f.token, nil
}

func (f *fakeScopedResolver) TokenForReposScoped(_ context.Context, _, owner string, repos []string, perms map[string]string) (githubapp.Token, error) {
	f.reposCalls = append(f.reposCalls, reposScopedCall{owner: owner, repos: repos, perms: perms})
	if err := f.errFor[owner]; err != nil {
		return githubapp.Token{}, err
	}
	return f.token, nil
}

type fakeTeamRepos struct {
	db.TeamGitHubReposStore
	tracked map[string]bool // key: lowercased "owner/repo"
}

func (f *fakeTeamRepos) TracksRepoSystem(_ context.Context, _, owner, repo string) (bool, error) {
	return f.tracked[strings.ToLower(owner+"/"+repo)], nil
}

type fakeTasks struct {
	db.TaskStore
	task *domain.Task
}

func (f *fakeTasks) GetSystem(context.Context, string, string) (*domain.Task, error) {
	return f.task, nil
}

type fakeConversationWorktrees struct {
	db.ConversationWorktreeStore
	rows []domain.ConversationWorktree
}

func (f *fakeConversationWorktrees) ListSystem(context.Context, string, string) ([]domain.ConversationWorktree, error) {
	return f.rows, nil
}

// TestManager_resolveGitHub_MintsScopedTokensForAuthorizedRepos is the
// brain-side half of "the App private key lives on control; executors hold
// only minted, hour-lived, scoped tokens": the provisioner mints one
// repo-scoped installation token per repo in the run's authorized set (the
// task's own repo plus its conversation_worktrees, intersected with what the team
// tracks) and seals nothing outside it. An untracked worktree repo is never
// minted for — minting there would be pointless (the git proxy would 403 it
// anyway) and widens the bundle for no reason.
func TestManager_resolveGitHub_MintsScopedTokensForAuthorizedRepos(t *testing.T) {
	// Hour-lived, like a real minted installation token — future-dated (and
	// truncated to GitHub's second granularity) so the fixture reads as a live
	// token and stays robust if an expiry check is ever added downstream.
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	res := &fakeScopedResolver{
		base:    "https://ghe.example",
		name:    "acme[bot]",
		email:   "acme[bot]@users.noreply.github.com",
		hasCred: true,
		token:   githubapp.Token{Value: "ghs_scoped", ExpiresAt: exp},
	}
	m := &Manager{
		stores: db.Stores{
			TeamGitHubRepos: &fakeTeamRepos{tracked: map[string]bool{"acme/widgets": true}},
			Tasks:           &fakeTasks{task: &domain.Task{EntitySource: "github", EntitySourceID: "acme/widgets#42"}},
			// A repo the run touched but the team does NOT track — must be
			// filtered out of the mint set.
			ConversationWorktrees: &fakeConversationWorktrees{rows: []domain.ConversationWorktree{{RepoID: "acme/secret"}}},
		},
		ghResolver: res,
	}

	gh, err := m.resolveGitHub(context.Background(), "org-1", "team-1", "task-1", "run-1")
	if err != nil {
		t.Fatalf("resolveGitHub: %v", err)
	}
	if gh == nil {
		t.Fatal("resolveGitHub returned nil for an org with a usable App credential")
	}
	if gh.Mode != "app" {
		t.Errorf("Mode = %q, want %q", gh.Mode, "app")
	}
	rt, ok := gh.RepoTokens["acme/widgets"]
	if !ok {
		t.Fatalf("RepoTokens missing the authorized repo; got %v", gh.RepoTokens)
	}
	if rt.Token != "ghs_scoped" || !rt.ExpiresAt.Equal(exp) {
		t.Errorf("RepoTokens[acme/widgets] = (%q, %v), want (ghs_scoped, %v)", rt.Token, rt.ExpiresAt, exp)
	}
	if _, minted := gh.RepoTokens["acme/secret"]; minted {
		t.Error("minted a token for an untracked repo — the authorized set must gate minting")
	}
	if gh.BaseURL != "https://ghe.example" {
		t.Errorf("BaseURL = %q, want the resolver's base", gh.BaseURL)
	}
	if gh.IdentityName != "acme[bot]" {
		t.Errorf("IdentityName = %q, want the resolver's org identity", gh.IdentityName)
	}
	if len(res.calls) != 1 {
		t.Fatalf("TokenForRepoScoped called %d times, want exactly 1 (only the authorized repo)", len(res.calls))
	}
	if c := res.calls[0]; c.owner != "acme" || c.repo != "widgets" {
		t.Errorf("minted for %s/%s, want acme/widgets", c.owner, c.repo)
	}

	// The gh-channel single team-set token is minted for the primary owner
	// (acme, from the task repo) over its authorized repo names.
	if gh.CLIToken == nil {
		t.Fatal("CLIToken is nil, want the gh-channel team-set token")
	}
	if gh.CLIToken.Token != "ghs_scoped" || !gh.CLIToken.ExpiresAt.Equal(exp) {
		t.Errorf("CLIToken = (%q, %v), want (ghs_scoped, %v)", gh.CLIToken.Token, gh.CLIToken.ExpiresAt, exp)
	}
	if len(res.reposCalls) != 1 {
		t.Fatalf("TokenForReposScoped called %d times, want exactly 1", len(res.reposCalls))
	}
	if c := res.reposCalls[0]; c.owner != "acme" || len(c.repos) != 1 || c.repos[0] != "widgets" {
		t.Errorf("gh-channel mint = owner %q repos %v, want acme [widgets]", c.owner, c.repos)
	}
}

// TestCLIChannelScope pins the primary-owner selection for the gh-channel's
// single team-set token: the task's primary owner wins when present, else the
// owner with the most authorized repos (alphabetically-first breaks a tie).
func TestCLIChannelScope(t *testing.T) {
	cases := []struct {
		name      string
		primary   string
		repos     []string
		wantOwner string
		wantRepos []string
	}{
		{"primary owner wins", "acme/widgets", []string{"acme/widgets", "acme/gadgets", "other/thing"}, "acme", []string{"widgets", "gadgets"}},
		{"no primary, sole owner", "", []string{"acme/a", "acme/b"}, "acme", []string{"a", "b"}},
		{"no primary, most repos wins", "", []string{"beta/x", "acme/a", "acme/b"}, "acme", []string{"a", "b"}},
		{"no primary, tie broken alphabetically", "", []string{"beta/x", "acme/a"}, "acme", []string{"a"}},
		{"primary owner not in set falls back", "ghost/repo", []string{"acme/a", "acme/b"}, "acme", []string{"a", "b"}},
		{"empty set", "acme/widgets", nil, "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner, repos := cliChannelScope(tc.primary, tc.repos)
			if owner != tc.wantOwner {
				t.Errorf("owner = %q, want %q", owner, tc.wantOwner)
			}
			sort.Strings(repos)
			want := append([]string(nil), tc.wantRepos...)
			sort.Strings(want)
			if strings.Join(repos, ",") != strings.Join(want, ",") {
				t.Errorf("repos = %v, want %v", repos, tc.wantRepos)
			}
		})
	}
}

// TestManager_resolveGitHub_PATFallback pins the no-App path: an org without
// a GitHub App borrows its PAT (a zero-expiry token that TokenForRepoScoped
// returns unchanged, since a PAT cannot be narrowed), and the bundle records
// mode "pat" so the executor treats it as an unscoped, all-repos credential.
func TestManager_resolveGitHub_PATFallback(t *testing.T) {
	res := &fakeScopedResolver{
		base:    "https://github.com",
		hasCred: true,
		token:   githubapp.Token{Value: "ghp_borrowed"}, // zero ExpiresAt == PAT
	}
	m := &Manager{
		stores: db.Stores{
			TeamGitHubRepos:       &fakeTeamRepos{tracked: map[string]bool{"acme/widgets": true}},
			Tasks:                 &fakeTasks{task: &domain.Task{EntitySource: "github", EntitySourceID: "acme/widgets#42"}},
			ConversationWorktrees: &fakeConversationWorktrees{},
		},
		ghResolver: res,
	}

	gh, err := m.resolveGitHub(context.Background(), "org-1", "team-1", "task-1", "run-1")
	if err != nil {
		t.Fatalf("resolveGitHub: %v", err)
	}
	if gh == nil {
		t.Fatal("resolveGitHub returned nil for a PAT-only org")
	}
	if gh.Mode != "pat" {
		t.Errorf("Mode = %q, want %q (a zero-expiry token is a PAT)", gh.Mode, "pat")
	}
	if gh.PAT != "ghp_borrowed" {
		t.Errorf("PAT = %q, want ghp_borrowed", gh.PAT)
	}
}

// TestManager_resolveGitHub_SkipsUnmintableRepoInAuthorizedSet pins the
// per-repo fall-through: when the authorized set spans repos under different
// owners and only some have a usable App installation, the provisioner mints
// for the ones it can and silently skips the rest (ErrNoGitHubCredentials —
// "this credential can't serve this repo"), rather than failing the whole
// bundle. A team can legitimately track a repo whose owner has no App
// installation alongside one whose owner does; the unmintable repo just gets
// no token (the git proxy would 403 it anyway).
func TestManager_resolveGitHub_SkipsUnmintableRepoInAuthorizedSet(t *testing.T) {
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	res := &fakeScopedResolver{
		base:    "https://github.com",
		hasCred: true,
		token:   githubapp.Token{Value: "ghs_scoped", ExpiresAt: exp},
		// globex has no App installation → its scoped mint reports no credential.
		errFor: map[string]error{"globex/gadgets": ghclient.ErrNoGitHubCredentials},
	}
	m := &Manager{
		stores: db.Stores{
			TeamGitHubRepos:       &fakeTeamRepos{tracked: map[string]bool{"acme/widgets": true, "globex/gadgets": true}},
			Tasks:                 &fakeTasks{task: &domain.Task{EntitySource: "github", EntitySourceID: "acme/widgets#42"}},
			ConversationWorktrees: &fakeConversationWorktrees{rows: []domain.ConversationWorktree{{RepoID: "globex/gadgets"}}},
		},
		ghResolver: res,
	}

	gh, err := m.resolveGitHub(context.Background(), "org-1", "team-1", "task-1", "run-1")
	if err != nil {
		t.Fatalf("resolveGitHub: %v (an unmintable repo in the set must be skipped, not fatal)", err)
	}
	if gh == nil {
		t.Fatal("resolveGitHub returned nil despite one mintable repo in the authorized set")
	}
	if _, ok := gh.RepoTokens["acme/widgets"]; !ok {
		t.Errorf("RepoTokens missing the mintable repo acme/widgets; got %v", gh.RepoTokens)
	}
	if _, ok := gh.RepoTokens["globex/gadgets"]; ok {
		t.Error("RepoTokens contains globex/gadgets, but its mint returned ErrNoGitHubCredentials and must be skipped")
	}
	if len(gh.RepoTokens) != 1 {
		t.Errorf("RepoTokens has %d entries, want exactly 1 (only the mintable repo)", len(gh.RepoTokens))
	}
	// Both repos were attempted — the skip is a per-repo mint result, not a
	// pre-filter over the set.
	if len(res.calls) != 2 {
		t.Errorf("TokenForRepoScoped called %d times, want 2 (both authorized repos attempted)", len(res.calls))
	}
}

// TestManager_resolveGitHub_HardMintErrorFailsBundle pins the other arm of the
// per-repo mint: an error that is NOT ErrNoGitHubCredentials (a real backend
// failure — a mint 5xx, a transient outage) is fatal for the whole bundle,
// never silently skipped. Sealing a partial bundle missing a repo the run
// needs — because of a transient failure a retry would fix — would strand that
// repo's git ops behind a confusing in-sandbox error; failing the provision
// lets the sweep re-provision cleanly once the backend recovers.
func TestManager_resolveGitHub_HardMintErrorFailsBundle(t *testing.T) {
	boom := errors.New("github: 503 while minting")
	res := &fakeScopedResolver{
		base:    "https://github.com",
		hasCred: true,
		token:   githubapp.Token{Value: "ghs_scoped"},
		errFor:  map[string]error{"acme/widgets": boom},
	}
	m := &Manager{
		stores: db.Stores{
			TeamGitHubRepos:       &fakeTeamRepos{tracked: map[string]bool{"acme/widgets": true}},
			Tasks:                 &fakeTasks{task: &domain.Task{EntitySource: "github", EntitySourceID: "acme/widgets#42"}},
			ConversationWorktrees: &fakeConversationWorktrees{},
		},
		ghResolver: res,
	}

	gh, err := m.resolveGitHub(context.Background(), "org-1", "team-1", "task-1", "run-1")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the backend mint failure (a hard error must fail the bundle, not skip the repo)", err)
	}
	if gh != nil {
		t.Errorf("resolveGitHub returned a non-nil bundle (%v) alongside a hard mint error; must return nil", gh)
	}
}

// TestManager_resolveGitHub_NoCredentialIsNotAnError pins the Jira-only org
// case: no usable GitHub credential yields a nil GitHubCreds (its runs do no
// git), never an error that would strand the whole bundle.
func TestManager_resolveGitHub_NoCredentialIsNotAnError(t *testing.T) {
	res := &fakeScopedResolver{hasCred: false}
	m := &Manager{stores: db.Stores{}, ghResolver: res}

	gh, err := m.resolveGitHub(context.Background(), "org-1", "team-1", "task-1", "run-1")
	if err != nil {
		t.Fatalf("resolveGitHub: %v", err)
	}
	if gh != nil {
		t.Errorf("resolveGitHub = %v, want nil for an org with no GitHub credential", gh)
	}
	if len(res.calls) != 0 {
		t.Errorf("TokenForRepoScoped called %d times, want 0 (nothing to mint without a credential)", len(res.calls))
	}
}

// TestManager_resolveGitHub_GHChannelMintFailureIsNonFatal pins the deliberate
// asymmetry against the per-repo loop: the gh-channel token is ADDITIVE, so a
// mint failure must degrade the gh channel, never abort provisioning. The run
// still gets its per-repo RepoTokens, which is what the exec-verb channel and
// the git proxy actually consume. Hard-failing here would turn "gh unavailable"
// into "run cannot start" — and this failure is reachable in normal operation,
// not just on a blip: a "selected repositories" App install 422s a mint naming a
// repo outside its grant.
func TestManager_resolveGitHub_GHChannelMintFailureIsNonFatal(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"transient backend error", errors.New("github: 503 while minting")},
		{"422 repo outside the installation grant", errors.New("github: mint installation token: status 422")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := &fakeScopedResolver{
				base:    "https://github.com",
				hasCred: true,
				token:   githubapp.Token{Value: "ghs_scoped", ExpiresAt: time.Now().Add(time.Hour)},
				// Keyed by OWNER — only the gh-channel (repos-scoped) mint fails;
				// the per-repo mints, keyed "owner/repo", still succeed.
				errFor: map[string]error{"acme": tc.err},
			}
			m := &Manager{
				stores: db.Stores{
					TeamGitHubRepos:       &fakeTeamRepos{tracked: map[string]bool{"acme/widgets": true}},
					Tasks:                 &fakeTasks{task: &domain.Task{EntitySource: "github", EntitySourceID: "acme/widgets#42"}},
					ConversationWorktrees: &fakeConversationWorktrees{},
				},
				ghResolver: res,
			}

			gh, err := m.resolveGitHub(context.Background(), "org-1", "team-1", "task-1", "run-1")
			if err != nil {
				t.Fatalf("resolveGitHub failed on a gh-channel mint error: %v (it must be non-fatal)", err)
			}
			if gh == nil {
				t.Fatal("resolveGitHub returned nil; the run must still get its per-repo credentials")
			}
			if gh.CLIToken != nil {
				t.Errorf("CLIToken = %+v, want nil when the gh-channel mint failed", gh.CLIToken)
			}
			// The load-bearing part: the channels that don't depend on the gh
			// token are untouched, so the run is fully operable.
			if _, ok := gh.RepoTokens["acme/widgets"]; !ok {
				t.Errorf("RepoTokens lost the authorized repo; got %v", gh.RepoTokens)
			}
		})
	}
}
