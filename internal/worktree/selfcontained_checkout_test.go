package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestCreateForCheckoutInRoot_SelfContainedClone_MultiMode is the multi-mode
// acceptance test for the `workspace add` default/--ref path (TFAC-546). In
// multi mode the run root is bind-mounted into a sandbox that can't see the
// shared bare, so a linked worktree (whose .git points back into the bare) is
// dead on arrival there. CreateForCheckoutInRoot must instead hand back a
// SELF-CONTAINED clone that preserves the checkout path's invariants: DETACHED
// at the fetched tip (no live branch → the push gate authorizes nothing until
// the agent branches), origin repointed at the upstream as a blobless
// promisor, an origin/<ref> tracking ref to branch/diff against, and no
// transient per-run ref left behind in the clone or the shared bare.
func TestCreateForCheckoutInRoot_SelfContainedClone_MultiMode(t *testing.T) {
	withTestHome(t)
	t.Setenv("GIT_SSL_NO_VERIFY", "true") // accept the httptest self-signed cert
	runmode.SetForTest(t, runmode.ModeMulti)
	t.Setenv("TF_STATE_ROOT", t.TempDir())

	reposRoot := t.TempDir()
	const token = "ghs_selfcontained_checkout_token"
	_, content := seedBloblessUpstream(t, reposRoot, "repo")
	cloneURL := startAuthedGitServer(t, reposRoot, "x-access-token", token) + "/repo.git"

	const rootKey = "checkout-run-multi"
	runRoot := t.TempDir()
	wtPath, err := CreateForCheckoutInRoot(context.Background(), "acme", "repo", cloneURL, "", rootKey, runRoot,
		WithCloneAuth(CloneAuthFor(cloneURL, token)))
	if err != nil {
		t.Fatalf("CreateForCheckoutInRoot (multi mode, self-contained): %v", err)
	}
	if want := filepath.Join(runRoot, "acme", "repo", "default"); wtPath != want {
		t.Fatalf("wtPath = %q, want %q", wtPath, want)
	}

	// The defining property: .git is a real directory (self-contained), not a
	// worktree pointer file — so nothing dangles when mounted in the sandbox.
	info, err := os.Stat(filepath.Join(wtPath, ".git"))
	if err != nil || !info.IsDir() {
		t.Fatalf(".git is not a self-contained directory (err=%v); a worktree pointer would dangle in the sandbox", err)
	}

	// Working tree materialized — the blobless clone deferred the blob and the
	// authenticated promisor checkout fetched it back.
	got, err := os.ReadFile(filepath.Join(wtPath, "canary.txt"))
	if err != nil || string(got) != content {
		t.Fatalf("canary.txt = %q (err=%v), want %q", got, err, content)
	}

	// Detached: the checkout claims no branch, so the push gate authorizes
	// nothing until the agent creates its own.
	if b := CurrentBranch(wtPath); b != "" {
		t.Errorf("CurrentBranch = %q, want detached HEAD", b)
	}
	if target := PushTargetBranch(wtPath); target != "" {
		t.Errorf("PushTargetBranch = %q, want \"\" (detached authorizes nothing)", target)
	}

	// origin repointed at the upstream (not the unreachable bare path) and marked
	// a blobless promisor, so in-sandbox lazy fetch / push resolve through the
	// gitproxy against GitHub.
	if url := gitCfgValue(t, wtPath, "remote.origin.url"); url != cloneURL {
		t.Errorf("origin url = %q, want %q (upstream, not the bare path)", url, cloneURL)
	}
	if p := gitCfgValue(t, wtPath, "remote.origin.promisor"); p != "true" {
		t.Errorf("remote.origin.promisor = %q, want true", p)
	}

	// The checked-out ref's tracking ref is present so the agent can branch off
	// / diff against origin/main, and the fetch refspec is repointed at the
	// real ref (not the transient staging branch) so `git fetch` refreshes it.
	if err := exec.Command("git", "-C", wtPath, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main").Run(); err != nil {
		t.Errorf("clone is missing refs/remotes/origin/main: %v", err)
	}
	if spec := gitCfgValue(t, wtPath, "remote.origin.fetch"); spec != "+refs/heads/main:refs/remotes/origin/main" {
		t.Errorf("remote.origin.fetch = %q, want +refs/heads/main:refs/remotes/origin/main", spec)
	}

	// The transient staging branch is gone from the clone AND the shared bare.
	staging := "triagefactory/" + rootKey + "/checkout"
	if exec.Command("git", "-C", wtPath, "show-ref", "--verify", "--quiet", "refs/heads/"+staging).Run() == nil {
		t.Errorf("run clone still has staging branch refs/heads/%s", staging)
	}
	bareDir, err := repoDir("acme", "repo")
	if err != nil {
		t.Fatalf("repoDir: %v", err)
	}
	if exec.Command("git", "-C", bareDir, "show-ref", "--verify", "--quiet", "refs/heads/"+staging).Run() == nil {
		t.Errorf("shared bare still has staging branch refs/heads/%s; it should be dropped after the clone", staging)
	}

	// The intended agent flow works end-to-end in the clone: branch off the
	// detached tip, and the push gate resolves that live branch as the target.
	runGit(t, "-C", wtPath, "checkout", "-b", "aa/TFAC-546-feature")
	if target := PushTargetBranch(wtPath); target != "aa/TFAC-546-feature" {
		t.Errorf("PushTargetBranch after branching = %q, want aa/TFAC-546-feature", target)
	}
}

// TestCreateForCheckoutInRoot_RejectsInvalidRef pins the interpolation-point
// guard: the create surface is reachable from the agenthost RPC (TFAC-546), so
// it must refuse an injection-shaped ref itself, not rely on the workspace
// CLI's argv validation.
func TestCreateForCheckoutInRoot_RejectsInvalidRef(t *testing.T) {
	withTestHome(t)
	if _, err := CreateForCheckoutInRoot(context.Background(), "acme", "repo", "https://example.invalid/repo.git", "--upload-pack=evil", "run-x", t.TempDir()); err == nil {
		t.Fatal("CreateForCheckoutInRoot accepted an injection-shaped ref; want validation error")
	}
}
