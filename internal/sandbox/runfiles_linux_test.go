//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// redirectSocketRoot points the per-run path derivations at a temp directory
// for the test's lifetime. The real root is /run/tf, which a non-root `go test`
// cannot create — the same reason trustedAgentHostSocketRoot is a var at all.
func redirectSocketRoot(t *testing.T) string {
	t.Helper()
	orig := trustedAgentHostSocketRoot
	trustedAgentHostSocketRoot = t.TempDir()
	t.Cleanup(func() { trustedAgentHostSocketRoot = orig })
	return trustedAgentHostSocketRoot
}

// seedCellFiles stages the full per-run family for runID: the agenthost
// socket, the gh-injector cert, and the tool-host socket directory with
// something inside it (a stale dir is never empty in production — it holds the
// previous jail's socket).
func seedCellFiles(t *testing.T, runID string) {
	t.Helper()
	for _, path := range []string{TrustedAgentHostSocketPath(runID), TrustedGHInjectorCertPath(runID)} {
		if err := os.WriteFile(path, []byte("stale"), 0o640); err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
	}
	dir := TrustedToolHostSocketDir(runID)
	if err := os.MkdirAll(dir, 0o770); err != nil {
		t.Fatalf("seed %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tools.sock"), []byte("stale"), 0o660); err != nil {
		t.Fatalf("seed tool host socket: %v", err)
	}
}

func mustNotExist(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("%s still exists (%s)", path, why)
	}
}

func mustExist(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("%s is gone (%s): %v", path, why, err)
	}
}

// TestClearRunCellFiles_RemovesThisRunsEntries pins the bring-up clear: every
// per-run entry a predecessor engagement of this run id could have left goes,
// and nothing keyed by another run id is touched. Unconditional by design —
// the successor cannot inspect what it is inheriting, and at most one claim per
// conversation is active, so anything under its own key is a predecessor's.
func TestClearRunCellFiles_RemovesThisRunsEntries(t *testing.T) {
	redirectSocketRoot(t)
	seedCellFiles(t, "run-a")
	seedCellFiles(t, "run-b")

	ClearRunCellFiles("run-a")

	mustNotExist(t, TrustedAgentHostSocketPath("run-a"), "the stale agenthost socket must be cleared before the new sidecar binds")
	mustNotExist(t, TrustedGHInjectorCertPath("run-a"), "the stale injector cert must be cleared before the new sidecar writes its own")
	mustNotExist(t, TrustedToolHostSocketDir("run-a"), "the stale tool-host socket dir must be cleared with the rest of the family")

	mustExist(t, TrustedAgentHostSocketPath("run-b"), "a sibling run's socket is not this run's to clear")
	mustExist(t, TrustedGHInjectorCertPath("run-b"), "a sibling run's cert is not this run's to clear")
	mustExist(t, TrustedToolHostSocketDir("run-b"), "a sibling run's tool-host dir is not this run's to clear")
}

// TestClearRunCellFiles_NothingToClear is the common case: a first engagement
// finds an empty root. It must not care.
func TestClearRunCellFiles_NothingToClear(t *testing.T) {
	root := redirectSocketRoot(t)
	ClearRunCellFiles("run-a")
	ClearRunCellFiles("run-a") // idempotent

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read socket root: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("socket root has %d entries after clearing an unused run id; want none", len(entries))
	}
}

// TestClearRunCellFiles_RemovesForeignOwnedEntries is the production shape: the
// files belong to the predecessor's sidecar uid, not to the process clearing
// them. That is exactly what the sidecar itself cannot do — the socket root is
// sticky, so unlink authority there is the file owner's and the DIRECTORY
// owner's, and consecutive engagements run their sidecars at different uids.
// The orchestrator has that authority (it owns the root in the split
// deployment, and is root in the one that does not split); this pins the root
// half of it.
func TestClearRunCellFiles_RemovesForeignOwnedEntries(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root to stage entries owned by a predecessor's sidecar uid")
	}
	root := redirectSocketRoot(t)
	// Mirror the real root's protection while we are at it: group-writable and
	// sticky, so the removal below is proven against the mode that makes the
	// sidecar's own attempt fail.
	if err := os.Chmod(root, os.ModeSticky|0o730); err != nil {
		t.Fatalf("chmod socket root sticky: %v", err)
	}
	seedCellFiles(t, "run-a")
	predecessorUID := SidecarUID(0)
	for _, path := range []string{TrustedAgentHostSocketPath("run-a"), TrustedGHInjectorCertPath("run-a")} {
		if err := os.Chown(path, predecessorUID, WorktreeGID); err != nil {
			t.Fatalf("chown %s to the predecessor sidecar uid: %v", path, err)
		}
	}

	ClearRunCellFiles("run-a")

	mustNotExist(t, TrustedAgentHostSocketPath("run-a"), "a predecessor-owned socket under a sticky root must still be cleared")
	mustNotExist(t, TrustedGHInjectorCertPath("run-a"), "a predecessor-owned cert under a sticky root must still be cleared")
}

// TestReleaseRunCellFiles_LeavesAnotherCellsEntries is the guard that keeps the
// teardown-side hygiene from becoming a worse bug than the one it tidies up
// after. A stop that queues a message re-claims within a scan tick, so this
// teardown routinely runs after its own successor has already re-created these
// paths — at a different subnet index, hence a different sidecar uid. Removing
// those would pull the cert out from under a live cell.
func TestReleaseRunCellFiles_LeavesAnotherCellsEntries(t *testing.T) {
	if IsSidecarUID(os.Getuid()) {
		t.Skip("this test process's uid falls inside the reserved sidecar band; the precondition (files NOT owned by the cell's sidecar uid) doesn't hold")
	}
	redirectSocketRoot(t)
	seedCellFiles(t, "run-a") // owned by the test process, not by SidecarUID(3)

	ReleaseRunCellFiles("run-a", 3)

	mustExist(t, TrustedAgentHostSocketPath("run-a"), "a socket owned by another cell's sidecar must survive this cell's teardown")
	mustExist(t, TrustedGHInjectorCertPath("run-a"), "a cert owned by another cell's sidecar must survive this cell's teardown")
}

// TestReleaseRunCellFiles_RemovesThisCellsEntries is the hygiene half: the
// files this cell's own sidecar created go at teardown, so a long-lived
// executor's tmpfs socket root does not carry an orphaned pair per run id it
// ever ran.
func TestReleaseRunCellFiles_RemovesThisCellsEntries(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root to stage entries owned by this cell's sidecar uid")
	}
	redirectSocketRoot(t)
	seedCellFiles(t, "run-a")
	const idx = 2
	for _, path := range []string{TrustedAgentHostSocketPath("run-a"), TrustedGHInjectorCertPath("run-a")} {
		if err := os.Chown(path, SidecarUID(idx), WorktreeGID); err != nil {
			t.Fatalf("chown %s to this cell's sidecar uid: %v", path, err)
		}
	}

	ReleaseRunCellFiles("run-a", idx)

	mustNotExist(t, TrustedAgentHostSocketPath("run-a"), "this cell's own socket must be removed at teardown")
	mustNotExist(t, TrustedGHInjectorCertPath("run-a"), "this cell's own cert must be removed at teardown")
	// The tool-host directory is not sidecar-owned and is reclaimed by its own
	// jail's Close; the uid-guarded release deliberately leaves it alone.
	mustExist(t, TrustedToolHostSocketDir("run-a"), "the tool-host dir is not part of the uid-guarded release")
}

// TestReleaseRunCellFiles_NothingToRelease: a graceful sidecar shutdown removes
// its own socket on the way out, so teardown routinely finds nothing.
func TestReleaseRunCellFiles_NothingToRelease(t *testing.T) {
	redirectSocketRoot(t)
	ReleaseRunCellFiles("run-a", 0)
}
