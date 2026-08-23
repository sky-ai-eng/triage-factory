package worktree

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// seedRenamedRepoDirs fabricates the on-disk shape a rename orphans: the old
// slug's bare, a registered (cold) checkout for it, and a sibling
// repository's bare that must survive every case.
func seedRenamedRepoDirs(t *testing.T) (bare, sibling string) {
	t.Helper()
	paths.SetForTest(t, t.TempDir())
	org := runmode.LocalDefaultOrgID

	bare = paths.BareCacheDir(org, "octo", "api")
	sibling = paths.BareCacheDir(org, "octo", "api-gateway")
	for _, dir := range []string{bare, sibling} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("seed %s: %v", dir, err)
		}
	}
	return bare, sibling
}

// registerWorktree writes the bare-admin entry bareHasLiveWorktrees reads —
// worktrees/<name>/gitdir pointing at checkout's .git — the same shape a real
// `git worktree add` records.
func registerWorktree(t *testing.T, bare, name, checkout string) {
	t.Helper()
	adminDir := filepath.Join(bare, "worktrees", name)
	if err := os.MkdirAll(adminDir, 0o755); err != nil {
		t.Fatalf("register worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(adminDir, "gitdir"), []byte(filepath.Join(checkout, ".git")+"\n"), 0o644); err != nil {
		t.Fatalf("write gitdir: %v", err)
	}
}

func TestDisposeRenamedRepoDirs_RemovesTheOrphanedTrees(t *testing.T) {
	bare, sibling := seedRenamedRepoDirs(t)

	// A registered checkout whose directory is gone — the cold ghost a
	// crashed run leaves — must not read as live.
	registerWorktree(t, bare, "dead-run", filepath.Join(t.TempDir(), "gone"))

	if !DisposeRenamedRepoDirs(runmode.LocalDefaultOrgID, "octo", "api") {
		t.Fatal("DisposeRenamedRepoDirs = false, want a removal")
	}
	if _, err := os.Stat(bare); !os.IsNotExist(err) {
		t.Errorf("old bare still on disk: %v", err)
	}
	// The no-eviction promise for everything the rename did not touch: the
	// sibling repository keeps its clone whatever happens next door.
	if _, err := os.Stat(sibling); err != nil {
		t.Errorf("sibling repository's bare was touched: %v", err)
	}

	// A second call finds nothing — the disposal is not a standing reaper.
	if DisposeRenamedRepoDirs(runmode.LocalDefaultOrgID, "octo", "api") {
		t.Error("second disposal reported a removal with nothing left to remove")
	}
}

func TestDisposeRenamedRepoDirs_ALiveWorktreeBlocksEverything(t *testing.T) {
	bare, sibling := seedRenamedRepoDirs(t)

	checkout := t.TempDir() // exists → the worktree is live
	registerWorktree(t, bare, "live-run", checkout)

	if DisposeRenamedRepoDirs(runmode.LocalDefaultOrgID, "octo", "api") {
		t.Fatal("DisposeRenamedRepoDirs = true, want a refusal under a live worktree")
	}
	for _, dir := range []string{bare, sibling} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("%s was removed under a live worktree: %v", dir, err)
		}
	}
}

func TestDisposeRenamedRepoDirs_NothingOnDiskIsANoOp(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	if DisposeRenamedRepoDirs(runmode.LocalDefaultOrgID, "octo", "api") {
		t.Error("DisposeRenamedRepoDirs = true with nothing on disk")
	}
}
