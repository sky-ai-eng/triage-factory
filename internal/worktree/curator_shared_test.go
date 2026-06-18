package worktree

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// sharedOrg is the org id the shared-worktree tests materialize under. Using
// the local-default sentinel keeps paths.OrgRoot collapsed onto the test HOME
// (withTestHome) so the checkout lands under the tempdir without flipping
// runmode into multi.
const sharedOrg = runmode.LocalDefaultOrgID

// TestEnsureSharedCuratorWorktree_FreshMaterializesAtSharedPath pins the
// org-scoped shared layout: <OrgRoot>/curator-repos/<owner>/<repo>, a real
// checkout, and a non-nil release.
func TestEnsureSharedCuratorWorktree_FreshMaterializesAtSharedPath(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)
	if _, err := EnsureBareClone(context.Background(), "owner", "shared", upstream); err != nil {
		t.Fatalf("seed bare: %v", err)
	}

	wt, release, err := EnsureSharedCuratorWorktree(context.Background(), sharedOrg, "owner", "shared", "main")
	if err != nil {
		t.Fatalf("EnsureSharedCuratorWorktree: %v", err)
	}
	defer release()

	want := paths.CuratorSharedRepoDir(sharedOrg, "owner", "shared")
	if wt != want {
		t.Errorf("shared worktree path = %q, want %q", wt, want)
	}
	if _, err := os.Stat(filepath.Join(wt, ".git")); err != nil {
		t.Errorf("expected .git in shared worktree, got %v", err)
	}
	if release == nil {
		t.Error("release func must be non-nil on success")
	}
}

// TestEnsureSharedCuratorWorktree_SharesSingleCheckout is the core TFAC-61
// property: two concurrent sessions on the same pinned repo resolve to ONE
// on-disk worktree (no per-session duplication), and the same path.
func TestEnsureSharedCuratorWorktree_SharesSingleCheckout(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)
	if _, err := EnsureBareClone(context.Background(), "o", "r", upstream); err != nil {
		t.Fatalf("seed bare: %v", err)
	}

	// First reader materializes and stays active (not released) so the second
	// Acquire observes a live reader.
	wt1, rel1, err := EnsureSharedCuratorWorktree(context.Background(), sharedOrg, "o", "r", "main")
	if err != nil {
		t.Fatalf("reader 1: %v", err)
	}
	defer rel1()
	wt2, rel2, err := EnsureSharedCuratorWorktree(context.Background(), sharedOrg, "o", "r", "main")
	if err != nil {
		t.Fatalf("reader 2: %v", err)
	}
	defer rel2()

	if wt1 != wt2 {
		t.Errorf("two sessions got different worktrees %q vs %q — not shared", wt1, wt2)
	}

	// Exactly one worktree is registered against the bare (single
	// materialization, no per-session duplication). Each linked worktree gets
	// a subdir under <bare>/worktrees/; count them directly.
	bareDir, err := repoDir("o", "r")
	if err != nil {
		t.Fatalf("repoDir: %v", err)
	}
	if n := linkedWorktreeCount(t, bareDir); n != 1 {
		t.Errorf("linked worktree count = %d, want 1 (one shared checkout)", n)
	}
}

// linkedWorktreeCount returns how many linked worktrees are registered against
// the bare at bareDir (one subdir per worktree under <bare>/worktrees/).
func linkedWorktreeCount(t *testing.T, bareDir string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(bareDir, "worktrees"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read worktrees dir: %v", err)
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			n++
		}
	}
	return n
}

// TestEnsureSharedCuratorWorktree_SkipsResetWithLiveReaders is the live-reader
// safety contract: while a jail is reading the shared tree, a second Acquire
// must NOT reset/clean it (that would tear files out from under the reader);
// once the tree goes quiescent (all readers released), the next Acquire
// refreshes it.
func TestEnsureSharedCuratorWorktree_SkipsResetWithLiveReaders(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)
	if _, err := EnsureBareClone(context.Background(), "o", "r", upstream); err != nil {
		t.Fatalf("seed bare: %v", err)
	}

	// Reader 1 materializes and stays active.
	wt, rel1, err := EnsureSharedCuratorWorktree(context.Background(), sharedOrg, "o", "r", "main")
	if err != nil {
		t.Fatalf("reader 1: %v", err)
	}

	// An untracked scratch file in the tree — `clean -fdx` would remove it.
	scratch := filepath.Join(wt, "scratch.txt")
	if err := os.WriteFile(scratch, []byte("live-reader state\n"), 0o644); err != nil {
		t.Fatalf("write scratch: %v", err)
	}

	// Reader 2 arrives while reader 1 is live → refresh must be skipped, so the
	// scratch file survives (the shared snapshot is served untouched).
	_, rel2, err := EnsureSharedCuratorWorktree(context.Background(), sharedOrg, "o", "r", "main")
	if err != nil {
		t.Fatalf("reader 2: %v", err)
	}
	if _, err := os.Stat(scratch); err != nil {
		t.Errorf("reset ran under a live reader — scratch file gone (%v); must skip reset while readers active", err)
	}

	// Both readers leave; the tree is quiescent.
	rel1()
	rel2()

	// Reader 3 (no live readers) → refresh runs, clean -fdx removes the scratch.
	_, rel3, err := EnsureSharedCuratorWorktree(context.Background(), sharedOrg, "o", "r", "main")
	if err != nil {
		t.Fatalf("reader 3: %v", err)
	}
	defer rel3()
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("quiescent refresh did not clean untracked scratch (stat err = %v)", err)
	}
}

// TestEnsureSharedCuratorWorktree_ConcurrentAcquireNoRace runs many concurrent
// Acquire/release cycles for the same repo to shake out data races on the
// reader registry + per-repo git ops under -race.
func TestEnsureSharedCuratorWorktree_ConcurrentAcquireNoRace(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)
	if _, err := EnsureBareClone(context.Background(), "o", "r", upstream); err != nil {
		t.Fatalf("seed bare: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wt, rel, err := EnsureSharedCuratorWorktree(context.Background(), sharedOrg, "o", "r", "main")
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			if !dirExists(wt) {
				t.Errorf("worktree %q missing after acquire", wt)
			}
			rel()
		}()
	}
	wg.Wait()

	// After every reader released, the registry entry is gone, so the next
	// Acquire refreshes (readers==0) — exercised here as a smoke check it still
	// succeeds and leaves exactly one checkout.
	wt, rel, err := EnsureSharedCuratorWorktree(context.Background(), sharedOrg, "o", "r", "main")
	if err != nil {
		t.Fatalf("final acquire: %v", err)
	}
	defer rel()
	if !dirExists(wt) {
		t.Errorf("final worktree %q missing", wt)
	}
}

// TestEnsureSharedCuratorWorktree_RejectsBadArgs covers the guard rails.
func TestEnsureSharedCuratorWorktree_RejectsBadArgs(t *testing.T) {
	withTestHome(t)
	for _, tc := range []struct {
		name                     string
		org, owner, repo, branch string
	}{
		{"org", "", "o", "r", "main"},
		{"owner", sharedOrg, "", "r", "main"},
		{"repo", sharedOrg, "o", "", "main"},
		{"branch", sharedOrg, "o", "r", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := EnsureSharedCuratorWorktree(context.Background(), tc.org, tc.owner, tc.repo, tc.branch)
			if err == nil {
				t.Errorf("expected error for empty %s", tc.name)
			}
		})
	}
}

// TestEnsureSharedCuratorWorktree_RejectsMissingBareWithoutURL mirrors the
// per-project contract: a missing bare with no seed URL is a clear error.
func TestEnsureSharedCuratorWorktree_RejectsMissingBareWithoutURL(t *testing.T) {
	withTestHome(t)
	_, _, err := EnsureSharedCuratorWorktree(context.Background(), sharedOrg, "ghost", "repo", "main")
	if err == nil {
		t.Fatal("expected error for missing bare with no clone URL")
	}
}

// TestSharedWorktree_QuiescentDoesNotPinBare is the reaper-interaction
// contract (TFAC-61): a shared worktree pins its bare (non-evictable) only
// while a jail is actively reading it; once quiescent (readers==0) the bare is
// reclaimable, so a shared checkout can't defeat the bounded bare cache.
func TestSharedWorktree_QuiescentDoesNotPinBare(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)
	if _, err := EnsureBareClone(context.Background(), "o", "r", upstream); err != nil {
		t.Fatalf("seed bare: %v", err)
	}
	bareDir, err := repoDir("o", "r")
	if err != nil {
		t.Fatalf("repoDir: %v", err)
	}

	_, release, err := EnsureSharedCuratorWorktree(context.Background(), sharedOrg, "o", "r", "main")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if !bareHasLiveWorktrees(bareDir) {
		t.Error("bare with an actively-read shared worktree must be live (non-evictable)")
	}

	release()
	if bareHasLiveWorktrees(bareDir) {
		t.Error("bare with only a QUIESCENT shared worktree must be evictable (readers==0)")
	}
}

// TestPerProjectWorktree_StillPinsBare is the regression guard: the
// reaper-liveness change is scoped to shared worktrees — a per-project curator
// worktree (under the project KB dir, not curator-repos) still keeps its bare
// live by mere existence, exactly as before.
func TestPerProjectWorktree_StillPinsBare(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)
	if _, err := EnsureBareClone(context.Background(), "o", "r", upstream); err != nil {
		t.Fatalf("seed bare: %v", err)
	}
	bareDir, err := repoDir("o", "r")
	if err != nil {
		t.Fatalf("repoDir: %v", err)
	}
	projectDir := filepath.Join(t.TempDir(), "proj")
	if _, err := EnsureCuratorWorktree(context.Background(), "o", "r", "main", projectDir); err != nil {
		t.Fatalf("curator worktree: %v", err)
	}
	if !bareHasLiveWorktrees(bareDir) {
		t.Error("a per-project curator worktree must keep its bare live by existence (unchanged)")
	}
}

// TestEnsureSharedCuratorWorktree_RecreatesAfterExternalRemoval is the
// self-heal contract: if the checkout is removed out from under the bare
// (operator cleanup / crash mid-eviction) while the bare keeps the admin
// registration, the next Acquire prunes the stale entry and re-materializes
// instead of failing "already registered as a linked worktree" — which would
// otherwise strand every session pinning this repo.
func TestEnsureSharedCuratorWorktree_RecreatesAfterExternalRemoval(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)
	if _, err := EnsureBareClone(context.Background(), "o", "r", upstream); err != nil {
		t.Fatalf("seed bare: %v", err)
	}

	wt, release, err := EnsureSharedCuratorWorktree(context.Background(), sharedOrg, "o", "r", "main")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	release()

	// Remove the checkout out-of-band; the bare keeps the admin registration.
	if err := os.RemoveAll(wt); err != nil {
		t.Fatalf("remove checkout: %v", err)
	}

	// Next acquire must prune the stale entry and recreate, not fail.
	wt2, release2, err := EnsureSharedCuratorWorktree(context.Background(), sharedOrg, "o", "r", "main")
	if err != nil {
		t.Fatalf("re-acquire after external removal: %v (prune-before-add should self-heal)", err)
	}
	defer release2()
	if wt2 != wt {
		t.Errorf("recreated worktree path = %q, want %q", wt2, wt)
	}
	if _, err := os.Stat(filepath.Join(wt2, ".git")); err != nil {
		t.Errorf("recreated worktree missing .git: %v", err)
	}
}
