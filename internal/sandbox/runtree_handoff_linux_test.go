//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRunTreeHandedOff pins the predicate callers use to suppress a write that
// the launch-time ownership hand-off has already made impossible: false while the
// tree is still the orchestrator's, true once the sandbox identity owns it.
func TestRunTreeHandedOff(t *testing.T) {
	dir := t.TempDir()

	if RunTreeHandedOff(dir) {
		t.Error("a freshly created, orchestrator-owned tree reports handed-off")
	}
	if RunTreeHandedOff("") || RunTreeHandedOff(filepath.Join(dir, "nope")) {
		t.Error("empty/missing paths must report false so callers keep their prior behavior")
	}

	if os.Geteuid() != 0 {
		t.Skip("changing a directory's owner needs root; the negative cases above still ran")
	}
	if err := os.Chown(dir, WorktreeUID, WorktreeGID); err != nil {
		t.Fatalf("chown to the sandbox identity: %v", err)
	}
	if !RunTreeHandedOff(dir) {
		t.Error("a tree owned by the sandbox uid does not report handed-off")
	}
}
