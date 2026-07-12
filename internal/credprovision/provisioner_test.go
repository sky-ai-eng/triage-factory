package credprovision

import (
	"context"
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

// fakeScopedResolver stands in for the production ghclient.Resolver +
// ScopedResolver: it returns a canned token from every scoped mint and
// records the calls, so a test can prove the brain-side provisioner mints
// one repo-scoped installation token per authorized repo (App) — or falls
// through to the PAT — without a real GitHub App, secret store, or key
// material.
type fakeScopedResolver struct {
	ghclient.Resolver
	base    string
	name    string
	email   string
	hasCred bool
	token   githubapp.Token
	calls   []scopedCall
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

type fakeRunWorktrees struct {
	db.RunWorktreeStore
	rows []domain.RunWorktree
}

func (f *fakeRunWorktrees) ListSystem(context.Context, string, string) ([]domain.RunWorktree, error) {
	return f.rows, nil
}

// TestManager_resolveGitHub_MintsScopedTokensForAuthorizedRepos is the
// brain-side half of "the App private key lives on control; executors hold
// only minted, hour-lived, scoped tokens": the provisioner mints one
// repo-scoped installation token per repo in the run's authorized set (the
// task's own repo plus its run_worktrees, intersected with what the team
// tracks) and seals nothing outside it. An untracked worktree repo is never
// minted for — minting there would be pointless (the git proxy would 403 it
// anyway) and widens the bundle for no reason.
func TestManager_resolveGitHub_MintsScopedTokensForAuthorizedRepos(t *testing.T) {
	exp := time.Unix(1_700_000_000, 0).UTC()
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
			RunWorktrees: &fakeRunWorktrees{rows: []domain.RunWorktree{{RepoID: "acme/secret"}}},
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
			TeamGitHubRepos: &fakeTeamRepos{tracked: map[string]bool{"acme/widgets": true}},
			Tasks:           &fakeTasks{task: &domain.Task{EntitySource: "github", EntitySourceID: "acme/widgets#42"}},
			RunWorktrees:    &fakeRunWorktrees{},
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
