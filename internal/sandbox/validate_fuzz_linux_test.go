//go:build linux

package sandbox

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/githooks"
)

// The cap-broker RPC's whole security value equals the narrowness and
// correctness of these validators: they are the only thing between a
// compromised, unprivileged orchestrator and the privileged broker, which
// trusts whatever they wave through when it builds the OCI spec or touches
// the host. The property that must never regress is therefore
// "accepted ⇒ safe" — an input the validator ACCEPTS must still satisfy
// every invariant the broker then relies on. A wiring regression (a
// validate* call dropped, a new LaunchParams field added without a check,
// or an allowlist that grows a dangerous member) surfaces here as an input
// that is accepted yet violates an invariant. The invariants below are
// checked independently of the validators — some deliberately hardcode the
// safe set (e.g. mount options) rather than reading the package's own map,
// so a widened allowlist is caught instead of agreed with.
//
// See docs/security/security-overview.md §4.3.

// parseEgressForTest re-derives, from first principles, the network an
// accepted egress CIDR resolves to — matching validateEgressCIDR's bare-IP
// normalization but not calling it, so the overlap check below is a genuine
// second opinion rather than the same code.
func parseEgressForTest(cidr string) *net.IPNet {
	if !strings.Contains(cidr, "/") {
		ip := net.ParseIP(cidr)
		if ip == nil {
			return nil
		}
		if ip.To4() != nil {
			cidr += "/32"
		} else {
			cidr += "/128"
		}
	}
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil
	}
	return n
}

func assertLaunchParamsSafe(t *testing.T, p LaunchParams) {
	t.Helper()

	// Container id seeds the runsc container id, the per-run cgroup name, and
	// the broker's wait/kill key: never empty, never path-shaped.
	if p.ContainerID == "" || strings.ContainsAny(p.ContainerID, `/\`+"\x00") || strings.Contains(p.ContainerID, "..") {
		t.Fatalf("accepted an empty/path-shaped container id %q", p.ContainerID)
	}

	// Rootfs is resolved by catalog name, never a path — an accepted selector
	// must name a real catalog entry.
	if _, ok := rootfsCatalog[orDefaultRootfs(p.Rootfs.Name)]; !ok {
		t.Fatalf("accepted a rootfs selector %q not in the catalog", p.Rootfs.Name)
	}

	// The command is pinned: argv[0:2] is always the trusted tool-host binary
	// + its serve verb, so the orchestrator varies arguments but never the
	// executed program.
	if len(p.Args) < 2 || p.Args[0] != TrustedToolHostBinaryDestination || p.Args[1] != toolHostServeVerb {
		t.Fatalf("accepted argv %q whose entrypoint is not the pinned %q %q", p.Args, TrustedToolHostBinaryDestination, toolHostServeVerb)
	}

	// Worktree is a bind SOURCE the privileged broker mounts: clean, absolute,
	// NUL-free.
	if !filepath.IsAbs(p.Worktree) || p.Worktree != filepath.Clean(p.Worktree) || strings.IndexByte(p.Worktree, 0) >= 0 {
		t.Fatalf("accepted a relative/non-clean worktree %q", p.Worktree)
	}

	// Env: this reuses envKeyAllowed on purpose — it is a WIRING check (that
	// validateEnv still runs), NOT an allowlist-widening guard like the
	// hardcoded mount options above. A widened env-key allowlist is not a
	// security regression the way a dangerous mount option is: Property B is a
	// VALUE property (values are per-run placeholders, never real credentials —
	// the allowlist deliberately includes credential-NAMED keys like
	// AWS_SECRET_ACCESS_KEY with placeholder values), and env keys land in an
	// already-agent-controlled sandbox, so the key set is not an escalation
	// surface. The independently-dangerous key properties ARE asserted: no key
	// may be empty or carry '=' / NUL (which would misparse into the spec).
	for _, e := range p.Env {
		if e.Key == "" || strings.ContainsAny(e.Key, "=\x00") || !envKeyAllowed(e.Key) {
			t.Fatalf("accepted an off-allowlist/malformed env key %q", e.Key)
		}
	}

	// Mounts: clean absolute source+dest, options within {ro,rw}. The safe set
	// is hardcoded here on purpose — if allowedMountOptions ever grows a
	// dangerous member (dev/suid/exec/…) this catches it rather than agreeing.
	for _, m := range p.Mounts {
		if !filepath.IsAbs(m.Source) || m.Source != filepath.Clean(m.Source) {
			t.Fatalf("accepted a relative/non-clean mount source %q", m.Source)
		}
		if !filepath.IsAbs(m.Destination) || m.Destination != filepath.Clean(m.Destination) {
			t.Fatalf("accepted a relative/non-clean mount destination %q", m.Destination)
		}
		for _, o := range m.Options {
			if o != "ro" && o != "rw" {
				t.Fatalf("accepted mount %q with option %q outside {ro,rw}", m.Destination, o)
			}
		}
	}

	// Worktree + mount-source scoping: an accepted worktree must be either
	// this run's own ephemeral tree or an org-scoped state-root subtree, and
	// every mount's source must be either a broker-resolved global, this
	// run's own agenthost socket, or (only when the worktree carried an org
	// scope) under that same org prefix. Re-uses the real trusted-value
	// functions (RunTreeRoot / TrustedTFBinaryPath / TrustedGitHooksDir /
	// TrustedAgentHostSocketPath) rather than re-deriving them — same as the
	// netns check below reusing NetnsNameForRun — since these ARE the
	// broker's own resolutions, not a parallel implementation to agree with.
	orgPrefix, hasScope, scopeErr := worktreeScope(p.ConversationID, p.MemoryNamespace, p.Worktree)
	if scopeErr != nil {
		t.Fatalf("accepted worktree %q that fails its own scope re-check: %v", p.Worktree, scopeErr)
	}
	for _, m := range p.Mounts {
		// realPath, not the raw string: the production check resolves
		// symlinks before comparing (see realPath's doc), so an accepted
		// mount may lexically sit outside its trusted/org path yet still be
		// safe because it RESOLVES into it — re-deriving with the raw
		// string here would false-positive on exactly that case.
		realSource, srcErr := realPath(m.Source)
		if srcErr != nil {
			t.Fatalf("accepted mount %q source %q that fails its own real-path re-check: %v", m.Destination, m.Source, srcErr)
		}
		switch m.Destination {
		case TrustedTFBinaryDestination:
			trusted, tfErr := TrustedTFBinaryPath()
			if tfErr != nil {
				t.Fatalf("resolve trusted TF binary path: %v", tfErr)
			}
			realTrusted, rErr := realPath(trusted)
			if rErr != nil || realSource != realTrusted {
				t.Fatalf("accepted mount %q source %q not the trusted TF binary path", m.Destination, m.Source)
			}
		case githooks.SandboxDir:
			realTrusted, rErr := realPath(TrustedGitHooksDir())
			if rErr != nil || realSource != realTrusted {
				t.Fatalf("accepted mount %q source %q not the trusted git-hooks dir", m.Destination, m.Source)
			}
		case TrustedAgentHostSocketDestination:
			realTrusted, rErr := realPath(TrustedAgentHostSocketPath(p.ConversationID))
			if rErr != nil || realSource != realTrusted {
				t.Fatalf("accepted mount %q source %q not this run's own agenthost socket", m.Destination, m.Source)
			}
		default:
			if !hasScope {
				t.Fatalf("accepted mount %q source %q with no org scope to authorize it", m.Destination, m.Source)
			}
			rel, relErr := filepath.Rel(orgPrefix, realSource)
			if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				t.Fatalf("accepted mount %q source %q outside the run's own org tree %q", m.Destination, m.Source, orgPrefix)
			}
		}
	}

	// Rlimits: allowlisted type, soft <= hard, no repeats.
	seen := make(map[string]struct{}, len(p.Rlimits))
	for _, r := range p.Rlimits {
		if _, ok := allowedRlimitTypes[r.Type]; !ok {
			t.Fatalf("accepted a non-allowlisted rlimit type %q", r.Type)
		}
		if r.Soft > r.Hard {
			t.Fatalf("accepted rlimit %s soft %d > hard %d", r.Type, r.Soft, r.Hard)
		}
		if _, dup := seen[r.Type]; dup {
			t.Fatalf("accepted a duplicate rlimit type %q", r.Type)
		}
		seen[r.Type] = struct{}{}
	}

	// Netns: under the sandbox netns dir, tf-shaped, and bound to THIS run's
	// id — never the host netns (which would bypass the egress allowlist) and
	// never a sibling's namespace.
	dir, name := filepath.Split(p.NetnsPath)
	if filepath.Clean(dir) != "/var/run/netns" && filepath.Clean(dir) != "/run/netns" {
		t.Fatalf("accepted a netns path %q outside the sandbox netns dir", p.NetnsPath)
	}
	if idx, ok := subnetIdxFromNetnsName(name); !ok || name != NetnsNameForRun(p.ConversationID, idx) {
		t.Fatalf("accepted netns %q not derived from run id %q", name, p.ConversationID)
	}

	// ExtraEgressCIDR (if any): must parse AND not overlap the internal
	// denylist, re-derived here independently of validateEgressCIDR. An
	// accepted CIDR that doesn't parse is itself a validator bug (a divergence
	// from validateEgressCIDR's own net.ParseCIDR) — fail, never skip.
	if p.ExtraEgressCIDR != "" {
		reqNet := parseEgressForTest(p.ExtraEgressCIDR)
		if reqNet == nil {
			t.Fatalf("accepted ExtraEgressCIDR %q that does not parse as an IP/CIDR", p.ExtraEgressCIDR)
		}
		for _, deny := range internalEgressDenylist {
			if deny.Contains(reqNet.IP) || reqNet.Contains(deny.IP) {
				t.Fatalf("accepted egress CIDR %q overlapping denylist %s", p.ExtraEgressCIDR, deny)
			}
		}
	}
}

// FuzzValidateLaunchParams is the load-bearing one: it fuzzes the exact
// decode-then-validate path the broker runs (JSON → LaunchParams →
// ValidateLaunchParams) and asserts every accepted params is safe. It must
// never panic, and must never accept an input that violates an invariant
// the broker then trusts.
func FuzzValidateLaunchParams(f *testing.F) {
	// realPath now requires the seed's Worktree/Mounts sources to actually
	// exist (see realPath's doc), so materialize them here — the same
	// on-disk fixtures a real "run-1" launch would have by RPC time. Plain
	// files/dirs, not a live net.Listen: Go's fuzzing engine re-runs this
	// setup independently in each of several worker PROCESSES against the
	// SAME fixed path, and a second Listen on an already-bound socket path
	// would fail (realPath only needs the path to exist, not be dialable).
	if err := os.MkdirAll(RunTreeRoot("run-1"), 0o755); err != nil {
		f.Fatalf("mkdir seed worktree fixture: %v", err)
	}
	// Redirect the agenthost socket root to a per-process temp dir: the real
	// root is /run/tf, which only root (or whoever the container entrypoint
	// hands it to) can create — mirrors cmd/capbroker's brokerSocketPath
	// test redirection for the same reason.
	trustedAgentHostSocketRoot = f.TempDir()
	seedSocket := TrustedAgentHostSocketPath("run-1")
	if err := os.WriteFile(seedSocket, nil, 0o600); err != nil {
		f.Fatalf("create seed socket fixture: %v", err)
	}

	valid := LaunchParams{
		ContainerID:    "tf-run1frag-3",
		ConversationID: "run-1",
		Env: []EnvVar{
			{Key: "PATH", Value: "/usr/bin"},
			{Key: "ANTHROPIC_BASE_URL", Value: "http://127.0.0.1:9"},
		},
		Args:      []string{TrustedToolHostBinaryDestination, toolHostServeVerb, "--connect", "/run/tf-tools/tools.sock"},
		Worktree:  RunTreeRoot("run-1"),
		Mounts:    []Mount{{Source: seedSocket, Destination: TrustedAgentHostSocketDestination, Options: []string{"ro"}}},
		Rlimits:   []Rlimit{{Type: "RLIMIT_NOFILE", Soft: 1024, Hard: 4096}},
		NetnsPath: "/var/run/netns/" + NetnsNameForRun("run-1", 3),
	}
	if vb, err := json.Marshal(valid); err == nil {
		f.Add(vb)
	}
	// Adversarial seeds — each SHOULD be rejected; the fuzzer explores around
	// them. If one is ever accepted, the invariant check fails loudly.
	for _, s := range []string{
		`{}`,
		`{"ContainerID":"../evil","Args":["/usr/bin/node","/sdk/wrapper.mjs"],"Worktree":"/work","NetnsPath":"/var/run/netns/tf-a-1"}`,
		`{"ContainerID":"c","Args":["/bin/sh"],"Worktree":"/work"}`,
		`{"ContainerID":"c","Args":["/usr/bin/node","/sdk/wrapper.mjs"],"Worktree":"../../etc"}`,
		`{"ContainerID":"c","Args":["/usr/bin/node","/sdk/wrapper.mjs"],"Worktree":"/work","Env":[{"key":"EVIL","value":"x"}]}`,
		`{"ContainerID":"c","Args":["/usr/bin/node","/sdk/wrapper.mjs"],"Worktree":"/work","Mounts":[{"Source":"/a","Destination":"/b","Options":["exec"]}]}`,
		`{"ContainerID":"c","Args":["/usr/bin/node","/sdk/wrapper.mjs"],"Worktree":"/work","ExtraEgressCIDR":"169.254.169.254"}`,
		`{"ContainerID":"c","Args":["/usr/bin/node","/sdk/wrapper.mjs"],"Worktree":"/work","NetnsPath":"/proc/1/ns/net"}`,
	} {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var p LaunchParams
		if err := json.Unmarshal(data, &p); err != nil {
			return // not our surface — the broker decodes JSON before validating
		}
		if err := ValidateLaunchParams(p); err != nil {
			return // rejected — nothing to prove
		}
		assertLaunchParamsSafe(t, p)
	})
}

// FuzzValidateRunTreeShape guards the pure path-shape gate every privileged
// chown/remove runs before touching a run tree: an accepted path must be
// absolute, already-clean, and at least two levels deep (never "/" or a
// top-level directory a compromised orchestrator could aim a recursive
// chown/rm at).
func FuzzValidateRunTreeShape(f *testing.F) {
	for _, s := range []string{
		"/data/runs/abc", "/", "/etc", "../../etc", "/a/../b",
		"/data//x", "relative/x", "/data/runs/../../etc", "", "/x",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, path string) {
		if err := validateRunTreeShape("fuzz", path); err != nil {
			return
		}
		if !filepath.IsAbs(path) || path != filepath.Clean(path) {
			t.Fatalf("accepted a relative/non-clean run-tree path %q", path)
		}
		if path == "/" || filepath.Dir(path) == "/" {
			t.Fatalf("accepted a too-shallow run-tree path %q", path)
		}
	})
}

// FuzzValidateEgressCIDR guards the egress denylist directly (better
// coverage of the CIDR parser than reaching it through a whole LaunchParams):
// any accepted CIDR must parse AND must not overlap the internal denylist.
func FuzzValidateEgressCIDR(f *testing.F) {
	for _, s := range []string{
		"", "203.0.113.0/24", "169.254.169.254", "10.0.0.1", "192.168.1.0/24",
		"::1", "fd00::/8", "8.8.8.8/32", "2001:db8::/32", "not-a-cidr",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, cidr string) {
		if err := validateEgressCIDR(cidr); err != nil || cidr == "" {
			return
		}
		reqNet := parseEgressForTest(cidr)
		if reqNet == nil {
			t.Fatalf("accepted %q but it does not parse as an IP/CIDR", cidr)
		}
		for _, deny := range internalEgressDenylist {
			if deny.Contains(reqNet.IP) || reqNet.Contains(deny.IP) {
				t.Fatalf("accepted egress CIDR %q overlapping denylist %s", cidr, deny)
			}
		}
	})
}

// TestValidateEgressCIDR_KnownDangerousAlwaysRejected is the concrete
// denylist-completeness guard: the specific ranges a permit must never touch
// (cloud metadata endpoint, RFC1918, the sandbox subnet pool, loopback, and
// the v6 loopback/ULA/link-local space) each stay rejected. A future edit
// that drops a denylist entry fails here rather than in production.
func TestValidateEgressCIDR_KnownDangerousAlwaysRejected(t *testing.T) {
	mustReject := []string{
		"169.254.169.254", "169.254.169.254/32", "169.254.0.0/16", // cloud metadata + link-local
		"10.0.0.0/8", "10.42.0.7", "10.5.6.7/24", // RFC1918 + sandbox pool + control-plane space
		"172.16.5.0/24", "192.168.1.1", // RFC1918
		"127.0.0.1", "127.0.0.0/8", // loopback
		"::1", "fc00::1", "fd12:3456::/48", "fe80::1", // v6 loopback / ULA / link-local
	}
	for _, c := range mustReject {
		if err := validateEgressCIDR(c); err == nil {
			t.Errorf("expected egress CIDR %q to be rejected by the denylist, but it was accepted", c)
		}
	}
}
