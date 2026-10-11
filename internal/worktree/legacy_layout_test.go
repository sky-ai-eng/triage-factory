package worktree

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// A bare is keyed by its repository's row id, so a bare of the slug-keyed
// layout (<repos>/<owner>/<repo>.git) is never resolved again and startup
// reclaims it — unless a checkout on disk still links into it, which keeps it
// until a later startup finds it unused. Bares of the current layout are not
// touched.
func TestCleanup_RemovesTheSlugKeyedLayout(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)
	current, err := EnsureBareClone(context.Background(), testRepo("octo", "api"), upstream)
	if err != nil {
		t.Fatalf("seed current bare: %v", err)
	}

	root := paths.BareCacheRoot(runmode.LocalDefaultOrgID)
	cold := filepath.Join(root, "octo", "api.git")
	warm := filepath.Join(root, "acme", "web.git")
	for _, dir := range []string{cold, warm} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir legacy bare: %v", err)
		}
	}
	// A cold registration (its checkout is gone) does not hold a bare; a
	// checkout still on disk does.
	registerWorktree(t, cold, "dead-run", filepath.Join(t.TempDir(), "gone"))
	live := t.TempDir()
	registerWorktree(t, warm, "warm-run", live)

	Cleanup()

	if _, err := os.Stat(cold); !os.IsNotExist(err) {
		t.Errorf("cold legacy bare still on disk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "octo")); !os.IsNotExist(err) {
		t.Errorf("emptied legacy owner directory still on disk: %v", err)
	}
	if _, err := os.Stat(warm); err != nil {
		t.Errorf("legacy bare a live checkout links into was removed: %v", err)
	}
	if _, err := os.Stat(current); err != nil {
		t.Errorf("current-layout bare was touched: %v", err)
	}
}
