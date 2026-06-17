package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// reapOrphans is the cross-platform entry point that ReapOrphans
// dispatches to. Linux walks /var/run/netns and $TMPDIR; non-Linux
// is a no-op (the netns concept doesn't exist).
func reapOrphans(ctx context.Context) error {
	return reapOrphansImpl(ctx)
}

// reapBundleOrphans is the shared cross-platform half of orphan
// cleanup: walks $TMPDIR for tf-bundle-* directories and removes
// them. Safe on every platform.
//
// Called from reapOrphansImpl on Linux (after netns cleanup) and
// could be called from a non-Linux no-op Wrap path in the future.
// In SKY-254 it only runs on Linux because reapOrphansImpl is
// a no-op on other platforms (see reaper_other.go).
func reapBundleOrphans(_ context.Context) error {
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "tf-bundle-") {
			continue
		}
		path := filepath.Join(os.TempDir(), e.Name())
		if err := os.RemoveAll(path); err != nil {
			sandboxLog.Warn("reap bundle failed", "path", path, "error", err)
		}
	}
	return nil
}
