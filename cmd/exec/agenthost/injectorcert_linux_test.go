//go:build linux

package agenthost

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWriteFreshInjectorCert_ReplacesStaleFile pins the remove-before-create
// rule on the per-conversation gh-injector cert. The path is keyed by
// conversation id, so a
// second engagement of one conversation writes exactly where the first one
// did; without the remove the new sidecar inherits the old file rather than
// owning its own.
//
// The mode is the observable, and it is exact: open(O_CREATE) applies its
// permission argument only when it CREATES the file, so a stale file staged at
// 0600 stays 0600 through a truncate-in-place and comes back 0640 only if it
// was unlinked first. (The inode number cannot stand in for this — filesystems
// hand the just-freed inode straight back to the next create.)
func TestWriteFreshInjectorCert_ReplacesStaleFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conv-1-gh-injector.crt")
	if err := os.WriteFile(path, []byte("STALE PREDECESSOR PEM"), 0o600); err != nil {
		t.Fatalf("seed stale cert: %v", err)
	}

	if err := writeFreshInjectorCert(path, []byte("FRESH PEM")); err != nil {
		t.Fatalf("writeFreshInjectorCert over a stale file: %v", err)
	}

	if mode := modeOf(t, path); mode != 0o640 {
		t.Errorf("cert mode = %#o, want 0640 — the stale file's 0600 survived, so it was truncated in place rather than recreated", mode)
	}
	if got := readFile(t, path); got != "FRESH PEM" {
		t.Errorf("cert = %q, want the freshly written PEM", got)
	}
}

// TestWriteFreshInjectorCert_ReplacesUnwritableFile is the shape the live
// failure took: the file already at the path is one the writer may not open
// for writing, so a truncate-in-place fails outright and only an unlink gets
// past it. Staged as a mode the writing uid cannot write through rather than
// as a foreign owner, because staging a foreign owner needs root and this
// property does not.
func TestWriteFreshInjectorCert_ReplacesUnwritableFile(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses the file mode this case turns on; the mode assertion above covers the rule for every uid")
	}
	path := filepath.Join(t.TempDir(), "conv-2-gh-injector.crt")
	if err := os.WriteFile(path, []byte("STALE"), 0o440); err != nil {
		t.Fatalf("seed stale cert: %v", err)
	}
	if err := os.WriteFile(path, []byte("nope"), 0o640); err == nil {
		t.Fatal("precondition failed: a plain write over the staged file succeeded, so this case proves nothing")
	}

	if err := writeFreshInjectorCert(path, []byte("FRESH PEM")); err != nil {
		t.Fatalf("writeFreshInjectorCert over an unwritable stale file: %v", err)
	}
	if got := readFile(t, path); got != "FRESH PEM" {
		t.Errorf("cert = %q, want the freshly written PEM", got)
	}
}

// TestWriteFreshInjectorCert_NoStaleFile covers the ordinary case — nothing to
// remove — so the remove-first cannot have made a first write conditional on
// one having happened before.
func TestWriteFreshInjectorCert_NoStaleFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conv-3-gh-injector.crt")
	if err := writeFreshInjectorCert(path, []byte("FRESH PEM")); err != nil {
		t.Fatalf("writeFreshInjectorCert on a clean path: %v", err)
	}
	if got := readFile(t, path); got != "FRESH PEM" {
		t.Errorf("cert = %q, want the freshly written PEM", got)
	}
	if mode := modeOf(t, path); mode != 0o640 {
		t.Errorf("cert mode = %#o, want 0640", mode)
	}
}

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
