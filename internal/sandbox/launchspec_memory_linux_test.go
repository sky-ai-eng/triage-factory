//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// stageMemoryFixture materializes a memory staging dir at the path the broker
// derives for runID, the way the orchestrator does before issuing the launch RPC.
func stageMemoryFixture(t *testing.T, runID string) string {
	t.Helper()
	dir := TrustedMemorySourcePath(runID)
	if err := os.MkdirAll(filepath.Join(dir, "this-run"), 0o755); err != nil {
		t.Fatalf("stage memory fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "this-run", "01-triage.md"), []byte("what step 1 decided\n"), 0o644); err != nil {
		t.Fatalf("write staged memory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func withMemoryMount(p LaunchParams, source string, options ...string) LaunchParams {
	p.Mounts = append(p.Mounts, Mount{Source: source, Destination: TrustedMemoryDestination, Options: options})
	return p
}

// TestValidateLaunchParams_AcceptsOwnMemoryMount: the happy path — a read-only
// mount of THIS launch's own materialized memory tree at the fixed destination.
func TestValidateLaunchParams_AcceptsOwnMemoryMount(t *testing.T) {
	p := validParams()
	dir := stageMemoryFixture(t, p.RunID)
	if err := ValidateLaunchParams(withMemoryMount(p, dir, "ro")); err != nil {
		t.Fatalf("rejected this run's own read-only memory mount: %v", err)
	}
}

// TestValidateLaunchParams_RejectsWritableMemoryMount: the mount exists so the
// jail can READ a tree TF rendered from the database. Writable would hand the
// agent orchestrator-owned state outside its run tree — and would let one step
// forge the handoff a later step reads as fact — so "ro" is required, not merely
// permitted.
func TestValidateLaunchParams_RejectsWritableMemoryMount(t *testing.T) {
	p := validParams()
	dir := stageMemoryFixture(t, p.RunID)
	for _, opts := range [][]string{{"rw"}, {"ro", "rw"}, nil} {
		if err := ValidateLaunchParams(withMemoryMount(p, dir, opts...)); err == nil {
			t.Errorf("accepted a memory mount with options %v; want rejection", opts)
		}
	}
}

// TestValidateLaunchParams_RejectsForeignMemorySource is the validate-not-override
// half: a source that is shape-valid and even genuinely a staging dir — just
// another step's — is rejected. That is what keeps "each step reads the tree
// rendered for it" true at the boundary rather than only by convention.
func TestValidateLaunchParams_RejectsForeignMemorySource(t *testing.T) {
	p := validParams()
	stageMemoryFixture(t, p.RunID) // this run's own dir exists; the mount still claims another
	foreign := stageMemoryFixture(t, "some-other-run")

	for name, source := range map[string]string{
		"another runs staging dir": foreign,
		"arbitrary host path":      t.TempDir(),
		"relative path":            "triagefactory-memory/" + p.RunID,
		"traversal":                filepath.Join(MemoryStagingBase(), p.RunID, "..", "some-other-run"),
	} {
		if err := ValidateLaunchParams(withMemoryMount(p, source, "ro")); err == nil {
			t.Errorf("accepted memory mount source (%s) %q; want rejection", name, source)
		}
	}
}

// TestValidateLaunchParams_RejectsSymlinkedMemorySource: the base is resolved and
// the per-run component re-derived, so a lexically-correct path that actually
// resolves elsewhere cannot smuggle a foreign directory into the jail.
func TestValidateLaunchParams_RejectsSymlinkedMemorySource(t *testing.T) {
	p := validParams()
	elsewhere := stageMemoryFixture(t, "victim-run")

	own := TrustedMemorySourcePath(p.RunID)
	if err := os.MkdirAll(filepath.Dir(own), 0o755); err != nil {
		t.Fatalf("mkdir staging base: %v", err)
	}
	_ = os.RemoveAll(own)
	if err := os.Symlink(elsewhere, own); err != nil {
		t.Fatalf("symlink own staging path at another run's dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(own) })

	if err := ValidateLaunchParams(withMemoryMount(p, own, "ro")); err == nil {
		t.Fatal("accepted a memory mount whose source symlinks to another run's staging dir; want rejection")
	}
}

// TestValidateLaunchParams_RejectsMissingMemorySource: agentproc omits the mount
// entirely when the staging dir is gone, so a launch that still claims one is
// inconsistent and fails at the boundary rather than late inside runsc.
func TestValidateLaunchParams_RejectsMissingMemorySource(t *testing.T) {
	p := validParams()
	if err := ValidateLaunchParams(withMemoryMount(p, TrustedMemorySourcePath(p.RunID), "ro")); err == nil {
		t.Fatal("accepted a memory mount with no staging dir on disk; want rejection")
	}
}

// TestValidateLaunchParams_RejectsMemoryMountWithoutRunID: the run id is what
// scopes the staging dir to one launch. Without it the derivation would collapse
// to the shared staging base — every run's memory at once — so it is refused.
func TestValidateLaunchParams_RejectsMemoryMountWithoutRunID(t *testing.T) {
	p := validParams()
	stageMemoryFixture(t, p.RunID)
	base := MemoryStagingBase()
	p.MemoryNamespace = p.RunID // keeps the worktree scope check satisfied
	p.RunID = ""
	if err := ValidateLaunchParams(withMemoryMount(p, base, "ro")); err == nil {
		t.Fatal("accepted a memory mount of the shared staging base with no run id; want rejection")
	}
}

// TestTrustedMemoryPaths_Shape pins the two literals the mount contract rests on:
// a fixed rootfs destination outside /work (an in-worktree destination could be
// redirected by an agent-planted symlink, since the tree is agent-authored
// between steps) and a per-run staging path under the temp namespace, distinct
// from the step-skill one.
func TestTrustedMemoryPaths_Shape(t *testing.T) {
	if TrustedMemoryDestination != "/opt/tf/memory" {
		t.Errorf("TrustedMemoryDestination = %q, want /opt/tf/memory", TrustedMemoryDestination)
	}
	if got := TrustedMemorySourcePath("run-1"); got != filepath.Join(os.TempDir(), "triagefactory-memory", "run-1") {
		t.Errorf("TrustedMemorySourcePath = %q", got)
	}
	if base, dest := MemoryStagingBase(), TrustedMemoryDestination; filepath.Dir(dest) == base {
		t.Errorf("destination %q must not live under the host staging base %q", dest, base)
	}
	if MemoryStagingBase() == SkillStagingBase() {
		t.Error("memory and step-skill staging must not share a base; one sweep key would collide the other's dirs")
	}
	if TrustedMemoryDestination == TrustedSkillsDestination {
		t.Error("memory and step-skill mounts must not share a destination")
	}
}
