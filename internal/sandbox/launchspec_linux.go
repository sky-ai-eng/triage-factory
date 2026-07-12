//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// TrustedSDKDir is the broker-owned agent-SDK install directory (where
// EnsureSDK writes wrapper.mjs + node_modules). The broker resolves this
// itself instead of trusting the SDKDir a caller sends over the RPC:
// buildSpec bind-mounts /sdk from here and the pinned argv execs
// /sdk/wrapper.mjs, so if the source were caller-supplied a compromised
// orchestrator could point it at an attacker-authored wrapper and the
// pinned entrypoint would run attacker code. Resolving it broker-side is
// what makes "the broker owns the command" actually true — same principle
// as resolving the rootfs by catalog name rather than accepting a path.
func TrustedSDKDir() string {
	return paths.SDKDir()
}

// This file is the load-bearing narrowing of the broker's attack surface:
// the broker OWNS the OCI spec (built from a fixed template — empty
// capabilities, uid/gid 10000, seccomp, the namespace set, a
// content-addressed read-only rootfs) and accepts over its RPC only the
// narrow, validated DATA in LaunchParams. The broker is the only launch
// path, so ValidateLaunchParams is unconditional: it is the gate every
// launch passes before the broker builds or execs anything, so a
// compromised (unprivileged) orchestrator can inject data the sandbox sees
// but can never make the broker run arbitrary code with capabilities.

// --- rootfs catalog (name → recipe → hash) ---
//
// The orchestrator names a variant; the broker resolves it against a
// catalog IT owns and mounts the result read-only by content hash. It
// never accepts a rootfs path. v1 is curated ("base"); additional named
// variants ("browser", org-authored recipes) are later catalog rows built
// by an isolated builder — never `apk add <input>` in the broker.

// rootfsVariant is one curated catalog entry. resolve returns the on-disk
// path of the content-addressed rootfs (baking it if the cache is cold).
type rootfsVariant struct {
	name    string
	resolve func(ctx context.Context) (path string, err error)
}

// rootfsCatalog is the broker's curated variant set. "base" is today's
// toolchain rootfs; its recipe (alpine sha + apkPackages + pnpm) resolves
// to a content hash via rootfsCacheKey, exactly the name→recipe→hash
// contract the sandbox-fleet image dimension consumes.
var rootfsCatalog = map[string]rootfsVariant{
	"base": {name: "base", resolve: ensureRootfs},
}

// defaultRootfsName is the variant an empty selector resolves to.
const defaultRootfsName = "base"

// resolveCatalogRootfs validates the selector against the catalog and
// returns the resolved rootfs path. An empty Name means "base"; any other
// name must be a known catalog entry. A path-shaped or unknown name is a
// hard rejection — the broker never mounts an orchestrator-supplied path.
func resolveCatalogRootfs(ctx context.Context, sel RootfsSelector) (string, error) {
	name, err := validateRootfsName(sel.Name)
	if err != nil {
		return "", err
	}
	v, ok := rootfsCatalog[name]
	if !ok {
		return "", fmt.Errorf("sandbox: unknown rootfs variant %q (catalog: %s)", name, catalogNames())
	}
	return v.resolve(ctx)
}

// validateRootfsName normalizes and rejects a rootfs selector name. Empty
// → the default. A name carrying any path structure (slash, backslash,
// "..", NUL, leading dot) is rejected before the catalog lookup so a
// path-shaped selector can never masquerade as a variant name.
func validateRootfsName(name string) (string, error) {
	if name == "" {
		return defaultRootfsName, nil
	}
	if len(name) > 64 {
		return "", fmt.Errorf("sandbox: rootfs variant name too long")
	}
	if strings.ContainsAny(name, "/\\\x00") || strings.Contains(name, "..") || name[0] == '.' {
		return "", fmt.Errorf("sandbox: rootfs selector %q is path-shaped; expected a curated catalog name", name)
	}
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			return "", fmt.Errorf("sandbox: rootfs variant name %q has an illegal character", name)
		}
	}
	return name, nil
}

func catalogNames() string {
	names := make([]string, 0, len(rootfsCatalog))
	for n := range rootfsCatalog {
		names = append(names, n)
	}
	return strings.Join(names, ", ")
}

// --- environment allowlist ---
//
// The sandbox env is an allowlist, not a passthrough: a key not on this
// set fails the launch loudly. The set is the UNION of what the sandbox
// legitimately needs — enumerated here explicitly with each group tied to
// its producer. internal/agentproc's secret_keys drift test asserts every
// key the proxy/git wiring emits is covered here, so a new producer key
// can't silently slip past validation.
//
// Values are unrestricted (URLs, per-run placeholder tokens, git identity)
// — Property B holds because the orchestrator only ever puts non-secret /
// per-run-scoped values here; the real credentials live in the host-side
// proxies and never cross into the sandbox env.
var allowedSandboxEnvKeys = map[string]struct{}{
	// Base runtime floor (agentproc.buildSandboxEnv + agentRuntimeEnv).
	"PATH":           {},
	"HOME":           {},
	"TERM":           {},
	"BUN_JSC_useJIT": {},

	// Run-scoped metadata (delegate/resume ExtraEnv + the git-hooks bin).
	"TRIAGE_FACTORY_RUN_ID":               {},
	"TRIAGE_FACTORY_RUN_ROOT":             {},
	"TRIAGE_FACTORY_BLUEPRINT_RUN_ID":     {},
	"TRIAGE_FACTORY_REPO":                 {},
	"TRIAGE_FACTORY_GIT_COAUTHOR_TRAILER": {},
	"TRIAGE_FACTORY_BIN":                  {},

	// Egress proxy routing (agentproc.sandboxEgressProxyEnv). Both spellings
	// because curl reads only the lowercase forms.
	"HTTPS_PROXY": {},
	"https_proxy": {},
	"HTTP_PROXY":  {},
	"http_proxy":  {},
	"NO_PROXY":    {},
	"no_proxy":    {},

	// LLM proxy placeholders (agentproc.buildSandboxProxyEnv). The API-key /
	// AWS-key values here are per-run PLACEHOLDERS scoped to the run's own
	// proxy, never the real provider credential (Property B).
	"ANTHROPIC_BASE_URL":         {},
	"ANTHROPIC_API_KEY":          {},
	"ANTHROPIC_BEDROCK_BASE_URL": {},
	"AWS_BEARER_TOKEN_BEDROCK":   {},
	"CLAUDE_CODE_USE_BEDROCK":    {},
	"AWS_ACCESS_KEY_ID":          {},
	"AWS_SECRET_ACCESS_KEY":      {},
	"AWS_REGION":                 {},

	// Git routing config count + the push-capture hook toggle
	// (agentproc.encodeGitConfigEnv, githooks.PushCaptureEnvVar). The
	// numbered GIT_CONFIG_KEY_<n>/GIT_CONFIG_VALUE_<n> entries are matched
	// by prefix below.
	"GIT_CONFIG_COUNT":    {},
	"TF_GIT_PUSH_CAPTURE": {},
}

// allowedSandboxEnvPrefixes covers the dynamically-numbered env families
// (git's indexed config form) whose exact key count isn't known ahead of
// time. A key matching one of these prefixes is allowed.
var allowedSandboxEnvPrefixes = []string{
	"GIT_CONFIG_KEY_",
	"GIT_CONFIG_VALUE_",
}

// EnvKeyAllowed reports whether an env key is on the sandbox allowlist —
// exported so internal/agentproc (which produces the sandbox env and can
// import this package without a cycle) can drift-test that every key its
// proxy/git wiring emits is covered here. Without that guard a new producer
// key would break every sandboxed run at launch — the broker's
// ValidateLaunchParams rejects an unlisted key — instead of being caught at
// test time by the drift test.
func EnvKeyAllowed(key string) bool {
	return envKeyAllowed(key)
}

// envKeyAllowed reports whether key is on the allowlist (exact or prefix).
func envKeyAllowed(key string) bool {
	if _, ok := allowedSandboxEnvKeys[key]; ok {
		return true
	}
	for _, p := range allowedSandboxEnvPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// --- rlimits ---

// allowedRlimitTypes is the closed set of POSIX rlimits a launch may set.
// Numeric-only params, but the type NAME is still validated so a launch
// can't request an unrelated (or nonsensical) limit.
var allowedRlimitTypes = map[string]struct{}{
	"RLIMIT_NOFILE": {},
	"RLIMIT_NPROC":  {},
}

// --- egress CIDR denylist ---
//
// The self-host extra egress CIDR is validated against an IMMUTABLE
// internal denylist before it could ever become an iptables permit;
// "validated" means safe, not merely well-formed. The denylist is the
// operator/tenant-protecting set from sandbox-fleet §3.1: the cloud
// metadata endpoint, the sandbox subnet pool, and all private/link-local
// space (which contains the control-plane subnet and every other tenant's
// gateway on shared infra). A denylisted CIDR is rejected loudly.

// internalEgressDenylist is the set of networks an extra egress CIDR may
// never overlap. Parsed once at init.
var internalEgressDenylist = mustParseCIDRs(
	"169.254.0.0/16", // link-local — includes the cloud metadata endpoint 169.254.169.254
	"10.42.0.0/16",   // the sandbox subnet pool (subnetBase) — reaching a sibling run's gateway
	"10.0.0.0/8",     // RFC1918 — contains the control-plane subnet + operator internal network on shared infra
	"172.16.0.0/12",  // RFC1918
	"192.168.0.0/16", // RFC1918
	"127.0.0.0/8",    // loopback
	"::1/128",        // loopback (v6)
	"fc00::/7",       // unique-local (v6)
	"fe80::/10",      // link-local (v6)
)

func mustParseCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic("sandbox: bad internal denylist CIDR " + c + ": " + err.Error())
		}
		out = append(out, n)
	}
	return out
}

// validateEgressCIDR rejects an extra egress CIDR that is malformed or
// that overlaps the immutable internal denylist. An empty CIDR is a no-op
// (no extra egress requested). This is the security gate the epic requires
// "before any iptables permit is written"; the permit that would apply an
// accepted CIDR is the self-host raw-L3-to-private variant tracked with
// the sandbox fleet, not wired here — no caller populates the field today.
func validateEgressCIDR(cidr string) error {
	if cidr == "" {
		return nil
	}
	// Accept both a bare IP and a CIDR; normalize a bare IP to a host route.
	if !strings.Contains(cidr, "/") {
		if ip := net.ParseIP(cidr); ip != nil {
			if ip.To4() != nil {
				cidr += "/32"
			} else {
				cidr += "/128"
			}
		}
	}
	_, reqNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return fmt.Errorf("sandbox: extra egress CIDR %q is malformed: %w", cidr, err)
	}
	for _, deny := range internalEgressDenylist {
		if cidrsOverlap(reqNet, deny) {
			return fmt.Errorf("sandbox: extra egress CIDR %q overlaps the internal denylist %s (metadata/control-plane/private ranges are never permitted)", cidr, deny)
		}
	}
	return nil
}

// cidrsOverlap reports whether two networks intersect (either contains the
// other's base address). Enough for denylist enforcement: a permitted CIDR
// must not touch any denied range at all.
func cidrsOverlap(a, b *net.IPNet) bool {
	return a.Contains(b.IP) || b.Contains(a.IP)
}

// --- id / netns / argv validation ---

// validateRunID rejects an id (run id or container id) that is empty,
// over-long, or carries path structure. These ids seed the bundle dir
// prefix, the netns name, and the cgroup name; a path-shaped id must never
// be able to redirect any of those.
func validateRunID(kind, id string) error {
	if id == "" {
		return fmt.Errorf("sandbox: %s is required", kind)
	}
	if len(id) > 128 {
		return fmt.Errorf("sandbox: %s is too long", kind)
	}
	if strings.ContainsAny(id, "/\\\x00") || strings.Contains(id, "..") {
		return fmt.Errorf("sandbox: %s %q is path-shaped", kind, id)
	}
	return nil
}

// validateNetnsPath rejects a netns path that is not the per-run namespace
// this run's own netns MUST be. Two layers:
//
//   - Shape: it must live under /var/run/netns (or /run/netns) and match the
//     tf-<hex>-<idx> naming — this alone stops the primary escalation, a
//     compromised orchestrator pointing the sandbox at the host netns (or any
//     non-sandbox namespace) to bypass the per-run egress allowlist.
//   - Ownership: its name must equal NetnsNameForRun(runID, idx) — the name
//     derived from THIS launch's run id. That binds the namespace to the run
//     and rejects a sibling run's (still broker-created, so shape-valid)
//     netns, which shape-only validation would wave through.
//
// Residual: a fully compromised orchestrator drives both SetupNetwork and
// LaunchRun, so it can still make the two consistent by also lying about the
// run id it targets. Airtight per-run ownership requires the broker to track
// the netns it created for each run in its own state (the "broker-internal
// network state" evolution the launch params anticipate); it is bounded even
// so, because such an orchestrator already holds every host-side credential a
// reachable sibling gateway's proxy would mediate.
func validateNetnsPath(runID, p string) error {
	if p == "" {
		return fmt.Errorf("sandbox: netns path is required")
	}
	dir, name := filepath.Split(p)
	if filepath.Clean(dir) != "/var/run/netns" && filepath.Clean(dir) != "/run/netns" {
		return fmt.Errorf("sandbox: netns path %q is not under /var/run/netns (or /run/netns)", p)
	}
	idx, ok := subnetIdxFromNetnsName(name)
	if !ok {
		return fmt.Errorf("sandbox: netns name %q is not a broker-created sandbox namespace", name)
	}
	if name != NetnsNameForRun(runID, idx) {
		return fmt.Errorf("sandbox: netns %q is not the namespace created for this run", name)
	}
	return nil
}

// validateArgv enforces the pinned entrypoint: the first two argv elements
// must be exactly the node binary + wrapper. Everything after is the
// wrapper's own arguments, which only steer the unprivileged agent.
func validateArgv(argv []string) error {
	if len(argv) < 2 {
		return fmt.Errorf("sandbox: argv must start with the pinned entrypoint %s %s", sandboxNodeBinary, sandboxWrapperEntry)
	}
	if argv[0] != sandboxNodeBinary || argv[1] != sandboxWrapperEntry {
		return fmt.Errorf("sandbox: argv entrypoint %q %q is not the pinned %q %q; the broker owns the command", argv[0], argv[1], sandboxNodeBinary, sandboxWrapperEntry)
	}
	return nil
}

// ValidateLaunchParams is the broker's RPC-boundary gate. It runs before
// the broker resolves the rootfs, builds the spec, or execs anything, so
// every rejection below denies a compromised orchestrator a way to steer
// the privileged broker with hostile data. Each check maps to a documented
// rejection: path-shaped ids, an unknown/path-shaped rootfs name, a
// non-allowlisted env key, a non-pinned command, an unknown rlimit, a
// forged netns, or a denylisted egress CIDR.
func ValidateLaunchParams(p LaunchParams) error {
	if err := validateRunID("container id", p.ContainerID); err != nil {
		return err
	}
	if p.RunID != "" {
		if err := validateRunID("run id", p.RunID); err != nil {
			return err
		}
	}
	if _, err := validateRootfsName(p.Rootfs.Name); err != nil {
		return err
	}
	if _, ok := rootfsCatalog[orDefaultRootfs(p.Rootfs.Name)]; !ok {
		return fmt.Errorf("sandbox: unknown rootfs variant %q (catalog: %s)", p.Rootfs.Name, catalogNames())
	}
	// Worktree (/work) is a bind-mount SOURCE the privileged broker mounts;
	// require a clean absolute path at the boundary so a relative or
	// non-clean value can't resolve to an unintended host location or fail
	// late inside runsc. SDKDir is deliberately NOT trusted here — the broker
	// overrides it with TrustedSDKDir() before building the spec, so whatever
	// the orchestrator sends is discarded (see the launchRun override).
	if err := validateAbsCleanPath("worktree", p.Worktree); err != nil {
		return err
	}
	if err := validateArgv(p.Args); err != nil {
		return err
	}
	if err := validateEnv(p.Env); err != nil {
		return err
	}
	if err := validateRlimits(p.Rlimits); err != nil {
		return err
	}
	if err := validateMounts(p.Mounts); err != nil {
		return err
	}
	if err := validateNetnsPath(p.RunID, p.NetnsPath); err != nil {
		return err
	}
	if err := validateEgressCIDR(p.ExtraEgressCIDR); err != nil {
		return err
	}
	return nil
}

// ValidateSidecarLaunchParams is the broker's RPC-boundary gate for
// LaunchSidecar — the sidecar analog of ValidateLaunchParams. The
// load-bearing check is the uid/gid band: the orchestrator computes
// SidecarUID(subnetIdx) itself and sends the result, so the broker must
// independently confirm it falls inside the reserved sidecar range (via
// IsSidecarUID — the same predicate the boot-time orphan reap uses) before
// ever handing it to a setuid Credential — otherwise a compromised
// orchestrator could ask the broker to exec the sidecar as uid 0, as its
// own uid, or as any other uid on the host.
//
// The ContainerID suffix check closes the other half of the shared-registry
// boundary: SidecarContainerIDSuffix is what keeps a sidecar's registry key
// from ever colliding with a run's own (see wrap()'s "tf-<frag>-<idx>"
// naming, which never carries it) — enforced here rather than left as a
// convention only the Linux dispatcher (sidecar_linux.go) upholds.
//
// Residual (accepted, same shape as validateNetnsPath's documented one): a
// fully compromised orchestrator could still request a uid inside the band
// that's already in use by another live sidecar, by lying about which
// subnet index it belongs to — airtight per-run uid ownership would need
// the broker to track which index it handed out for which run, the same
// broker-internal-state evolution validateNetnsPath's doc anticipates. It's
// bounded even so: such an orchestrator already holds every host-side
// credential a reachable sibling sidecar would mediate.
func ValidateSidecarLaunchParams(p SidecarLaunchParams) error {
	if err := validateRunID("sidecar container id", p.ContainerID); err != nil {
		return err
	}
	if !strings.HasSuffix(p.ContainerID, SidecarContainerIDSuffix) {
		return fmt.Errorf("sandbox: sidecar container id %q missing the %q suffix that keeps it distinct from a run's own", p.ContainerID, SidecarContainerIDSuffix)
	}
	if p.UID != p.GID {
		return fmt.Errorf("sandbox: sidecar uid %d and gid %d must match", p.UID, p.GID)
	}
	if !IsSidecarUID(p.UID) {
		return fmt.Errorf("sandbox: sidecar uid %d outside the reserved band [%d, %d)", p.UID, SidecarUIDBase, SidecarUIDBase+MaxSandboxes)
	}
	return nil
}

// ValidateCaptureStdoutSocketPath validates the per-capture stdout socket
// path the orchestrator sends over the CaptureRunDelta RPC — the same
// clean-absolute-path treatment ValidateLaunchParams gives Worktree, since
// it is the same kind of value: a bind SOURCE / dial TARGET the privileged
// broker touches on the orchestrator's say-so. Dialing this path carries no
// privilege delta (the broker and orchestrator share a uid; a compromised
// orchestrator could dial anything itself from its own process), so
// path-shape validation is sufficient here — unlike the rootfs/env/mount
// checks above, there is no allowlist to invent.
func ValidateCaptureStdoutSocketPath(path string) error {
	return validateAbsCleanPath("capture stdout socket path", path)
}

func orDefaultRootfs(name string) string {
	if name == "" {
		return defaultRootfsName
	}
	return name
}

// allowedMountOptions is the closed set of per-mount options the broker
// honors. The run-data mounts the orchestrator sends only ever need
// read-only vs read-write; restricting to this set stops a compromised
// caller from slipping a surprising option (dev, suid, exec, …) into the
// broker-built spec. "rbind" is added by the broker itself (mountsFromExtra)
// and is not caller-supplied.
var allowedMountOptions = map[string]struct{}{
	"ro": {},
	"rw": {},
}

// validateMounts is defense-in-depth on the run-data bind mounts. They are
// not an escalation vector (they land in the already-unprivileged sandbox,
// never the rootfs), but the broker performs the bind with capabilities, so
// require both Source and Destination to be clean absolute paths and the
// Options to be within the small honored set — a malformed descriptor can't
// then produce a surprising spec or an unexpected mount flag.
//
// Residual (accepted): the Source is only shape-checked, not pinned to the
// run's own paths — the broker has no per-run notion of which host paths this
// run owns (the orchestrator creates the worktree/checkouts), so a compromised
// orchestrator could direct the broker to bind any path readable by the fixed
// sandbox UID (10000), including a sibling run's worktree, into a new sandbox.
// This is a data confidentiality/integrity residual bounded by host DAC at
// that fixed UID (no user-namespace remapping) — NOT a capability escalation.
// A real per-run source allowlist would need the broker to track each run's
// owned paths; tracked with the broker-internal-state evolution.
func validateMounts(mounts []Mount) error {
	for _, m := range mounts {
		if err := validateAbsCleanPath("mount source", m.Source); err != nil {
			return err
		}
		if err := validateAbsCleanPath("mount destination", m.Destination); err != nil {
			return err
		}
		for _, o := range m.Options {
			if _, ok := allowedMountOptions[o]; !ok {
				return fmt.Errorf("sandbox: mount %q has unsupported option %q (only ro/rw)", m.Destination, o)
			}
		}
	}
	return nil
}

// validateAbsCleanPath rejects a path that is empty, not absolute, not
// already cleaned, or that carries a NUL. Used for the paths the broker
// binds (worktree, sdk, mount sources/destinations) so the RPC boundary —
// not a late runsc failure — is where a bad path is caught.
func validateAbsCleanPath(kind, p string) error {
	if p == "" {
		return fmt.Errorf("sandbox: %s is required", kind)
	}
	if strings.IndexByte(p, 0) >= 0 {
		return fmt.Errorf("sandbox: %s contains NUL", kind)
	}
	if !filepath.IsAbs(p) || p != filepath.Clean(p) {
		return fmt.Errorf("sandbox: %s %q must be a clean absolute path", kind, p)
	}
	return nil
}

// validateEnv checks every env entry against the allowlist AND rejects
// malformed entries — an empty key, a key carrying '=' or NUL, or a value
// carrying NUL — so the broker never folds an entry into the spec that
// would misparse or fail at exec/syscall time. Values are otherwise
// unrestricted (URLs, per-run placeholder tokens); only the key is
// allowlisted.
func validateEnv(env []EnvVar) error {
	for _, e := range env {
		if e.Key == "" {
			return fmt.Errorf("sandbox: env entry has an empty key")
		}
		if strings.ContainsAny(e.Key, "=\x00") {
			return fmt.Errorf("sandbox: env key %q contains '=' or NUL", e.Key)
		}
		if strings.IndexByte(e.Value, 0) >= 0 {
			return fmt.Errorf("sandbox: env value for %q contains NUL", e.Key)
		}
		if !envKeyAllowed(e.Key) {
			return fmt.Errorf("sandbox: env key %q is not on the sandbox allowlist", e.Key)
		}
	}
	return nil
}

// validateRlimits checks the numeric resource shape: each type must be on
// the allowlist, soft must not exceed hard, and a type must not repeat.
// Catching this at the RPC boundary keeps runtime behavior predictable
// rather than surfacing as a late, opaque runsc failure.
func validateRlimits(rl []Rlimit) error {
	seen := make(map[string]struct{}, len(rl))
	for _, r := range rl {
		if _, ok := allowedRlimitTypes[r.Type]; !ok {
			return fmt.Errorf("sandbox: rlimit type %q is not allowed", r.Type)
		}
		if r.Soft > r.Hard {
			return fmt.Errorf("sandbox: rlimit %s soft %d exceeds hard %d", r.Type, r.Soft, r.Hard)
		}
		if _, dup := seen[r.Type]; dup {
			return fmt.Errorf("sandbox: rlimit type %q is set more than once", r.Type)
		}
		seen[r.Type] = struct{}{}
	}
	return nil
}

// --- bundle preparation (broker-owned spec) ---

// envVarsToStrings folds validated key/value pairs back into the "K=V"
// slice buildSpec expects for spec.Process.Env, preserving order.
func envVarsToStrings(env []EnvVar) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		out = append(out, e.Key+"="+e.Value)
	}
	return out
}

// stringsToEnvVars splits a "K=V" slice into structured pairs at the first
// '='. The orchestrator builds the sandbox env as "K=V" strings (base env +
// proxy additions), which always contain '='; this is the boundary
// conversion into the validated wire shape. An entry with no '=' becomes a
// key-only pair (empty value); the broker's validateEnv then rejects it
// unless the whole string happens to be an allowlisted key, in which case it
// folds to "KEY=" — harmless, and never produced by the real orchestrator env.
func stringsToEnvVars(env []string) []EnvVar {
	out := make([]EnvVar, 0, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			out = append(out, EnvVar{Key: kv[:i], Value: kv[i+1:]})
		} else {
			out = append(out, EnvVar{Key: kv})
		}
	}
	return out
}

// PrepareBundle is the broker-owned spec construction: from the validated
// LaunchParams it resolves the rootfs against the catalog (name → hash →
// read-only path), builds the OCI spec from the fixed template, and writes
// the per-run bundle. It runs on the privileged (cap-broker) side, which
// holds the capabilities; the orchestrator only ever supplies the validated
// data above.
//
// The broker's launch dispatch MUST call ValidateLaunchParams before this —
// the boundary check that keeps a compromised orchestrator from steering the
// spec construction. Returns the bundle dir; the caller (the LaunchedRun)
// owns removing it via cleanupBundle when the run ends.
func PrepareBundle(ctx context.Context, p LaunchParams) (string, error) {
	rootfsPath, err := resolveCatalogRootfs(ctx, p.Rootfs)
	if err != nil {
		return "", err
	}
	// RunID only seeds the bundle dir's grep prefix + buildSpec's
	// required-field check; the unique key is ContainerID. Fall back to it
	// so an empty (but otherwise valid) RunID doesn't fail the build.
	runID := p.RunID
	if runID == "" {
		runID = p.ContainerID
	}
	cfg := Config{
		RunID:         runID,
		Worktree:      p.Worktree,
		SDKDir:        p.SDKDir,
		Argv:          p.Args,
		Env:           envVarsToStrings(p.Env),
		ExtraMounts:   p.Mounts,
		MemoryLimitMB: p.MemoryLimitMB,
		Rlimits:       p.Rlimits,
	}
	spec, err := buildSpec(cfg, p.NetnsPath)
	if err != nil {
		return "", err
	}
	bundleDir, err := writeBundle(cfg, spec, rootfsPath)
	if err != nil {
		return "", err
	}
	return bundleDir, nil
}

// RemoveBundle removes a bundle dir PrepareBundle created. Idempotent.
// Exported so the cap-broker — which now owns the bundle it built for a
// supervised run — can reclaim it when the run is reaped, without reaching
// into the package's unexported bundle helpers.
func RemoveBundle(bundleDir string) error {
	return cleanupBundle(bundleDir)
}
