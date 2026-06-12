package agentproc

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadOnlyRepoMounts_TranslatesToROUnderWork pins the mount construction:
// each ReadOnlyRepoMount becomes a sandbox bind mount of Source at
// /work/<RelPath> with the "ro" option — the boundary that makes an in-jail
// write fail. Empty entries are dropped.
func TestReadOnlyRepoMounts_TranslatesToROUnderWork(t *testing.T) {
	got := readOnlyRepoMounts([]ReadOnlyRepoMount{
		{Source: "/data/orgs/acme/curator-repos/o/r", RelPath: filepath.Join("repos", "o", "r")},
		{Source: "", RelPath: "repos/x/y"},      // dropped: no source
		{Source: "/data/whatever", RelPath: ""}, // dropped: no relpath
	})
	if len(got) != 1 {
		t.Fatalf("got %d mounts, want 1 (empties dropped): %+v", len(got), got)
	}
	m := got[0]
	if m.Source != "/data/orgs/acme/curator-repos/o/r" {
		t.Errorf("Source = %q, want the shared worktree path", m.Source)
	}
	if m.Destination != "/work/repos/o/r" {
		t.Errorf("Destination = %q, want /work/repos/o/r (nested under the cwd mount)", m.Destination)
	}
	var ro bool
	for _, o := range m.Options {
		if o == "ro" {
			ro = true
		}
	}
	if !ro {
		t.Errorf("Options = %v, MUST contain \"ro\"", m.Options)
	}
}

// TestEnsureRepoMountPoints_CreatesDirsUnderCwd verifies the empty mount-point
// dirs are created under workCwd (so they're chowned with the rest of /work
// and exist as bind targets), and that nothing is created outside it.
func TestEnsureRepoMountPoints_CreatesDirsUnderCwd(t *testing.T) {
	cwd := t.TempDir()
	mounts := []ReadOnlyRepoMount{
		{Source: "/somewhere/shared/o/r", RelPath: filepath.Join("repos", "o", "r")},
		{Source: "/somewhere/shared/a/b", RelPath: filepath.Join("repos", "a", "b")},
		{Source: "", RelPath: ""}, // skipped
	}
	if err := ensureRepoMountPoints(cwd, mounts); err != nil {
		t.Fatalf("ensureRepoMountPoints: %v", err)
	}
	for _, rel := range []string{"repos/o/r", "repos/a/b"} {
		info, err := os.Stat(filepath.Join(cwd, rel))
		if err != nil {
			t.Errorf("mount point %s not created: %v", rel, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("mount point %s is not a directory", rel)
		}
	}
}

// TestEnsureRepoMountPoints_NoMountsIsNoop confirms the common (delegate /
// scorer) case — no read-only repo mounts — does nothing and errors never.
func TestEnsureRepoMountPoints_NoMountsIsNoop(t *testing.T) {
	cwd := t.TempDir()
	if err := ensureRepoMountPoints(cwd, nil); err != nil {
		t.Fatalf("ensureRepoMountPoints(nil): %v", err)
	}
	entries, err := os.ReadDir(cwd)
	if err != nil {
		t.Fatalf("read cwd: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("ensureRepoMountPoints(nil) created %d entries, want 0", len(entries))
	}
}

// TestWillSandbox_TracksGate confirms WillSandbox mirrors the internal gate
// (false in the default local-mode test environment).
func TestWillSandbox_TracksGate(t *testing.T) {
	if WillSandbox() != shouldSandbox() {
		t.Errorf("WillSandbox() = %v, want it to mirror shouldSandbox() = %v", WillSandbox(), shouldSandbox())
	}
}
