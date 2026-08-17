//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// stageSkillsFixture materializes a staging dir at the path the broker derives
// for conversationID, the way the orchestrator does before issuing the launch RPC.
func stageSkillsFixture(t *testing.T, conversationID string) string {
	t.Helper()
	dir := TrustedSkillsSourcePath(conversationID)
	if err := os.MkdirAll(filepath.Join(dir, "chain-step-1-x"), 0o755); err != nil {
		t.Fatalf("stage skills fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "chain-step-1-x", "SKILL.md"), []byte("---\nname: chain-step-1-x\n---\n"), 0o644); err != nil {
		t.Fatalf("write staged SKILL.md: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func withSkillsMount(p LaunchParams, source string, options ...string) LaunchParams {
	p.Mounts = append(p.Mounts, Mount{Source: source, Destination: TrustedSkillsDestination, Options: options})
	return p
}

// TestValidateLaunchParams_AcceptsOwnSkillsMount: the happy path — a read-only
// mount of THIS run's own step-skill staging dir at the fixed rootfs destination.
func TestValidateLaunchParams_AcceptsOwnSkillsMount(t *testing.T) {
	p := validParams()
	dir := stageSkillsFixture(t, p.ConversationID)
	if err := ValidateLaunchParams(withSkillsMount(p, dir, "ro")); err != nil {
		t.Fatalf("rejected this run's own read-only skills mount: %v", err)
	}
}

// TestValidateLaunchParams_RejectsWritableSkillsMount: the mount exists so the
// jail can READ a skill TF authored. Writable would hand the agent a channel
// into orchestrator-owned state outside its run tree, so "ro" is required, not
// merely permitted — a bare mount with no options is rejected too.
func TestValidateLaunchParams_RejectsWritableSkillsMount(t *testing.T) {
	p := validParams()
	dir := stageSkillsFixture(t, p.ConversationID)
	for _, opts := range [][]string{{"rw"}, {"ro", "rw"}, nil} {
		if err := ValidateLaunchParams(withSkillsMount(p, dir, opts...)); err == nil {
			t.Errorf("accepted a skills mount with options %v; want rejection", opts)
		}
	}
}

// TestValidateLaunchParams_RejectsForeignSkillsSource is the validate-not-override
// half: a source that is shape-valid and even genuinely a staging dir — just
// another run's — is rejected, so one step can never be handed a sibling's skill.
// An arbitrary host path is rejected the same way.
func TestValidateLaunchParams_RejectsForeignSkillsSource(t *testing.T) {
	p := validParams()
	stageSkillsFixture(t, p.ConversationID) // this run's own dir exists; the mount still claims another
	foreign := stageSkillsFixture(t, "some-other-run")

	for name, source := range map[string]string{
		"another runs staging dir": foreign,
		"arbitrary host path":      t.TempDir(),
		"relative path":            "triagefactory-skills/" + p.ConversationID,
		"traversal":                filepath.Join(SkillStagingBase(), p.ConversationID, "..", "some-other-run"),
	} {
		if err := ValidateLaunchParams(withSkillsMount(p, source, "ro")); err == nil {
			t.Errorf("accepted skills mount source (%s) %q; want rejection", name, source)
		}
	}
}

// TestValidateLaunchParams_RejectsSymlinkedSkillsSource: the source is compared
// after symlink resolution, so a lexically-correct path that actually resolves
// somewhere else cannot smuggle a foreign directory into the jail.
func TestValidateLaunchParams_RejectsSymlinkedSkillsSource(t *testing.T) {
	p := validParams()
	elsewhere := stageSkillsFixture(t, "victim-run")

	own := TrustedSkillsSourcePath(p.ConversationID)
	if err := os.MkdirAll(filepath.Dir(own), 0o755); err != nil {
		t.Fatalf("mkdir staging base: %v", err)
	}
	_ = os.RemoveAll(own)
	if err := os.Symlink(elsewhere, own); err != nil {
		t.Fatalf("symlink own staging path at another run's dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(own) })

	if err := ValidateLaunchParams(withSkillsMount(p, own, "ro")); err == nil {
		t.Fatal("accepted a skills mount whose source symlinks to another run's staging dir; want rejection")
	}
}

// TestValidateLaunchParams_RejectsMissingSkillsSource: the staging dir must exist
// at validation time (every real launch stages it before the RPC), so a
// nonexistent source fails at the boundary rather than late inside runsc.
func TestValidateLaunchParams_RejectsMissingSkillsSource(t *testing.T) {
	p := validParams()
	if err := ValidateLaunchParams(withSkillsMount(p, TrustedSkillsSourcePath(p.ConversationID), "ro")); err == nil {
		t.Fatal("accepted a skills mount with no staging dir on disk; want rejection")
	}
}

// TestValidateLaunchParams_RejectsSkillsMountWithoutRunID: the run id is what
// scopes the staging dir to one step. Without it the derivation would collapse to
// the shared staging base — every run's skills at once — so the mount is refused.
func TestValidateLaunchParams_RejectsSkillsMountWithoutRunID(t *testing.T) {
	p := validParams()
	stageSkillsFixture(t, p.ConversationID)
	base := SkillStagingBase()
	p.MemoryNamespace = p.ConversationID // keeps the worktree scope check satisfied
	p.ConversationID = ""
	if err := ValidateLaunchParams(withSkillsMount(p, base, "ro")); err == nil {
		t.Fatal("accepted a skills mount of the shared staging base with no run id; want rejection")
	}
}

// TestTrustedSkillsPaths_Shape pins the two literals the mount contract rests on:
// a fixed rootfs destination outside /work (an in-worktree destination could be
// redirected by an agent-planted symlink, since the tree is agent-authored
// between steps) and a per-run staging path under the temp namespace.
func TestTrustedSkillsPaths_Shape(t *testing.T) {
	if TrustedSkillsDestination != "/opt/tf/skills" {
		t.Errorf("TrustedSkillsDestination = %q, want /opt/tf/skills", TrustedSkillsDestination)
	}
	if got := TrustedSkillsSourcePath("run-1"); got != filepath.Join(os.TempDir(), "triagefactory-skills", "run-1") {
		t.Errorf("TrustedSkillsSourcePath = %q", got)
	}
	if base, dest := SkillStagingBase(), TrustedSkillsDestination; filepath.Dir(dest) == base {
		t.Errorf("destination %q must not live under the host staging base %q", dest, base)
	}
}
