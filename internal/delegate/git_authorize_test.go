package delegate

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// stubLiveBranch installs a deterministic path→push-target mapping for the push
// gate's worktreePushTargetBranch seam and restores the real (git-backed)
// reader on cleanup. The value is the RESOLVED remote target — the live branch
// mapped through any configured push refspec (worktree.PushTargetBranch owns
// that resolution and has its own git-backed tests); with no refspec it is the
// live branch itself, which is what these table tests model. Lets the tests
// authorize against a known target without standing up real worktrees on disk.
func stubLiveBranch(t *testing.T, m map[string]string) {
	t.Helper()
	orig := worktreePushTargetBranch
	t.Cleanup(func() { worktreePushTargetBranch = orig })
	worktreePushTargetBranch = func(path string) string { return m[path] }
}

// TestGitAuthorizeDecision is the proxy gate's brain: a run may touch a repo
// only if its team tracks it AND it appears in the run's run_worktrees ledger,
// and the allowed push ref is the worktree's live checkout's PUSH TARGET —
// the current branch mapped through its configured push refspec (TFAC-498,
// refspec-aware; identity when no refspec, as stubbed here). It fails closed
// when the backing stores are absent.
func TestGitAuthorizeDecision(t *testing.T) {
	database := newDelegateTestDB(t)
	stores := sqlitestore.New(database)
	ctx := context.Background()
	// run_worktrees FKs the run, so seed it first (LocalDefaultTeamID via the task).
	seedRun(t, database, "run-1", "sess", "/tmp/wt")
	info := agenthost.RunInfo{
		OrgID:  runmode.LocalDefaultOrgID,
		TeamID: runmode.LocalDefaultTeamID,
		RunID:  "run-1",
	}

	// Track two repos for the team; materialize two — one overlapping (tracked
	// AND materialized), one only-materialized (not tracked).
	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, []domain.TeamGitHubRepo{
		{Owner: "acme", Repo: "api"},
		{Owner: "acme", Repo: "tracked-only"},
	}); err != nil {
		t.Fatalf("track repos: %v", err)
	}
	// A profile for acme/api so the protected-branch filter has a default to
	// compare against (it must not reject the agent's own feature branch).
	if err := stores.Repos.Upsert(ctx, runmode.LocalDefaultOrgID, domain.RepoProfile{
		ID: "acme/api", Owner: "acme", Repo: "api", DefaultBranch: "main", CloneURL: "https://x", ProfileText: "t",
	}); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	// Ref on the rows is informational only — the gate reads the live branch,
	// stubbed below by worktree path.
	for _, w := range []domain.RunWorktree{
		{RunID: "run-1", RepoID: "acme/api", Path: "/tmp/a", Ref: "@default"},
		{RunID: "run-1", RepoID: "acme/materialized-only", Path: "/tmp/m", Ref: "@default"},
	} {
		if _, _, err := stores.RunWorktrees.InsertSystem(ctx, runmode.LocalDefaultOrgID, w); err != nil {
			t.Fatalf("materialize %s: %v", w.RepoID, err)
		}
	}
	stubLiveBranch(t, map[string]string{
		"/tmp/a": "agent/feature-1",
		"/tmp/m": "agent/other",
	})

	cases := []struct {
		name        string
		owner, repo string
		wantAllowed bool
		wantRefs    []string
	}{
		{"tracked and materialized → allow with the live branch", "acme", "api", true, []string{"refs/heads/agent/feature-1"}},
		{"case-insensitive repo match", "Acme", "API", true, []string{"refs/heads/agent/feature-1"}},
		{"tracked but not materialized → deny", "acme", "tracked-only", false, nil},
		{"materialized but untracked → deny", "acme", "materialized-only", false, nil},
		{"neither tracked nor materialized → deny", "ghost", "repo", false, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := gitAuthorizeDecision(ctx, stores, info, c.owner, c.repo)
			if err != nil {
				t.Fatalf("gitAuthorizeDecision: %v", err)
			}
			if d.Allowed != c.wantAllowed {
				t.Errorf("Allowed = %v, want %v", d.Allowed, c.wantAllowed)
			}
			if !equalRefs(d.AllowedRefs, c.wantRefs) {
				t.Errorf("AllowedRefs = %v, want %v", d.AllowedRefs, c.wantRefs)
			}
		})
	}

	// Fail closed when the stores it needs are absent (a misconfigured gate
	// must never allow-all).
	if d, err := gitAuthorizeDecision(ctx, db.Stores{}, info, "acme", "api"); err != nil || d.Allowed {
		t.Errorf("nil-stores decision = %+v err=%v; want deny (fail closed)", d, err)
	}
}

// TestGitAuthorizeDecision_ProtectedAndDetached pins the two ways an allowed
// repo still yields no pushable ref: a checkout sitting on the repo's base /
// default branch (refused), and a detached HEAD (no live branch yet). Both keep
// Allowed=true (fetch/advertise is fine) but contribute nothing to AllowedRefs,
// so the receive-pack push is rejected per-ref.
func TestGitAuthorizeDecision_ProtectedAndDetached(t *testing.T) {
	database := newDelegateTestDB(t)
	stores := sqlitestore.New(database)
	ctx := context.Background()
	seedRun(t, database, "run-2", "sess", "/tmp/wt")
	info := agenthost.RunInfo{OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID, RunID: "run-2"}

	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, []domain.TeamGitHubRepo{
		{Owner: "acme", Repo: "api"},
	}); err != nil {
		t.Fatalf("track repo: %v", err)
	}
	if err := stores.Repos.Upsert(ctx, runmode.LocalDefaultOrgID, domain.RepoProfile{
		ID: "acme/api", Owner: "acme", Repo: "api", DefaultBranch: "main", CloneURL: "https://x", ProfileText: "t",
	}); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	// base_branch is user-configured (Upsert preserves it), so set it explicitly.
	if err := stores.Repos.UpdateBaseBranch(ctx, runmode.LocalDefaultOrgID, "acme/api", "develop"); err != nil {
		t.Fatalf("set base branch: %v", err)
	}
	if _, _, err := stores.RunWorktrees.InsertSystem(ctx, runmode.LocalDefaultOrgID, domain.RunWorktree{
		RunID: "run-2", RepoID: "acme/api", Path: "/tmp/api", Ref: "@default",
	}); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	// Sitting on the default branch → refused.
	stubLiveBranch(t, map[string]string{"/tmp/api": "main"})
	if d, err := gitAuthorizeDecision(ctx, stores, info, "acme", "api"); err != nil || !d.Allowed || len(d.AllowedRefs) != 0 {
		t.Errorf("on default branch: decision=%+v err=%v; want Allowed=true, no refs", d, err)
	}

	// Sitting on the configured base branch → refused.
	stubLiveBranch(t, map[string]string{"/tmp/api": "develop"})
	if d, err := gitAuthorizeDecision(ctx, stores, info, "acme", "api"); err != nil || !d.Allowed || len(d.AllowedRefs) != 0 {
		t.Errorf("on base branch: decision=%+v err=%v; want Allowed=true, no refs", d, err)
	}

	// Detached HEAD (fresh default checkout) → no live branch, no refs.
	stubLiveBranch(t, map[string]string{"/tmp/api": ""})
	if d, err := gitAuthorizeDecision(ctx, stores, info, "acme", "api"); err != nil || !d.Allowed || len(d.AllowedRefs) != 0 {
		t.Errorf("detached: decision=%+v err=%v; want Allowed=true, no refs", d, err)
	}

	// The agent's own branch → authorized.
	stubLiveBranch(t, map[string]string{"/tmp/api": "tfac/SKY-1"})
	if d, err := gitAuthorizeDecision(ctx, stores, info, "acme", "api"); err != nil || !d.Allowed || !equalRefs(d.AllowedRefs, []string{"refs/heads/tfac/SKY-1"}) {
		t.Errorf("on feature branch: decision=%+v err=%v; want Allowed with refs/heads/tfac/SKY-1", d, err)
	}
}

// TestGitAuthorizeDecision_PRWorktreeRefspecMapping is the regression test for
// the multi-mode PR push 403 (ref-not-allowed): against a REAL git checkout
// shaped exactly like a PR run clone — checked out on the run-namespaced
// triagefactory/<runID>/pr-<n> branch with configurePRPushTracking's per-run
// remote + explicit refspec to the PR's real head — the gate (through the real
// worktree.PushTargetBranch, no stub) must authorize the REMOTE head branch,
// which is what the receive-pack command block carries. Authorizing the local
// name instead is exactly what rejected every PR push. A refspec that maps to
// a protected branch is still refused.
func TestGitAuthorizeDecision_PRWorktreeRefspecMapping(t *testing.T) {
	database := newDelegateTestDB(t)
	stores := sqlitestore.New(database)
	ctx := context.Background()
	seedRun(t, database, "run-pr", "sess", "/tmp/wt")
	info := agenthost.RunInfo{OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID, RunID: "run-pr"}

	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, []domain.TeamGitHubRepo{
		{Owner: "acme", Repo: "api"},
	}); err != nil {
		t.Fatalf("track repo: %v", err)
	}
	if err := stores.Repos.Upsert(ctx, runmode.LocalDefaultOrgID, domain.RepoProfile{
		ID: "acme/api", Owner: "acme", Repo: "api", DefaultBranch: "main", CloneURL: "https://x", ProfileText: "t",
	}); err != nil {
		t.Fatalf("seed profile: %v", err)
	}

	// A real repo shaped like the multi-mode PR run clone.
	wt := t.TempDir()
	gitAt := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", wt}, args...)...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	gitAt("init", "-b", "triagefactory/run-pr/pr-58")
	gitAt("config", "user.email", "t@example.com")
	gitAt("config", "user.name", "T")
	gitAt("commit", "--allow-empty", "-m", "seed")
	gitAt("remote", "add", "tfpush-run-pr-58", "https://github.com/acme/api.git")
	gitAt("config", "remote.tfpush-run-pr-58.push", "refs/heads/triagefactory/run-pr/pr-58:refs/heads/aa/SKY-101")
	gitAt("config", "branch.triagefactory/run-pr/pr-58.pushRemote", "tfpush-run-pr-58")

	if _, _, err := stores.RunWorktrees.InsertSystem(ctx, runmode.LocalDefaultOrgID, domain.RunWorktree{
		RunID: "run-pr", RepoID: "acme/api", Path: wt, Ref: "pr-58",
	}); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	d, err := gitAuthorizeDecision(ctx, stores, info, "acme", "api")
	if err != nil {
		t.Fatalf("gitAuthorizeDecision: %v", err)
	}
	if !d.Allowed || !equalRefs(d.AllowedRefs, []string{"refs/heads/aa/SKY-101"}) {
		t.Errorf("decision = %+v, want Allowed with refs/heads/aa/SKY-101 (the refspec's remote head, not the local triagefactory/... name)", d)
	}

	// The same shape mapped at a protected branch must contribute no ref: the
	// protected check applies to the RESOLVED target.
	gitAt("config", "remote.tfpush-run-pr-58.push", "refs/heads/triagefactory/run-pr/pr-58:refs/heads/main")
	d, err = gitAuthorizeDecision(ctx, stores, info, "acme", "api")
	if err != nil {
		t.Fatalf("gitAuthorizeDecision (protected target): %v", err)
	}
	if !d.Allowed || len(d.AllowedRefs) != 0 {
		t.Errorf("decision with a protected-mapped refspec = %+v, want Allowed=true with no refs", d)
	}
}

// TestGitAuthorizeDecision_PRWorktreeRefspecMapping_DubiousOwnership is the
// push-gate starvation regression: identical setup to
// TestGitAuthorizeDecision_PRWorktreeRefspecMapping, except the worktree is
// chowned to a different uid before the decision is computed — reproducing
// what agentproc.chownWorktreeForSandbox leaves behind for every multi-mode
// run. Before the fix, the host-side (root) read of PushTargetBranch hit
// git's "detected dubious ownership" refusal, silently came back "", and the
// row was skipped — Allowed stayed true (fetch/advertise still fine) but
// AllowedRefs came back empty, which is exactly the observed symptom: the
// receive-pack ref gate then denies every pushed ref with "ref-not-allowed"
// even though the checkout's branch and push refspec are configured
// correctly. Skips if the chown below fails (needs root/CAP_CHOWN — euid==0
// alone doesn't guarantee it in a rootless/user-namespaced environment).
func TestGitAuthorizeDecision_PRWorktreeRefspecMapping_DubiousOwnership(t *testing.T) {
	database := newDelegateTestDB(t)
	stores := sqlitestore.New(database)
	ctx := context.Background()
	seedRun(t, database, "run-pr-own", "sess", "/tmp/wt")
	info := agenthost.RunInfo{OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID, RunID: "run-pr-own"}

	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, []domain.TeamGitHubRepo{
		{Owner: "acme", Repo: "api"},
	}); err != nil {
		t.Fatalf("track repo: %v", err)
	}
	if err := stores.Repos.Upsert(ctx, runmode.LocalDefaultOrgID, domain.RepoProfile{
		ID: "acme/api", Owner: "acme", Repo: "api", DefaultBranch: "main", CloneURL: "https://x", ProfileText: "t",
	}); err != nil {
		t.Fatalf("seed profile: %v", err)
	}

	// A real repo shaped like the multi-mode PR run clone.
	wt := t.TempDir()
	gitAt := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", wt}, args...)...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	gitAt("init", "-b", "triagefactory/run-pr-own/pr-58")
	gitAt("config", "user.email", "t@example.com")
	gitAt("config", "user.name", "T")
	gitAt("commit", "--allow-empty", "-m", "seed")
	gitAt("remote", "add", "tfpush-run-pr-own-58", "https://github.com/acme/api.git")
	gitAt("config", "remote.tfpush-run-pr-own-58.push", "refs/heads/triagefactory/run-pr-own/pr-58:refs/heads/aa/SKY-101")
	gitAt("config", "branch.triagefactory/run-pr-own/pr-58.pushRemote", "tfpush-run-pr-own-58")

	if _, _, err := stores.RunWorktrees.InsertSystem(ctx, runmode.LocalDefaultOrgID, domain.RunWorktree{
		RunID: "run-pr-own", RepoID: "acme/api", Path: wt, Ref: "pr-58",
	}); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	// Reproduce chownWorktreeForSandbox's effect: the worktree ends up owned
	// by a uid other than this (root) process's. Git's dubious-ownership
	// check only compares the top-level directory's owner, so chowning wt
	// itself (rather than walking the whole tree like the real chown does)
	// reproduces the failure exactly.
	if err := os.Chown(wt, 65534, 65534); err != nil {
		t.Skipf("can't chown %s to a foreign uid (needs root/CAP_CHOWN): %v", wt, err)
	}

	d, err := gitAuthorizeDecision(ctx, stores, info, "acme", "api")
	if err != nil {
		t.Fatalf("gitAuthorizeDecision: %v", err)
	}
	if !d.Allowed || !equalRefs(d.AllowedRefs, []string{"refs/heads/aa/SKY-101"}) {
		t.Errorf("decision against a foreign-owned worktree = %+v, want Allowed with refs/heads/aa/SKY-101 (dubious ownership must not starve AllowedRefs)", d)
	}
}

// TestGitAuthorizeDecision_UniversalProtectionWithoutProfile pins that main and
// master are refused even when the repo has no profile row to name them (the
// universal protected set), so an unprofiled repo can't be pushed to on its
// default branch. A non-default feature branch on the same repo is still
// authorized.
func TestGitAuthorizeDecision_UniversalProtectionWithoutProfile(t *testing.T) {
	database := newDelegateTestDB(t)
	stores := sqlitestore.New(database)
	ctx := context.Background()
	seedRun(t, database, "run-3", "sess", "/tmp/wt")
	info := agenthost.RunInfo{OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID, RunID: "run-3"}

	// Track + materialize a repo with NO repo_profiles row.
	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, []domain.TeamGitHubRepo{
		{Owner: "acme", Repo: "noprofile"},
	}); err != nil {
		t.Fatalf("track repo: %v", err)
	}
	if _, _, err := stores.RunWorktrees.InsertSystem(ctx, runmode.LocalDefaultOrgID, domain.RunWorktree{
		RunID: "run-3", RepoID: "acme/noprofile", Path: "/tmp/np", Ref: "@default",
	}); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	for _, base := range []string{"main", "master"} {
		stubLiveBranch(t, map[string]string{"/tmp/np": base})
		if d, err := gitAuthorizeDecision(ctx, stores, info, "acme", "noprofile"); err != nil || !d.Allowed || len(d.AllowedRefs) != 0 {
			t.Errorf("on %q without a profile: decision=%+v err=%v; want Allowed=true, no refs", base, d, err)
		}
	}

	// A feature branch is still authorized.
	stubLiveBranch(t, map[string]string{"/tmp/np": "fix/thing"})
	if d, err := gitAuthorizeDecision(ctx, stores, info, "acme", "noprofile"); err != nil || !equalRefs(d.AllowedRefs, []string{"refs/heads/fix/thing"}) {
		t.Errorf("on feature branch without a profile: decision=%+v err=%v; want refs/heads/fix/thing", d, err)
	}
}

// TestGitAuthorizeDecision_FailsClosedWithoutReposStore pins that a wiring
// missing the Repos store denies — it can't name the protected refs, so it must
// not fall through to authorizing whatever branch the checkout is on.
func TestGitAuthorizeDecision_FailsClosedWithoutReposStore(t *testing.T) {
	database := newDelegateTestDB(t)
	full := sqlitestore.New(database)
	ctx := context.Background()
	seedRun(t, database, "run-4", "sess", "/tmp/wt")
	info := agenthost.RunInfo{OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID, RunID: "run-4"}

	if err := full.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, []domain.TeamGitHubRepo{
		{Owner: "acme", Repo: "api"},
	}); err != nil {
		t.Fatalf("track repo: %v", err)
	}
	if _, _, err := full.RunWorktrees.InsertSystem(ctx, runmode.LocalDefaultOrgID, domain.RunWorktree{
		RunID: "run-4", RepoID: "acme/api", Path: "/tmp/api4", Ref: "@default",
	}); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	stubLiveBranch(t, map[string]string{"/tmp/api4": "agent/feature"})

	// Repos omitted from the wiring — even a clean feature-branch checkout must
	// deny, because the gate can't determine the protected set.
	partial := db.Stores{TeamGitHubRepos: full.TeamGitHubRepos, RunWorktrees: full.RunWorktrees}
	if d, err := gitAuthorizeDecision(ctx, partial, info, "acme", "api"); err != nil || d.Allowed {
		t.Errorf("missing Repos store: decision=%+v err=%v; want deny (fail closed)", d, err)
	}
}

func equalRefs(a, b []string) bool {
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
