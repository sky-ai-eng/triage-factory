package gh

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

func TestParseGitRemoteURL(t *testing.T) {
	cases := []struct {
		name      string
		url       string
		wantOwner string
		wantRepo  string
		wantOK    bool
	}{
		// HTTPS
		{"https with .git", "https://github.com/sky-ai-eng/triage-factory.git", "sky-ai-eng", "triage-factory", true},
		{"https without .git", "https://github.com/sky-ai-eng/triage-factory", "sky-ai-eng", "triage-factory", true},
		{"https with trailing slash stripped", "https://github.com/octo/repo.git", "octo", "repo", true},
		{"http enterprise host", "http://github.example.com/team/proj.git", "team", "proj", true},

		// SCP-style SSH (git@host:path)
		{"scp ssh with .git", "git@github.com:sky-ai-eng/triage-factory.git", "sky-ai-eng", "triage-factory", true},
		{"scp ssh without .git", "git@github.com:octo/repo", "octo", "repo", true},
		{"scp ssh ghe host", "git@github.example.com:team/proj.git", "team", "proj", true},

		// URL-style SSH
		{"ssh:// with .git", "ssh://git@github.com/sky-ai-eng/triage-factory.git", "sky-ai-eng", "triage-factory", true},
		{"ssh:// without .git", "ssh://git@github.com/octo/repo", "octo", "repo", true},

		// git://
		{"git:// with .git", "git://github.com/octo/repo.git", "octo", "repo", true},

		// Tolerated edge cases
		{"trailing slash", "https://github.com/octo/repo/", "octo", "repo", true},
		{"trailing slash after .git", "https://github.com/octo/repo.git/", "octo", "repo", true},
		{"scp with trailing slash", "git@github.com:octo/repo.git/", "octo", "repo", true},

		// Multi-segment path rejection (regression guard for silent
		// mis-resolution on Bitbucket / nested GitLab / custom layouts).
		// These are rejected rather than guessed because "first two" and
		// "last two" are both wrong in different environments, and
		// silent wrong-target is worse than a hard error that prompts
		// --repo.
		{"bitbucket scm layout rejected", "https://bitbucket.example.com/scm/project/repo.git", "", "", false},
		{"gitlab nested groups rejected", "https://gitlab.com/group/subgroup/repo.git", "", "", false},
		{"deep gitlab nesting rejected", "https://gitlab.com/a/b/c/d/repo.git", "", "", false},
		{"scp bitbucket layout rejected", "git@bitbucket.org:scm/project/repo.git", "", "", false},

		// Failures
		{"empty string", "", "", "", false},
		{"no path", "https://github.com", "", "", false},
		{"only owner", "https://github.com/octo", "", "", false},
		{"scp no colon", "git@github.com", "", "", false},
		{"unknown scheme", "ftp://github.com/octo/repo", "", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotOwner, gotRepo, gotOK := parseGitRemoteURL(tc.url)
			if gotOK != tc.wantOK {
				t.Fatalf("ok = %v, want %v (owner=%q repo=%q)", gotOK, tc.wantOK, gotOwner, gotRepo)
			}
			if tc.wantOK {
				if gotOwner != tc.wantOwner || gotRepo != tc.wantRepo {
					t.Errorf("got (%q, %q), want (%q, %q)", gotOwner, gotRepo, tc.wantOwner, tc.wantRepo)
				}
			}
		})
	}
}

func TestSplitOwnerRepoStr(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		wantErr bool
		owner   string
		repo    string
	}{
		{"valid", "sky-ai-eng/triage-factory", false, "sky-ai-eng", "triage-factory"},
		{"valid with dashes", "my-org/my-repo", false, "my-org", "my-repo"},
		{"empty", "", true, "", ""},
		{"no slash", "owner", true, "", ""},
		{"trailing slash", "owner/", true, "", ""},
		{"leading slash", "/repo", true, "", ""},
		{"only slash", "/", true, "", ""},
		// Path-traversal guard: extra segments / .. must be rejected so
		// owner/repo can't escape the _tfac dir when used in a path.
		{"extra segment", "owner/repo/extra", true, "", ""},
		{"dotdot repo", "owner/../../../etc", true, "", ""},
		{"dotdot owner", "../owner/repo", true, "", ""},
		{"backslash repo", "owner/re\\po", true, "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner, repo, err := splitOwnerRepoStr(tc.value, "test source")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if !tc.wantErr {
				if owner != tc.owner || repo != tc.repo {
					t.Errorf("got (%q, %q), want (%q, %q)", owner, repo, tc.owner, tc.repo)
				}
			}
		})
	}
}

// fakeCheckouts is a runCheckouts that hands back canned
// conversation_worktrees rows (host view) and a root pair to translate them.
type fakeCheckouts struct {
	rows                []domain.ConversationWorktree
	hostRoot, agentRoot string
	listErr             error
}

func (f fakeCheckouts) ListConversationWorktrees(context.Context) ([]domain.ConversationWorktree, error) {
	return f.rows, f.listErr
}

func (f fakeCheckouts) WorkspaceRoots(context.Context) (string, string, error) {
	return f.hostRoot, f.agentRoot, nil
}

// isolateGit points HOME and XDG_CONFIG_HOME at an empty directory so the
// developer's global git config (a templateDir that pre-populates remotes, say)
// can't influence what `git config --get remote.origin.url` answers.
func isolateGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(fakeHome, ".config"))
}

// gitCheckoutWithOrigin makes dir a git checkout whose origin is originURL.
// Neither step needs a user identity (unlike a commit).
func gitCheckoutWithOrigin(t *testing.T, dir, originURL string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	for _, argv := range [][]string{
		{"git", "init", "-q"},
		{"git", "remote", "add", "origin", originURL},
	} {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\noutput: %s", argv, err, out)
		}
	}
}

// TestResolveRepo_FlagWins verifies the explicit flag beats the checkout the
// current directory sits in.
func TestResolveRepo_FlagWins(t *testing.T) {
	isolateGit(t)
	dir := t.TempDir()
	gitCheckoutWithOrigin(t, dir, "https://github.com/checkout-owner/checkout-repo.git")
	t.Chdir(dir)

	owner, repo, err := resolveRepo(context.Background(), nil, []string{"--repo", "flag-owner/flag-repo"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if owner != "flag-owner" || repo != "flag-repo" {
		t.Errorf("got (%q, %q), want (flag-owner, flag-repo)", owner, repo)
	}
}

// TestResolveRepo_HardErrorWhenNothingResolves runs from a temp directory
// outside any checkout with no run context, so every resolution path fails and
// the resolver returns an error naming both ways out.
func TestResolveRepo_HardErrorWhenNothingResolves(t *testing.T) {
	isolateGit(t)
	t.Chdir(t.TempDir())

	_, _, err := resolveRepo(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("expected error when no resolution path succeeds, got nil")
	}
	if !strings.Contains(err.Error(), "--repo") {
		t.Errorf("error should point at --repo: %v", err)
	}
}

// TestResolveRepo_InvalidFlagFormat — a malformed --repo value should
// error, not fall through to the checkout.
func TestResolveRepo_InvalidFlagFormat(t *testing.T) {
	isolateGit(t)
	dir := t.TempDir()
	gitCheckoutWithOrigin(t, dir, "https://github.com/checkout-owner/checkout-repo.git")
	t.Chdir(dir)

	_, _, err := resolveRepo(context.Background(), nil, []string{"--repo", "not-a-valid-format"})
	if err == nil {
		t.Fatal("expected error on invalid flag value, got nil")
	}
}

// TestResolveRepo_EmptyFlagValue is the regression guard for the
// "--repo without a value" case. flagVal returns "" both when --repo
// isn't present AND when --repo is the last token in args (no value to
// consume). resolveRepo disambiguates via hasFlag so an explicit --repo
// with no value fails instead of silently resolving the checkout's repo.
func TestResolveRepo_EmptyFlagValue(t *testing.T) {
	// Stand in a checkout so there IS a fallback available — the test is
	// that we error instead of quietly using it.
	isolateGit(t)
	dir := t.TempDir()
	gitCheckoutWithOrigin(t, dir, "https://github.com/checkout-owner/checkout-repo.git")
	t.Chdir(dir)

	cases := [][]string{
		{"--repo"},                      // last arg, no value
		{"--repo", "--some-other-flag"}, // value looks like another flag — ambiguous, but user clearly forgot to supply one
		{"pos-arg", "--repo"},           // --repo at the end after positional
	}

	for _, args := range cases {
		t.Run("", func(t *testing.T) {
			// The "--some-other-flag" case is a known soft spot: flagVal
			// returns "--some-other-flag" as the value, and splitOwnerRepoStr
			// rejects it as malformed. Either way it errors, which is the
			// behavior we want.
			_, _, err := resolveRepo(context.Background(), nil, args)
			if err == nil {
				t.Errorf("args %v: expected error on empty/invalid --repo, got nil", args)
			}
		})
	}
}

// TestResolveRepo_GitConfigFallback exercises the checkout path: read
// remote.origin.url via `git config --get`, from the checkout root and from a
// folder nested inside it. Uses real git commands rather than hand-crafting the
// git config format, because the config path goes through `git config` at
// runtime and a synthetic fixture would miss parser quirks.
func TestResolveRepo_GitConfigFallback(t *testing.T) {
	isolateGit(t)
	dir := t.TempDir()
	gitCheckoutWithOrigin(t, dir, "https://github.com/test-owner/test-repo.git")
	nested := filepath.Join(dir, "pkg", "sub")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, wd := range []string{dir, nested} {
		t.Run(filepath.Base(wd), func(t *testing.T) {
			t.Chdir(wd)
			owner, repo, err := resolveRepo(context.Background(), nil, nil)
			if err != nil {
				t.Fatalf("resolveRepo via git config: %v", err)
			}
			if owner != "test-owner" || repo != "test-repo" {
				t.Errorf("got (%q, %q), want (test-owner, test-repo)", owner, repo)
			}
		})
	}
}

// TestResolveRepo_TwoCheckouts pins the multi-repo run: with checkouts of two
// different repos, a verb run from inside either one targets that one, and
// from the run root — inside neither — it fails with an error that lists both
// checkouts by repo and by a path this process can cd into.
func TestResolveRepo_TwoCheckouts(t *testing.T) {
	isolateGit(t)
	root := t.TempDir()
	pathA := filepath.Join(root, "owner-a", "repo-a", "pr-7")
	pathB := filepath.Join(root, "owner-b", "repo-b", "default")
	gitCheckoutWithOrigin(t, pathA, "https://github.com/owner-a/repo-a.git")
	gitCheckoutWithOrigin(t, pathB, "git@github.com:owner-b/repo-b.git")

	// Rows are recorded in host view; the agent sees the same tree at root.
	const hostRoot = "/host/runs/task-1"
	checkouts := fakeCheckouts{
		hostRoot:  hostRoot,
		agentRoot: root,
		rows: []domain.ConversationWorktree{
			{RepoID: "owner-a/repo-a", Ref: "pr-7", Path: hostRoot + "/owner-a/repo-a/pr-7"},
			{RepoID: "owner-b/repo-b", Ref: "default", Path: hostRoot + "/owner-b/repo-b/default"},
		},
	}

	for _, tc := range []struct{ wd, owner, repo string }{
		{pathA, "owner-a", "repo-a"},
		{pathB, "owner-b", "repo-b"},
	} {
		t.Chdir(tc.wd)
		owner, repo, err := resolveRepo(context.Background(), checkouts, nil)
		if err != nil {
			t.Fatalf("from %s: %v", tc.wd, err)
		}
		if owner != tc.owner || repo != tc.repo {
			t.Errorf("from %s: got %s/%s, want %s/%s", tc.wd, owner, repo, tc.owner, tc.repo)
		}
	}

	t.Chdir(root)
	_, _, err := resolveRepo(context.Background(), checkouts, nil)
	if err == nil {
		t.Fatal("from the run root: expected an error, got nil")
	}
	for _, want := range []string{
		"owner-a/repo-a  " + pathA,
		"owner-b/repo-b  " + pathB,
		"--repo",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %q:\n%v", want, err)
		}
	}
}

// TestResolveRepo_NoCheckoutsYet covers a run that has materialized nothing:
// the error says so and names how to get one, rather than listing nothing.
func TestResolveRepo_NoCheckoutsYet(t *testing.T) {
	isolateGit(t)
	t.Chdir(t.TempDir())

	_, _, err := resolveRepo(context.Background(), fakeCheckouts{}, nil)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "no checkouts yet") || !strings.Contains(err.Error(), "workspace add") {
		t.Errorf("error should say the run has no checkouts and name workspace add: %v", err)
	}
}
