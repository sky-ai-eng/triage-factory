//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRunTreeHandedOff pins the predicate callers use to suppress a write that
// the launch-time ownership hand-off has already made impossible: false while the
// tree is still the orchestrator's, true once a jail identity owns it — and still
// true once that engagement has ended and parked it.
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
	if err := os.Chown(dir, JailUID(4), JailGID(4)); err != nil {
		t.Fatalf("chown to a jail identity: %v", err)
	}
	if !RunTreeHandedOff(dir) {
		t.Error("a tree owned by a jail uid does not report handed-off")
	}
	if uid, ok := RunTreeJailUID(dir); !ok || uid != JailUID(4) {
		t.Errorf("RunTreeJailUID = %d, %v; want %d, true", uid, ok, JailUID(4))
	}
	if err := os.Chown(dir, ParkedRunTreeUID, os.Getgid()); err != nil {
		t.Fatalf("chown to the parked owner: %v", err)
	}
	if !RunTreeHandedOff(dir) {
		t.Error("a parked tree does not report handed-off; TF would write into a tree an agent has been in")
	}
	if _, ok := RunTreeJailUID(dir); ok {
		t.Error("a parked tree reports a jail identity holding it")
	}
}
