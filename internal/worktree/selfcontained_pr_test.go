package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestCreateForPR_SelfContainedClone_MultiMode is the multi-mode PR-path
// acceptance test. In multi mode the run root is bind-mounted into a sandbox
// that can't see the shared bare, so a linked worktree (whose .git points back
// into the bare) is dead on arrival there. CreateForPR must instead hand back a
// SELF-CONTAINED clone: its .git a real directory, the working tree materialized
// (the deferred blob lazily fetched through the repointed promisor origin,
// authenticated), the `git push` wiring in the clone's own config so it ships
// into the sandbox, and no per-run ref left behind in the shared bare.
//
// The local-mode counterpart (TestCreateForPR_BloblessPrivateRepo) stays on the
// zero-copy worktree path — the bare is reachable on-host there.
func TestCreateForPR_SelfContainedClone_MultiMode(t *testing.T) {
	withTestHome(t)
	t.Setenv("GIT_SSL_NO_VERIFY", "true") // accept the httptest self-signed cert
	// Multi mode routes CreateForPR through the self-contained clone. Point the
	// state root (where the shared bare lives) at a tempdir so multi-mode
	// StateRoot doesn't resolve to /data; TMPDIR (withTestHome) holds the run root.
	runmode.SetForTest(t, runmode.ModeMulti)
	t.Setenv("TF_STATE_ROOT", t.TempDir())

	reposRoot := t.TempDir()
	const token = "ghs_selfcontained_token"
	_, content := seedBloblessUpstream(t, reposRoot, "repo", "refs/pull/7/head")
	cloneURL := startAuthedGitServer(t, reposRoot, "x-access-token", token) + "/repo.git"

	const runID = "pr-run-multi"
	const prNumber = 7
	// Own-repo PR: head URL == upstream URL. WithBaseBranch exercises the
	// clone-side base fetch (diff framing) too.
	wtPath, err := CreateForPR(context.Background(), "acme", "repo", cloneURL, cloneURL, "feature-branch", prNumber, runID,
		WithCloneAuth(CloneAuthFor(cloneURL, token)), WithBaseBranch("main"))
	if err != nil {
		t.Fatalf("CreateForPR (multi mode, self-contained): %v", err)
	}
	t.Cleanup(func() { _ = RemoveAt(wtPath, runID) })

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

	// origin repointed at the upstream (not the unreachable bare path) and marked
	// a blobless promisor, so in-sandbox lazy fetch / push resolve through the
	// gitproxy against GitHub.
	if url := gitCfgValue(t, wtPath, "remote.origin.url"); url != cloneURL {
		t.Errorf("origin url = %q, want %q (upstream, not the bare path)", url, cloneURL)
	}
	if p := gitCfgValue(t, wtPath, "remote.origin.promisor"); p != "true" {
		t.Errorf("remote.origin.promisor = %q, want true", p)
	}

	// The base branch's remote-tracking ref is present in the clone for diff framing.
	if err := exec.Command("git", "-C", wtPath, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main").Run(); err != nil {
		t.Errorf("clone is missing refs/remotes/origin/main (base tracking ref for diff framing): %v", err)
	}

	// Push tracking lives in the CLONE's config (so it ships into the sandbox),
	// not the shared bare. Own-repo PR → a per-run push remote at the upstream.
	localBranch := prLocalBranch(runID, prNumber)
	remote := prPushRemoteName(runID, prNumber)
	if r := gitCfgValue(t, wtPath, "branch."+localBranch+".pushRemote"); r != remote {
		t.Errorf("clone branch.%s.pushRemote = %q, want %q", localBranch, r, remote)
	}
	if u := gitCfgValue(t, wtPath, "remote."+remote+".url"); u != cloneURL {
		t.Errorf("clone push remote %s url = %q, want %q", remote, u, cloneURL)
	}

	// The shared bare is left with no per-run ref (dropBareRunRefs ran after the
	// clone copied its objects).
	bareDir, err := repoDir("acme", "repo")
	if err != nil {
		t.Fatalf("repoDir: %v", err)
	}
	if exec.Command("git", "-C", bareDir, "show-ref", "--verify", "--quiet", "refs/heads/"+localBranch).Run() == nil {
		t.Errorf("shared bare still has per-run branch refs/heads/%s; it should be dropped after the clone", localBranch)
	}
}

// gitCfgValue reads a single git config value from dir, returning "" when the
// key is unset or the read fails.
func gitCfgValue(t *testing.T, dir, key string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "config", "--get", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
