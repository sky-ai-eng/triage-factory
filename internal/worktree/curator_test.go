package worktree

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnsureCuratorWorktree_FreshMaterializesAtExpectedPath pins the
// layout contract: <projectDir>/repos/<owner>/<repo>/, checked out at
// the requested branch. The frontend will encode the same path
// when telling the agent where to look for source.
func TestEnsureCuratorWorktree_FreshMaterializesAtExpectedPath(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)

	if _, err := EnsureBareClone(context.Background(), "owner", "repoA", upstream); err != nil {
		t.Fatalf("seed bare: %v", err)
	}

	projectDir := filepath.Join(t.TempDir(), "proj")
	wt, err := EnsureCuratorWorktree(context.Background(), "owner", "repoA", "main", projectDir)
	if err != nil {
		t.Fatalf("EnsureCuratorWorktree: %v", err)
	}
	want := filepath.Join(projectDir, "repos", "owner", "repoA")
	if wt != want {
		t.Errorf("worktree path = %q, want %q", wt, want)
	}
	if _, err := os.Stat(filepath.Join(wt, ".git")); err != nil {
		t.Errorf("expected .git in worktree, got %v", err)
	}
}

// TestEnsureCuratorWorktree_RefreshIsIdempotent pins the every-dispatch
// contract: a second call with the same args refreshes (fetches +
// resets hard) and returns the same path. The third call is a tighter
// idempotence check that no state from the second leaks.
func TestEnsureCuratorWorktree_RefreshIsIdempotent(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)
	if _, err := EnsureBareClone(context.Background(), "o", "r", upstream); err != nil {
		t.Fatalf("seed bare: %v", err)
	}
	projectDir := filepath.Join(t.TempDir(), "proj")

	for i := 0; i < 3; i++ {
		if _, err := EnsureCuratorWorktree(context.Background(), "o", "r", "main", projectDir); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
	}
}

// TestEnsureCuratorWorktree_RefreshResetsAgentEdits is the load-bearing
// "reset --hard every dispatch" test: after the agent edits a tracked
// file in its working tree, the next dispatch's call to
// EnsureCuratorWorktree must blow that edit away. The Curator's
// contract is "current upstream state," not "agent's WIP."
func TestEnsureCuratorWorktree_RefreshResetsAgentEdits(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)
	if _, err := EnsureBareClone(context.Background(), "o", "r", upstream); err != nil {
		t.Fatalf("seed bare: %v", err)
	}
	projectDir := filepath.Join(t.TempDir(), "proj")
	wt, err := EnsureCuratorWorktree(context.Background(), "o", "r", "main", projectDir)
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	// makeTestUpstream's seed commit is empty — write a tracked file
	// to the worktree, commit and push it locally to mimic the
	// upstream having a real file, then have the "agent" edit it.
	tracked := filepath.Join(wt, "README.md")
	if err := os.WriteFile(tracked, []byte("upstream content\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	cmds := [][]string{
		{"-C", wt, "config", "user.email", "test@example.com"},
		{"-C", wt, "config", "user.name", "Test"},
		{"-C", wt, "add", "README.md"},
		{"-C", wt, "commit", "-m", "add readme"},
		{"-C", wt, "push", "origin", "main"},
	}
	for _, c := range cmds {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", c, err, out)
		}
	}

	// Simulate the agent dirtying the worktree.
	if err := os.WriteFile(tracked, []byte("AGENT EDIT\n"), 0o644); err != nil {
		t.Fatalf("agent edit: %v", err)
	}

	// Untracked scratch files the agent left behind.
	scratch := filepath.Join(wt, "scratch.txt")
	if err := os.WriteFile(scratch, []byte("debugging output\n"), 0o644); err != nil {
		t.Fatalf("scratch write: %v", err)
	}

	// Next dispatch — refresh.
	if _, err := EnsureCuratorWorktree(context.Background(), "o", "r", "main", projectDir); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	// Tracked file restored to upstream content.
	got, err := os.ReadFile(tracked)
	if err != nil {
		t.Fatalf("read after refresh: %v", err)
	}
	if string(got) != "upstream content\n" {
		t.Errorf("tracked file content = %q, want upstream content; reset --hard failed to discard agent edit", got)
	}

	// Untracked file removed by `clean -fdx`.
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("untracked scratch file survived refresh: stat err = %v", err)
	}
}

// TestCreateForBranchInRoot_CoexistsWithCuratorWorktree is the
// regression test for the "fatal: refusing to fetch into branch
// 'refs/heads/main' checked out at <curator path>" bug. The Curator
// materializes a per-project worktree with `main` checked out as a
// real local branch; a subsequent CreateForBranchInRoot call against
// the same bare must NOT collide with that checkout.
//
// Pre-fix: createBranchWorktreeAt fetched with refspec
// `+refs/heads/main:refs/heads/main`, which forces git to update the
// checked-out local ref and is refused. Now it fetches into
// refs/remotes/origin/main and branches off that ref. This test
// exercises the exact two-step sequence (curator first, workspace
// add second) the user hit in the wild.
func TestCreateForBranchInRoot_CoexistsWithCuratorWorktree(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)

	if _, err := EnsureBareClone(context.Background(), "owner", "repoX", upstream); err != nil {
		t.Fatalf("seed bare: %v", err)
	}

	// Step 1: Curator materializes its per-project worktree with
	// main checked out as a local branch. After this, the bare's
	// refs/heads/main is "checked out" at the curator's path.
	projectDir := filepath.Join(t.TempDir(), "proj")
	if _, err := EnsureCuratorWorktree(context.Background(), "owner", "repoX", "main", projectDir); err != nil {
		t.Fatalf("curator setup: %v", err)
	}

	// Step 2: A delegated Jira agent runs `workspace add owner/repoX`,
	// which invokes CreateForBranchInRoot against the same bare. With
	// the old refspec this returned a fetch error; with the new one
	// (refs/remotes/origin/main) it succeeds.
	runRoot := filepath.Join(t.TempDir(), "run-1")
	if err := os.MkdirAll(runRoot, 0o755); err != nil {
		t.Fatalf("mkdir runRoot: %v", err)
	}
	wtPath, err := CreateForBranchInRoot(
		context.Background(),
		"owner", "repoX",
		upstream, "main", "feature/SKY-100",
		"run-1", runRoot,
	)
	if err != nil {
		t.Fatalf("CreateForBranchInRoot collided with curator's main checkout: %v", err)
	}

	// Sanity: the new worktree exists and is on the feature branch.
	if _, err := os.Stat(filepath.Join(wtPath, ".git")); err != nil {
		t.Errorf("expected .git in feature worktree, got %v", err)
	}
	out, err := exec.Command("git", "-C", wtPath, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "feature/SKY-100" {
		t.Errorf("HEAD = %q, want feature/SKY-100", got)
	}
}

func TestEnsureCuratorWorktree_RejectsMissingBareWithoutURL(t *testing.T) {
	// Seed-on-demand (TFAC-60) retires the refuse-to-seed, but only when
	// the caller supplies a clone URL to seed FROM. With no URL there's
	// nothing to clone, so a missing bare still surfaces a clear error
	// rather than a confusing downstream git failure.
	withTestHome(t)
	projectDir := filepath.Join(t.TempDir(), "proj")
	_, err := EnsureCuratorWorktree(context.Background(), "ghost", "repo", "main", projectDir)
	if err == nil {
		t.Fatal("expected error for missing bare with no clone URL, got nil")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("error message should mention missing: %q", err.Error())
	}
}

// TestEnsureCuratorWorktree_SeedsMissingBareWithURL is the core TFAC-60/-62
// behavior: a curator dispatch on a pinned repo whose bare was never
// materialized (no prior delegation) seeds the bare on demand from the
// supplied clone URL and materializes the worktree — no error, no skip.
func TestEnsureCuratorWorktree_SeedsMissingBareWithURL(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)

	// No prior EnsureBareClone — the bare does not exist yet.
	if dir, _ := repoDir("seed-owner", "seed-repo"); dirExists(dir) {
		t.Fatalf("precondition: bare should not exist yet")
	}

	projectDir := filepath.Join(t.TempDir(), "proj")
	wt, err := EnsureCuratorWorktree(context.Background(), "seed-owner", "seed-repo", "main", projectDir,
		WithCloneURL(upstream))
	if err != nil {
		t.Fatalf("EnsureCuratorWorktree with seed URL: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, ".git")); err != nil {
		t.Errorf("worktree not materialized: %v", err)
	}
	// The bare itself now exists — the seed landed in the cache.
	bareDir, _ := repoDir("seed-owner", "seed-repo")
	if !dirExists(bareDir) {
		t.Errorf("bare was not seeded at %s", bareDir)
	}
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// TestEnsureCuratorWorktree_SeedCarriesAuthHeader proves the multi-mode
// seed path authenticates: when WithCloneURL + WithCloneAuth are supplied
// for a missing bare, the host-scoped Authorization header reaches the
// wire (the App-token clone). Mirrors clone_auth_test.go's TLS-capture
// pattern. The local path (no auth) is covered by the credential-free seed
// in TestEnsureCuratorWorktree_SeedsMissingBareWithURL above.
func TestEnsureCuratorWorktree_SeedCarriesAuthHeader(t *testing.T) {
	withTestHome(t)
	t.Setenv("GIT_SSL_NO_VERIFY", "true")

	base, authHeader := startRefsCaptureServer(t)
	cloneURL := base + "/acme/repo.git"

	projectDir := filepath.Join(t.TempDir(), "proj")
	// The clone fails (server serves no repo); we assert only the header
	// the seed carried, which is what authenticates a private clone.
	_, _ = EnsureCuratorWorktree(context.Background(), "acme", "curator-seed-auth", "main", projectDir,
		WithCloneURL(cloneURL), WithCloneAuth(CloneAuthFor(cloneURL, "ghs_curator_seed")))

	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:ghs_curator_seed"))
	if got := authHeader(); got != want {
		t.Errorf("seed info/refs Authorization = %q, want %q", got, want)
	}
}

func TestEnsureCuratorWorktree_RejectsEmptyArgs(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		owner, repo, branch, pdir string
	}{
		{"owner", "", "r", "main", "/tmp/p"},
		{"repo", "o", "", "main", "/tmp/p"},
		{"branch", "o", "r", "", "/tmp/p"},
		{"projectDir", "o", "r", "main", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := EnsureCuratorWorktree(context.Background(), tc.owner, tc.repo, tc.branch, tc.pdir); err == nil {
				t.Errorf("expected error for empty %s", tc.name)
			}
		})
	}
}

// TestPruneCuratorBare_NoOpOnMissingBare proves the cleanup hook is
// safe to call from the project-delete handler regardless of whether
// the bare exists.
func TestPruneCuratorBare_NoOpOnMissingBare(t *testing.T) {
	withTestHome(t)
	// No bare → log+return; should not panic or error.
	PruneCuratorBare("ghost", "repo")
}

func TestCuratorRepoSubpath_NestedLayout(t *testing.T) {
	// Pin: nested <owner>/<repo>, not flattened. Flattening with
	// a dash created a collision class (TestCuratorRepoSubpath_NoCollisions
	// covers that directly); nesting matches GitHub's URL convention
	// and makes the (owner, repo) → subpath mapping injective.
	got := CuratorRepoSubpath("sky-ai-eng", "triage-factory")
	want := filepath.Join("sky-ai-eng", "triage-factory")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestCuratorRepoSubpath_NoCollisions guards the bug an earlier flat
// "owner-repo" form had: GitHub allows hyphens in either half of a
// slug, so "a-b/c" and "a/b-c" both flatten to "a-b-c" and would
// silently share an on-disk path. The slash-separated form makes the
// mapping injective by construction — the slash can't appear inside
// either half of a real GitHub identifier.
func TestCuratorRepoSubpath_NoCollisions(t *testing.T) {
	pairs := [][2]string{
		{"a-b", "c"},
		{"a", "b-c"},
	}
	seen := make(map[string]string)
	for _, p := range pairs {
		got := CuratorRepoSubpath(p[0], p[1])
		if prev, dup := seen[got]; dup {
			t.Errorf("collision: %q produced by both %q and %q/%q", got, prev, p[0], p[1])
		}
		seen[got] = p[0] + "/" + p[1]
	}
}
