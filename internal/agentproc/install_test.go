package agentproc

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSDKAlreadyInstalled_HitsPinnedVersion pins the load-bearing
// short-circuit: when node_modules already contains the pinned SDK
// version, doInstall must return without invoking checkNode/checkNpm.
// This is what makes the Docker image's pre-installed SDK work in a
// runtime layer with no node binary on PATH.
func TestSDKAlreadyInstalled_HitsPinnedVersion(t *testing.T) {
	sdkDir := t.TempDir()
	pkgDir := filepath.Join(sdkDir, "node_modules", "@anthropic-ai", "claude-agent-sdk")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pkgJSON := []byte(`{"name":"@anthropic-ai/claude-agent-sdk","version":"` + sdkVersion + `"}`)
	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), pkgJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	if !sdkAlreadyInstalled(sdkDir) {
		t.Error("expected sdkAlreadyInstalled=true for the pinned version, got false")
	}
}

// TestSDKAlreadyInstalled_MissingReturnsFalse pins the negative case:
// an empty sdk dir must return false so doInstall falls through to
// the node/npm checks + npm ci path.
func TestSDKAlreadyInstalled_MissingReturnsFalse(t *testing.T) {
	sdkDir := t.TempDir()
	if sdkAlreadyInstalled(sdkDir) {
		t.Error("expected false for empty sdk dir, got true")
	}
}

// TestSDKAlreadyInstalled_WrongVersionReturnsFalse pins the version-
// gating part of the check: a stale install from a previous TF
// version must NOT short-circuit, so a sdkVersion bump triggers a
// re-install via the existing installSDK path.
func TestSDKAlreadyInstalled_WrongVersionReturnsFalse(t *testing.T) {
	sdkDir := t.TempDir()
	pkgDir := filepath.Join(sdkDir, "node_modules", "@anthropic-ai", "claude-agent-sdk")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pkgJSON := []byte(`{"name":"@anthropic-ai/claude-agent-sdk","version":"0.0.1-stale"}`)
	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), pkgJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	if sdkAlreadyInstalled(sdkDir) {
		t.Error("expected false for stale version, got true (would skip needed re-install)")
	}
}

// writePinnedSDK populates sdkDir's node_modules with the pinned SDK
// version, the on-disk shape that makes sdkAlreadyInstalled report true.
func writePinnedSDK(t *testing.T, sdkDir string) {
	t.Helper()
	pkgDir := filepath.Join(sdkDir, "node_modules", "@anthropic-ai", "claude-agent-sdk")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pkgJSON := []byte(`{"name":"@anthropic-ai/claude-agent-sdk","version":"` + sdkVersion + `"}`)
	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), pkgJSON, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestNeedsInstall_LockChangeOverridesPresentSDK is the regression guard
// for a transitive dependency bump that leaves sdkVersion alone — the
// shape every npm security patch to this tree takes. The pinned SDK is
// present, so the version-only check is satisfied and would short-circuit;
// the rewritten lockfile has to override it, or an upgraded install keeps
// running the superseded node_modules the bump was meant to replace.
func TestNeedsInstall_LockChangeOverridesPresentSDK(t *testing.T) {
	sdkDir := t.TempDir()
	writePinnedSDK(t, sdkDir)

	if !needsInstall(sdkDir, true) {
		t.Error("expected needsInstall=true when the lockfile was rewritten, got false (would keep a stale dependency tree)")
	}
}

// TestNeedsInstall_UnchangedLockSkipsPresentSDK pins the other side of
// that gate, which is load-bearing for the Docker image: the baked SDK dir
// matches the embedded files byte-for-byte, so nothing is rewritten and
// the install must be skipped entirely — the runtime layer has no node or
// npm on PATH to satisfy the checks that follow.
func TestNeedsInstall_UnchangedLockSkipsPresentSDK(t *testing.T) {
	sdkDir := t.TempDir()
	writePinnedSDK(t, sdkDir)

	if needsInstall(sdkDir, false) {
		t.Error("expected needsInstall=false for an unchanged lockfile with the pinned SDK present, got true (would demand node/npm in the runtime image)")
	}
}

// TestNeedsInstall_MissingSDKInstallsRegardlessOfLock covers a first run
// on a fresh machine, where the lockfile is written for the first time,
// and the pathological case of node_modules deleted out from under an
// otherwise up-to-date SDK dir.
func TestNeedsInstall_MissingSDKInstallsRegardlessOfLock(t *testing.T) {
	for _, lockChanged := range []bool{true, false} {
		if !needsInstall(t.TempDir(), lockChanged) {
			t.Errorf("lockChanged=%v: expected needsInstall=true for an empty sdk dir, got false", lockChanged)
		}
	}
}

// TestWriteFileIfChanged_ReportsWhetherItWrote pins the signal the
// lockfile gate is built on. The false-on-match case is also what spares
// the read-only baked SDK dir in the Docker image an EACCES rewrite.
func TestWriteFileIfChanged_ReportsWhetherItWrote(t *testing.T) {
	path := filepath.Join(t.TempDir(), "package-lock.json")
	content := []byte(`{"lockfileVersion":3}`)

	wrote, err := writeFileIfChanged(path, content, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Error("first write: expected wrote=true, got false")
	}

	wrote, err = writeFileIfChanged(path, content, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if wrote {
		t.Error("identical rewrite: expected wrote=false, got true")
	}

	wrote, err = writeFileIfChanged(path, []byte(`{"lockfileVersion":3,"bumped":true}`), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Error("changed content: expected wrote=true, got false")
	}
}
