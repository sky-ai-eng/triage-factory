package worktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// restoreModes runs a test once per checkout layout: the self-contained clone a
// sandboxed run gets, and the linked worktree an unsandboxed one gets.
var restoreModes = []struct {
	name          string
	selfContained bool
}{
	{"self-contained", true},
	{"linked", false},
}

func setRestoreMode(t *testing.T, selfContained bool) {
	t.Helper()
	withTestHome(t)
	if selfContained {
		sandboxingMode(t)
		t.Setenv("TF_STATE_ROOT", t.TempDir())
		return
	}
	paths.SetForTest(t, t.TempDir())
	runmode.SetForTest(t, runmode.ModeLocal)
}

// dirtyCheckout gives a checkout an unpushed commit, an uncommitted edit to a
// tracked file and an untracked file — the three kinds of work a restore has
// to bring back.
func dirtyCheckout(t *testing.T, wtDir string) {
	t.Helper()
	gitAt(t, wtDir, "config", "user.email", "test@example.com")
	gitAt(t, wtDir, "config", "user.name", "Test")
	writeUnder(t, wtDir, "committed.txt", "unpushed commit\n")
	gitAt(t, wtDir, "add", "committed.txt")
	gitAt(t, wtDir, "commit", "-q", "-m", "agent commit")
	writeUnder(t, wtDir, "pr.txt", "uncommitted edit\n")
	writeUnder(t, wtDir, "untracked.txt", "untracked file\n")
}

func assertDirtyCheckout(t *testing.T, wtDir, head string) {
	t.Helper()
	assertFileContent(t, filepath.Join(wtDir, "committed.txt"), "unpushed commit\n")
	assertFileContent(t, filepath.Join(wtDir, "pr.txt"), "uncommitted edit\n")
	assertFileContent(t, filepath.Join(wtDir, "untracked.txt"), "untracked file\n")
	if got := strings.TrimSpace(gitAt(t, wtDir, "rev-parse", "HEAD")); got != head {
		t.Errorf("restored HEAD = %s, want %s", got, head)
	}
}

// TestRestoreCheckout_PRRoundTrip is a PR checkout with all three kinds of
// work, rebuilt after its run root is gone — on a host that kept its bare and
// on a fresh one that did not. It comes back exactly, on its branch, with the
// push tracking a fresh --pr checkout gets; and a capture of the restored
// checkout still bundles the unpushed commit, which a tracking ref pointed at
// the local tip would have dropped.
func TestRestoreCheckout_PRRoundTrip(t *testing.T) {
	for _, mode := range restoreModes {
		for _, freshBare := range []bool{false, true} {
			name := mode.name
			if freshBare {
				name += "/fresh-bare"
			}
			t.Run(name, func(t *testing.T) {
				setRestoreMode(t, mode.selfContained)
				upstream := makeSkillsTrackingUpstream(t)
				const key = "pr-roundtrip-run"
				root := mustRunRoot(t, key)
				t.Cleanup(func() { RemoveRunRoot(key) })

				wtDir, err := CreateForPRInRoot(context.Background(), "acme", "repo", upstream, upstream, "feature", 7, "conv-a", root)
				if err != nil {
					t.Fatalf("CreateForPRInRoot: %v", err)
				}
				dirtyCheckout(t, wtDir)
				delta, err := CaptureWorkspaceGit(context.Background(), wtDir)
				if err != nil {
					t.Fatalf("CaptureWorkspaceGit: %v", err)
				}
				if delta.Branch != prLocalBranch("conv-a", 7) || len(delta.Bundle) == 0 || len(delta.Patch) == 0 {
					t.Fatalf("capture = branch %q, %d bundle bytes, %d patch bytes; want the PR branch with both members", delta.Branch, len(delta.Bundle), len(delta.Patch))
				}

				RemoveRunRoot(key)
				if freshBare {
					bare, _ := repoDir("acme", "repo")
					if err := os.RemoveAll(bare); err != nil {
						t.Fatalf("remove bare: %v", err)
					}
				}
				root = mustRunRoot(t, key)
				got := restoreDelta(t, root, key, "acme", "repo", PRRefSlug(7), upstream, CloneAuth{}, delta,
					&PRCheckout{HeadRef: "feature", HeadCloneURL: upstream, BaseRef: "main"})

				if got != filepath.Join(root, "acme", "repo", "pr-7") {
					t.Errorf("restored at %s, want acme/repo/pr-7 under the run root", got)
				}
				assertDirtyCheckout(t, got, delta.Head)
				if b := CurrentBranch(got); b != delta.Branch {
					t.Errorf("restored branch = %q, want %q", b, delta.Branch)
				}
				if target := PushTargetBranch(got); target != "feature" {
					t.Errorf("push target = %q, want the PR head branch", target)
				}
				if fi, err := os.Stat(filepath.Join(got, ".git")); err != nil || fi.IsDir() != mode.selfContained {
					t.Errorf(".git dir = %v (err=%v), want a directory exactly when self-contained", fi != nil && fi.IsDir(), err)
				}

				again, err := CaptureWorkspaceGit(context.Background(), got)
				if err != nil {
					t.Fatalf("capture of the restored checkout: %v", err)
				}
				if again.Head != delta.Head || len(again.Bundle) == 0 {
					t.Errorf("recapture = head %s, %d bundle bytes; want head %s with the unpushed commit still bundled", again.Head, len(again.Bundle), delta.Head)
				}
			})
		}
	}
}

// TestRestoreCheckout_BranchOffDetachedCheckout is the `workspace add` shape: a
// detached checkout of the repo's branch that the agent branched from itself.
// The branch comes back with its commit, and a self-contained clone's origin
// fetches the tracked branch as a fresh one does.
func TestRestoreCheckout_BranchOffDetachedCheckout(t *testing.T) {
	for _, mode := range restoreModes {
		t.Run(mode.name, func(t *testing.T) {
			setRestoreMode(t, mode.selfContained)
			upstream := makeSkillsTrackingUpstream(t)
			const key = "branch-roundtrip-run"
			root := mustRunRoot(t, key)
			t.Cleanup(func() { RemoveRunRoot(key) })

			wtDir, err := CreateForCheckoutInRoot(context.Background(), "acme", "repo", upstream, "main", "conv-a", root)
			if err != nil {
				t.Fatalf("CreateForCheckoutInRoot: %v", err)
			}
			gitAt(t, wtDir, "checkout", "-q", "-b", "fix/thing")
			dirtyCheckout(t, wtDir)
			delta, err := CaptureWorkspaceGit(context.Background(), wtDir)
			if err != nil {
				t.Fatalf("CaptureWorkspaceGit: %v", err)
			}

			RemoveRunRoot(key)
			root = mustRunRoot(t, key)
			got := restoreDelta(t, root, key, "acme", "repo", CheckoutRefSlug("main"), upstream, CloneAuth{}, delta, nil)
			assertDirtyCheckout(t, got, delta.Head)
			if b := CurrentBranch(got); b != "fix/thing" {
				t.Errorf("restored branch = %q, want fix/thing", b)
			}
			if mode.selfContained {
				if refspec := gitCfgValue(t, got, "remote.origin.fetch"); refspec != "+refs/heads/main:refs/remotes/origin/main" {
					t.Errorf("origin fetch refspec = %q, want the tracked branch", refspec)
				}
				if url := gitCfgValue(t, got, "remote.origin.url"); url != upstream {
					t.Errorf("origin = %q, want the upstream", url)
				}
			}
		})
	}
}

// TestRestoreCheckout_DetachedRoundTrip: a checkout left detached comes back
// detached at the same commit.
func TestRestoreCheckout_DetachedRoundTrip(t *testing.T) {
	for _, mode := range restoreModes {
		t.Run(mode.name, func(t *testing.T) {
			setRestoreMode(t, mode.selfContained)
			upstream := makeSkillsTrackingUpstream(t)
			const key = "detached-roundtrip-run"
			root := mustRunRoot(t, key)
			t.Cleanup(func() { RemoveRunRoot(key) })

			wtDir, err := CreateForCheckoutInRoot(context.Background(), "acme", "repo", upstream, "", "conv-a", root)
			if err != nil {
				t.Fatalf("CreateForCheckoutInRoot: %v", err)
			}
			dirtyCheckout(t, wtDir)
			delta, err := CaptureWorkspaceGit(context.Background(), wtDir)
			if err != nil {
				t.Fatalf("CaptureWorkspaceGit: %v", err)
			}
			if delta.Branch != "" {
				t.Fatalf("fixture is on branch %q; want detached", delta.Branch)
			}

			RemoveRunRoot(key)
			root = mustRunRoot(t, key)
			got := restoreDelta(t, root, key, "acme", "repo", CheckoutRefSlug(""), upstream, CloneAuth{}, delta, nil)
			assertDirtyCheckout(t, got, delta.Head)
			if b := CurrentBranch(got); b != "" {
				t.Errorf("restored onto branch %q; want detached", b)
			}
		})
	}
}

// TestRestoreCheckout_DeletedForkIsReadOnly: a PR whose head repository is gone
// comes back reviewable and with no push remote, as on a fresh run.
func TestRestoreCheckout_DeletedForkIsReadOnly(t *testing.T) {
	setRestoreMode(t, true)
	upstream := makeSkillsTrackingUpstream(t)
	const key = "deleted-fork-run"
	root := mustRunRoot(t, key)
	t.Cleanup(func() { RemoveRunRoot(key) })

	wtDir, err := CreateForPRInRoot(context.Background(), "acme", "repo", upstream, "", "feature", 7, "conv-a", root)
	if err != nil {
		t.Fatalf("CreateForPRInRoot: %v", err)
	}
	delta, err := CaptureWorkspaceGit(context.Background(), wtDir)
	if err != nil {
		t.Fatalf("CaptureWorkspaceGit: %v", err)
	}
	RemoveRunRoot(key)
	root = mustRunRoot(t, key)
	got := restoreDelta(t, root, key, "acme", "repo", PRRefSlug(7), upstream, CloneAuth{}, delta, &PRCheckout{HeadRef: "feature", BaseRef: "main"})
	assertFileContent(t, filepath.Join(got, "pr.txt"), "pr change\n")
	if remotes := strings.TrimSpace(gitAt(t, got, "remote")); remotes != "origin" {
		t.Errorf("remotes = %q, want origin alone", remotes)
	}
}

// TestRestoreCheckout_FailureLeavesNothing: a checkout that cannot be rebuilt —
// its captured HEAD is nowhere — fails the restore and leaves no directory and
// no staging branch behind.
func TestRestoreCheckout_FailureLeavesNothing(t *testing.T) {
	for _, mode := range restoreModes {
		t.Run(mode.name, func(t *testing.T) {
			setRestoreMode(t, mode.selfContained)
			upstream := makeSkillsTrackingUpstream(t)
			const key = "restore-failure-run"
			root := mustRunRoot(t, key)
			t.Cleanup(func() { RemoveRunRoot(key) })

			delta := &GitDelta{Head: strings.Repeat("ab", 20), Branch: "fix/thing"}
			if _, err := restoreDeltaErr(t, root, key, "acme", "repo", CheckoutRefSlug("main"), upstream, CloneAuth{}, delta, nil); err == nil {
				t.Fatal("restore of a HEAD that exists nowhere succeeded")
			}
			if _, err := os.Stat(filepath.Join(root, "acme", "repo", "ref-main")); !os.IsNotExist(err) {
				t.Errorf("failed restore left its checkout behind (err=%v)", err)
			}
			bare, _ := repoDir("acme", "repo")
			if out := gitAt(t, bare, "for-each-ref", "refs/heads/triagefactory/"); strings.TrimSpace(out) != "" {
				t.Errorf("failed restore left run-scoped refs in the bare:\n%s", out)
			}
		})
	}
}

// TestRestoreCheckout_UpstreamFetch: the refresh of what a checkout tracks may
// find the branch deleted upstream, and the checkout still comes back from the
// bare and the bundle. Any other refusal from origin fails the restore rather
// than rebuild against refs it could not refresh.
func TestRestoreCheckout_UpstreamFetch(t *testing.T) {
	for _, tc := range []struct {
		name     string
		break_   func(t *testing.T, upstream, bare string)
		wantFail bool
	}{
		{"branch deleted upstream", func(t *testing.T, upstream, _ string) {
			gitAt(t, upstream, "update-ref", "-d", "refs/heads/feature")
		}, false},
		{"origin refuses", func(t *testing.T, _, bare string) {
			gitAt(t, bare, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setRestoreMode(t, false)
			upstream := makeSkillsTrackingUpstream(t)
			gitAt(t, upstream, "update-ref", "refs/heads/feature", "refs/heads/main")
			const key = "upstream-fetch-run"
			root := mustRunRoot(t, key)
			t.Cleanup(func() { RemoveRunRoot(key) })

			wtDir, err := CreateForCheckoutInRoot(context.Background(), "acme", "repo", upstream, "feature", "conv-a", root)
			if err != nil {
				t.Fatalf("CreateForCheckoutInRoot: %v", err)
			}
			gitAt(t, wtDir, "checkout", "-q", "-b", "fix/thing")
			dirtyCheckout(t, wtDir)
			delta, err := CaptureWorkspaceGit(context.Background(), wtDir)
			if err != nil {
				t.Fatalf("CaptureWorkspaceGit: %v", err)
			}
			RemoveRunRoot(key)
			bare, _ := repoDir("acme", "repo")
			tc.break_(t, upstream, bare)

			// No clone URL: the restore uses the origin the surviving bare has.
			root = mustRunRoot(t, key)
			got, err := restoreDeltaErr(t, root, key, "acme", "repo", CheckoutRefSlug("feature"), "", CloneAuth{}, delta, nil)
			if tc.wantFail {
				if err == nil || !strings.Contains(err.Error(), "fetch feature") {
					t.Fatalf("restore with origin refusing = %v, want the fetch's failure", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("RestoreCheckout: %v", err)
			}
			assertDirtyCheckout(t, got.Path, delta.Head)
		})
	}
}

// TestRestoreCheckout_FailedPRFetchKeepsTheRunsBranch: a restore that fails
// fetching the PR head has written nothing of the run's PR state, so it leaves
// the run's PR branch in the bare — the one an earlier checkout put there,
// holding its commits — and its push remote as they were.
func TestRestoreCheckout_FailedPRFetchKeepsTheRunsBranch(t *testing.T) {
	setRestoreMode(t, false)
	upstream := makeSkillsTrackingUpstream(t)
	const key = "failed-pr-fetch-run"
	root := mustRunRoot(t, key)
	t.Cleanup(func() { RemoveRunRoot(key) })

	wtDir, err := CreateForPRInRoot(context.Background(), "acme", "repo", upstream, upstream, "feature", 7, "conv-a", root)
	if err != nil {
		t.Fatalf("CreateForPRInRoot: %v", err)
	}
	dirtyCheckout(t, wtDir)
	delta, err := CaptureWorkspaceGit(context.Background(), wtDir)
	if err != nil {
		t.Fatalf("CaptureWorkspaceGit: %v", err)
	}
	RemoveRunRoot(key)
	bare, _ := repoDir("acme", "repo")
	gitAt(t, bare, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))

	root = mustRunRoot(t, key)
	if _, err := restoreDeltaErr(t, root, key, "acme", "repo", PRRefSlug(7), "", CloneAuth{}, delta,
		&PRCheckout{HeadRef: "feature", HeadCloneURL: upstream, BaseRef: "main"}); err == nil {
		t.Fatal("restore with origin refusing the PR head fetch succeeded")
	}
	branch := "refs/heads/" + prLocalBranch("conv-a", 7)
	if got := strings.TrimSpace(gitAt(t, bare, "rev-parse", "--verify", branch)); got != delta.Head {
		t.Errorf("run's PR branch = %s after the failed restore, want it still at %s", got, delta.Head)
	}
	if remotes := gitAt(t, bare, "remote"); !strings.Contains(remotes, prPushRemoteName("conv-a", 7)) {
		t.Errorf("failed restore removed the run's push remote; remotes:\n%s", remotes)
	}
}

// TestRestoreCheckout_FailureDropsBareRunRefs: a self-contained PR restore
// that fails leaves none of the per-run refs it fetched or staged in the
// shared bare — whether it fails before the clone, or its deadline stops it
// partway through (a post-checkout hook that outlasts the deadline is what
// stops it there).
func TestRestoreCheckout_FailureDropsBareRunRefs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cutShort bool
	}{
		{"head missing before the clone", false},
		{"deadline during the clone", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setRestoreMode(t, true)
			upstream := makeSkillsTrackingUpstream(t)
			const key = "failure-drops-refs-run"
			root := mustRunRoot(t, key)
			t.Cleanup(func() { RemoveRunRoot(key) })

			wtDir, err := CreateForPRInRoot(context.Background(), "acme", "repo", upstream, upstream, "feature", 7, "conv-a", root)
			if err != nil {
				t.Fatalf("CreateForPRInRoot: %v", err)
			}
			dirtyCheckout(t, wtDir)
			delta, err := CaptureWorkspaceGit(context.Background(), wtDir)
			if err != nil {
				t.Fatalf("CaptureWorkspaceGit: %v", err)
			}
			RemoveRunRoot(key)
			root = mustRunRoot(t, key)

			r := CheckoutRestore{
				Owner: "acme", Repo: "repo", CloneURL: upstream, Root: root, Slug: PRRefSlug(7), RootKey: key,
				PR:   &PRCheckout{HeadRef: "feature", HeadCloneURL: upstream, BaseRef: "main"},
				Head: delta.Head, Branch: delta.Branch,
			}
			ctx := context.Background()
			if tc.cutShort {
				r.BundlePath = filepath.Join(t.TempDir(), "bundle")
				if err := os.WriteFile(r.BundlePath, delta.Bundle, 0o600); err != nil {
					t.Fatal(err)
				}
				hooks := t.TempDir()
				writeUnder(t, hooks, "post-checkout", "#!/bin/sh\nexec sleep 5 >/dev/null 2>&1\n")
				if err := os.Chmod(filepath.Join(hooks, "post-checkout"), 0o755); err != nil {
					t.Fatal(err)
				}
				t.Setenv("GIT_CONFIG_COUNT", "1")
				t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
				t.Setenv("GIT_CONFIG_VALUE_0", hooks)
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 1500*time.Millisecond)
				defer cancel()
			}
			if _, err := RestoreCheckout(ctx, r); err == nil {
				t.Fatal("restore succeeded")
			}
			bare, _ := repoDir("acme", "repo")
			if out := gitAt(t, bare, "for-each-ref", "refs/heads/triagefactory/", "refs/remotes/origin/triagefactory/"); strings.TrimSpace(out) != "" {
				t.Errorf("failed restore left per-run refs in the bare:\n%s", out)
			}
		})
	}
}

// TestConfigurePRPushTracking_RefusesUnsafeHead: the head repository URL and
// branch GitHub reports are spelled into git arguments, so one shaped like an
// option or an invalid ref name is refused before git sees it.
func TestConfigurePRPushTracking_RefusesUnsafeHead(t *testing.T) {
	setRestoreMode(t, false)
	gitDir := t.TempDir()
	gitAt(t, gitDir, "init", "-q")
	head := strings.Repeat("ab", 20)
	for _, tc := range []struct{ url, branch string }{
		{"--mirror=push", "feature"},
		{"", "feature"},
		{"https://github.com/acme/repo.git", "-feature"},
		{"https://github.com/acme/repo.git", "feature..x"},
	} {
		if err := configurePRPushTrackingAt(context.Background(), gitDir, "conv-a", 7, prLocalBranch("conv-a", 7), tc.url, tc.branch, head); err == nil {
			t.Errorf("push tracking accepted url %q, branch %q", tc.url, tc.branch)
		}
	}
	if remotes := strings.TrimSpace(gitAt(t, gitDir, "remote")); remotes != "" {
		t.Errorf("a refused configuration still wrote remotes: %q", remotes)
	}
}

// TestParseCheckoutSlug pins the inverse of the two slug builders.
func TestParseCheckoutSlug(t *testing.T) {
	for _, ref := range []string{"main", "feature/foo", "PROJ-12/a.b"} {
		got, n, ok := ParseCheckoutSlug(CheckoutRefSlug(ref))
		if !ok || n != 0 || got != ref {
			t.Errorf("ParseCheckoutSlug(%q) = %q, %d, %v; want %q", CheckoutRefSlug(ref), got, n, ok, ref)
		}
	}
	if ref, n, ok := ParseCheckoutSlug("default"); !ok || ref != "" || n != 0 {
		t.Errorf("default = %q, %d, %v", ref, n, ok)
	}
	if _, n, ok := ParseCheckoutSlug(PRRefSlug(42)); !ok || n != 42 {
		t.Errorf("pr-42 = %d, %v", n, ok)
	}
	for _, bad := range []string{"", "pr-0", "pr-x", "ref-", "ref-..", "main", "pr-01"} {
		if _, _, ok := ParseCheckoutSlug(bad); ok {
			t.Errorf("ParseCheckoutSlug(%q) accepted", bad)
		}
	}
}
