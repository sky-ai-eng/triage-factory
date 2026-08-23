//go:build linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// TrustedGitHooksDir is the broker-owned git-hooks bind-mount source
// (destination githooks.SandboxDir): the broker resolves the host hooks
// dir itself rather than trusting the orchestrator's claimed source for
// this fixed, global destination — same principle as resolving the rootfs
// by catalog name rather than accepting a path.
func TrustedGitHooksDir() string {
	return paths.HooksDir()
}

// TrustedTFBinaryDestination mirrors agentproc's private sandboxTFBinary
// constant — the fixed in-sandbox path the host TF binary is bind-mounted
// at. Duplicated as a literal (not imported: agentproc imports this
// package, so the reverse import would cycle); agentproc's own test suite
// cross-checks the two literals stay equal.
const TrustedTFBinaryDestination = "/usr/local/bin/triagefactory"

// TrustedTFBinaryPath resolves the broker's own running binary. It is the
// same file the orchestrator re-execs to spawn the broker (see
// cmd/capbroker's execSelfCommand: `exec.Command(self, "cap-broker", ...)`
// where self is the orchestrator's own os.Executable()), so calling
// os.Executable() again from inside the broker process always resolves to
// the identical path — no RPC field needed, mirroring TrustedGitHooksDir.
func TrustedTFBinaryPath() (string, error) {
	return os.Executable()
}

// TrustedTFACBinaryDestination mirrors agentproc's private sandboxTFACBinary —
// the second in-sandbox path the TF binary is bind-mounted at, under the
// additive `tfac` applet name. Same trusted source as the canonical TF-binary
// mount (the broker's own re-exec target), just a different destination.
const TrustedTFACBinaryDestination = "/opt/tf/bin/tfac"

// TrustedGHBinaryDestination mirrors agentproc's private sandboxGHBinary — the
// fixed in-sandbox path the TF-pinned real gh binary is bind-mounted at.
// Duplicated as a literal for the same cycle reason as TrustedTFBinaryDestination;
// agentproc's drift test cross-checks the two literals stay equal.
const TrustedGHBinaryDestination = "/opt/tf/bin/gh"

// trustedGHBinaryPath is the executor-host path the pinned gh binary is baked at
// (docker/Dockerfile). A var (not const) so this package's own tests can point
// it at a stub the non-root `go test` user can create; production never
// reassigns it, and the source it validates is the same fixed image path
// agentproc mounts from.
var trustedGHBinaryPath = "/opt/tf/bin/gh"

// TrustedGHBinaryPath returns the broker-trusted source path for the pinned gh
// binary mount. Unlike the TF binary (the broker's own re-exec target), gh is a
// fixed image path, so the broker validates a launch's gh mount source against
// this literal.
func TrustedGHBinaryPath() string { return trustedGHBinaryPath }

// TrustedGHInjectorCertDestination mirrors agentproc's private
// sandboxGHInjectorCert — the fixed in-sandbox path the per-run gh-injector cert
// is bind-mounted at (SSL_CERT_FILE points here).
const TrustedGHInjectorCertDestination = "/run/tf-gh-injector.crt"

// TrustedGHInjectorCertPath is the broker's own derivation of "this run's own
// gh-injector cert" host-side path. Like the agenthost socket, the broker did
// not create this file — the sidecar writes it during bring-up (agenthost.
// WriteInjectorCert → CertPathFor) before the launch RPC — so the broker
// VALIDATES a launch's mount source against this derivation. A drift test
// cross-checks it agrees with agenthost.CertPathFor.
func TrustedGHInjectorCertPath(conversationID string) string {
	return filepath.Join(trustedAgentHostSocketRoot, sanitizeSocketName(conversationID)+"-gh-injector.crt")
}

// TrustedToolHostBinaryDestination is the fixed in-sandbox path the
// compiled tool host is bind-mounted at, and the first element of the
// native runtime's pinned argv. Its source is a fixed image path, validated
// like the pinned gh binary rather than resolved from the RPC.
const TrustedToolHostBinaryDestination = "/opt/tf/bin/tf-harness-tools"

// toolHostServeVerb is the second element of the native runtime's pinned
// argv. Pinning the verb as well as the program is what keeps the tool
// host's other modes (the one-shot CLI, --definitions) off the launch path:
// only the resident server can be started as a jail's main process.
const toolHostServeVerb = "serve"

// trustedToolHostBinaryPath is the executor-host path the compiled tool
// host is baked at (docker/Dockerfile). A var for the same reason as
// trustedGHBinaryPath: this package's own tests point it at a stub a
// non-root `go test` user can create. Production never reassigns it.
var trustedToolHostBinaryPath = "/opt/tf/bin/tf-harness-tools"

// TrustedToolHostBinaryPath returns the broker-trusted source for the tool
// host mount.
func TrustedToolHostBinaryPath() string { return trustedToolHostBinaryPath }

// TrustedAgentHostSocketDestination mirrors cmd/exec/agenthost's exported
// DefaultSocketPath — the fixed in-sandbox path the per-run agenthost
// socket is bind-mounted at. Duplicated as a literal for the same cycle
// reason as TrustedTFBinaryDestination (agenthost imports this package);
// cmd/exec/agenthost's own test suite cross-checks the two literals stay
// equal.
const TrustedAgentHostSocketDestination = "/run/tf.sock"

// TrustedAgentHostSocketPath is the broker's own derivation of "this run's
// own agenthost socket" host-side path. Unlike TrustedGitHooksDir/
// TrustedTFBinaryPath, the broker did not create this
// file — cmd/exec/agenthost.Start does, host-side, before the launch RPC —
// so the broker VALIDATES a launch's mount source against this derivation
// rather than resolving/overriding it.
func TrustedAgentHostSocketPath(conversationID string) string {
	return filepath.Join(trustedAgentHostSocketRoot, sanitizeSocketName(conversationID)+".sock")
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
	// Claude Code behavior toggle: suppress the SDK's non-essential traffic
	// (telemetry / error reporting / auto-updater / feature gates) it can't
	// reach through the fail-closed egress allowlist anyway. Non-credential.
	"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": {},
	// The "I am inside the jail" marker (agentproc.SandboxMarkerEnvVar).
	// Non-credential; in-jail code reads it to tell a missing exec-verb
	// socket apart from a local-mode CLI invocation.
	"TRIAGE_FACTORY_SANDBOXED": {},

	// Run-scoped metadata (delegate/resume ExtraEnv + the git-hooks bin).
	"TRIAGE_FACTORY_CONVERSATION_ID":      {},
	"TRIAGE_FACTORY_CONVERSATION_ROOT":    {},
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

	// Go toolchain policy + fetch-relay routing (agentproc.buildSandboxEnv;
	// GOPROXY comes from the egressrelay catalog's go-modules entry).
	// Non-credential, and not paths the broker itself acts on — cmd/go
	// inside the jail is the only reader. GOPROXY names this run's own
	// host-side relay, the same shape as the egress and LLM proxy
	// addresses above. A new catalog entry's env key gets its own line
	// here; the agentproc env drift test names the key when one is
	// missing.
	"GOTOOLCHAIN": {},
	"GOPROXY":     {},

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

	// Real-gh channel (agentproc.ghChannelEnv). GH_HOST redirects gh at the
	// per-run injector; GH_ENTERPRISE_TOKEN is the per-run PLACEHOLDER (never
	// the real token — Property B); SSL_CERT_FILE points at the mounted per-run
	// injector cert; the two GH_* suppressors keep gh non-interactive and quiet.
	"GH_HOST":               {},
	"GH_ENTERPRISE_TOKEN":   {},
	"SSL_CERT_FILE":         {},
	"GH_PROMPT_DISABLED":    {},
	"GH_NO_UPDATE_NOTIFIER": {},
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

// validateOpaqueID rejects an id that is empty, over-long, or carries path
// structure. It takes no view on which id it is handed — callers pass a
// conversation id, a container id, or a memory namespace, and name it in
// kind. These ids seed the bundle dir prefix, the netns name, and the cgroup
// name; a path-shaped id must never be able to redirect any of those.
func validateOpaqueID(kind, id string) error {
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
//   - Ownership: its name must equal NetnsNameForRun(conversationID, idx) — the name
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
func validateNetnsPath(conversationID, p string) error {
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
	if name != NetnsNameForRun(conversationID, idx) {
		return fmt.Errorf("sandbox: netns %q is not the namespace created for this run", name)
	}
	return nil
}

// validateArgv enforces a pinned entrypoint: the native runtime's resident
// tool host plus its `serve` verb. The orchestrator may vary only the
// arguments after it (the socket path and working directory).
//
// The entrypoint is a program the broker itself resolves the source of
// (TrustedToolHostBinaryPath), which is what "the broker owns the command"
// means: a second pin would add a second broker-owned program, not a
// caller-chosen one.
func validateArgv(argv []string) error {
	if len(argv) >= 2 && argv[0] == TrustedToolHostBinaryDestination && argv[1] == toolHostServeVerb {
		return nil
	}
	return fmt.Errorf("sandbox: argv entrypoint %v is not the pinned entrypoint (%q %q); the broker owns the command",
		argv, TrustedToolHostBinaryDestination, toolHostServeVerb)
}

// ValidateLaunchParams is the broker's RPC-boundary gate. It runs before
// the broker resolves the rootfs, builds the spec, or execs anything, so
// every rejection below denies a compromised orchestrator a way to steer
// the privileged broker with hostile data. Each check maps to a documented
// rejection: path-shaped ids, an unknown/path-shaped rootfs name, a
// non-allowlisted env key, a non-pinned command, an unknown rlimit, a
// worktree/mount source outside this run's own scope, a forged netns, or a
// denylisted egress CIDR.
func ValidateLaunchParams(p LaunchParams) error {
	if err := validateOpaqueID("container id", p.ContainerID); err != nil {
		return err
	}
	if p.ConversationID != "" {
		if err := validateOpaqueID("run id", p.ConversationID); err != nil {
			return err
		}
	}
	// The memory namespace (blueprint run id) is the run's second legitimate
	// run-tree key — see worktreeScope. Optional, but path-shape it when present
	// so it can't smuggle a traversal into the RunTreeRoot join.
	if p.MemoryNamespace != "" {
		if err := validateOpaqueID("memory namespace", p.MemoryNamespace); err != nil {
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
	// late inside runsc.
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
	// Beyond the shape check above, pin the worktree to this run's own scope
	// and every mount source to either a broker-resolved global, this run's
	// own agenthost socket, or the same org scope as the worktree — see
	// worktreeScope's doc for the internal-consistency ceiling this enforces
	// (and does not: a credential-free broker cannot authenticate an org).
	if err := validateWorktreeAndMounts(p.ConversationID, p.MemoryNamespace, p.Worktree, p.Mounts); err != nil {
		return err
	}
	if err := validateNetnsPath(p.ConversationID, p.NetnsPath); err != nil {
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
	if err := validateOpaqueID("sidecar container id", p.ContainerID); err != nil {
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
	// The subnet index the broker binds the shared-origin listener against must
	// be the one this uid was derived from. Without the tie the index would be a
	// second, independent way to name a bind address — a caller could occupy one
	// run's sidecar slot while asking the broker to bind another run's veth IP,
	// and stand a listener in front of a sibling's traffic.
	if p.UID != SidecarUID(p.SubnetIdx) {
		return fmt.Errorf("sandbox: sidecar uid %d does not match subnet index %d (want uid %d)", p.UID, p.SubnetIdx, SidecarUID(p.SubnetIdx))
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

// realPath resolves p to its fully symlink-free, canonical form so the
// prefix/equality comparisons below operate on the actual path a bind
// mount would read, not the caller's possibly-symlinked claim. Without
// this, a compromised orchestrator — which legitimately owns and writes
// its own org's tree (repos, projects) — could plant a symlink
// INSIDE its own org's subtree pointing at another org's data (or
// anywhere else) and pass the lexically-in-scope symlink path as a mount
// source or worktree; a purely lexical filepath.Rel/== check would wave
// it through, and the broker would then bind-mount whatever it actually
// resolves to. Requires p to exist, which every real mount source and
// worktree does by launch time (the orchestrator materializes the
// worktree, and cmd/exec/agenthost creates its socket, before ever issuing
// the launch RPC) — a caller-supplied path that doesn't exist yet is
// rejected here rather than failing later inside runsc.
//
// Residual: this closes the STATIONARY case — a symlink already in place
// when the broker validates. It does not close a race where a compromised
// orchestrator swaps a resolved component for a symlink in the narrow
// window between this check and the broker's later bind-mount syscall;
// airtight closure of that would need the mount to resolve via
// openat2(RESOLVE_NO_SYMLINKS) into a stable fd, the same kernel-level
// technique tracked for the run-tree chown/remove TOCTOU. Bounded the same
// way as this file's other residuals: winning that race requires the
// orchestrator to already be compromised, at which point it holds every
// credential its own org's data would expose anyway.
func realPath(p string) (string, error) {
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("resolve real path of %q: %w", p, err)
	}
	return resolved, nil
}

// worktreeScope classifies Config.Worktree into the shape it must be one
// of, and returns the org-tree prefix every OTHER mount's source must
// share. This is internal consistency, not tenant authentication: the
// broker holds no credentials and has no tenant model, so it cannot
// authenticate "this run belongs to org A" — it can only require that
// every path a run touches shares the scope its OWN worktree resolved to.
// A fully compromised orchestrator can still set the worktree and every
// mount consistently under one org's tree, but that is just placing a run
// as that org outright (which it could always do); what this closes is
// the MIXED shape — org A's run identity paired with org B's data.
//
// Two shapes are legitimate, matched against the actual producers
// (internal/worktree, verified — not assumed):
//
//   - The ephemeral per-run tree: GitHub PR / Jira / Slack delegated task
//     runs materialize (or park) their whole working tree under
//     os.TempDir()/triagefactory-runs/<key>. The key is one of the run's OWN
//     two lifetime keys: its run id on the first launch (MakeRunRoot(conversationID)),
//     or its memory namespace — the blueprint run id — after a cold rehydrate
//     rebuilds the tree at RunRoot(namespace) (internal/delegate's snapshot
//     restore). Either of this run's keys is accepted; a THIRD run's tree is
//     not. These runs are org-blind by construction (the tree doesn't outlive
//     the run), so hasScope is false and no OTHER mount may claim an org scope
//     either — there is nothing for it to be consistent with.
//   - The org-scoped state-root tree: paths.BareCacheDir(orgID, owner, repo),
//     i.e. <StateRoot>/orgs/<orgID>/… in multi mode. orgPrefix is
//     <StateRoot>/orgs/<orgID>; every other mount under this run must live
//     under this same prefix.
//
// Anything else — an arbitrary host path, a worktree one level too
// shallow to name an org, a symlink-clean but out-of-tree path — is
// rejected outright.
func worktreeScope(conversationID, memoryNamespace, worktree string) (orgPrefix string, hasScope bool, err error) {
	realWorktree, err := realPath(worktree)
	if err != nil {
		return "", false, fmt.Errorf("sandbox: worktree %q: %w", worktree, err)
	}
	// Either of the run's own two lifetime keys is a legitimate ephemeral tree:
	// its run id (first launch) or its memory namespace / blueprint run id (a
	// tree rebuilt by a cold rehydrate). Matching either short-circuits with
	// hasScope=false — no org scope for an org-blind tree.
	for _, key := range [2]string{conversationID, memoryNamespace} {
		if key == "" {
			continue
		}
		if realRunTree, rtErr := realPath(RunTreeRoot(key)); rtErr == nil && realWorktree == realRunTree {
			return "", false, nil
		}
	}
	root, err := paths.StateRootErr()
	if err != nil {
		return "", false, fmt.Errorf("sandbox: resolve state root: %w", err)
	}
	orgsRoot, err := realPath(filepath.Join(root, "orgs"))
	if err != nil {
		// A missing orgs tree just means no org-scoped worktree exists yet (a
		// fresh deploy with no org-scoped sessions). The worktree, having matched
		// neither of this run's own trees, is simply not a legitimate shape —
		// report that rather than the bare lstat of the absent orgs dir.
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, fmt.Errorf("sandbox: worktree %q is neither this run's own tree nor under the state-root org tree", worktree)
		}
		return "", false, fmt.Errorf("sandbox: worktree %q is neither this run's own tree nor under the state-root org tree: %w", worktree, err)
	}
	rel, relErr := filepath.Rel(orgsRoot, realWorktree)
	if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false, fmt.Errorf("sandbox: worktree %q is neither this run's own tree nor under the state-root org tree", worktree)
	}
	orgID, rest, found := strings.Cut(rel, string(filepath.Separator))
	if orgID == "" || orgID == "." || !found || rest == "" {
		return "", false, fmt.Errorf("sandbox: worktree %q does not resolve to a per-org subtree", worktree)
	}
	// orgsRoot is already fully resolved (realPath above) and orgID is a
	// literal path segment taken from realWorktree — also fully resolved —
	// so this reconstruction is itself a real, symlink-free path; no
	// further resolution needed by callers that compare against it.
	return filepath.Join(orgsRoot, orgID), true, nil
}

// validateWorktreeAndMounts is defense-in-depth on the run-data bind
// mounts AND, unlike shape-only validation, pins every mount's Source to
// this run's own scope. It requires Source/Destination to be clean
// absolute paths and Options within the small honored set (as before),
// then classifies each mount by DESTINATION against the broker-owned
// trusted set — mirroring TrustedGitHooksDir rather than trusting shape alone:
//
//   - TrustedTFBinaryDestination / githooks.SandboxDir (global, fixed):
//     the Source MUST equal the broker's own resolution
//     (TrustedTFBinaryPath / TrustedGitHooksDir) — a caller's source for
//     one of these destinations is never trusted.
//   - TrustedAgentHostSocketDestination (per-run, fixed): the Source MUST
//     equal TrustedAgentHostSocketPath(conversationID) — the broker didn't create
//     this file (cmd/exec/agenthost did, host-side, before the launch
//     RPC), so it validates rather than overrides.
//   - Everything else (e.g. a shared read-only repo checkout under the
//     same org-scoped tree): the Source MUST be under the SAME org prefix
//     worktreeScope derived from Worktree. If Worktree carried no org
//     scope (the delegated-task-run shape), NO mount may fall in this
//     bucket at all — there is no legitimate one for that shape.
func validateWorktreeAndMounts(conversationID, memoryNamespace, worktree string, mounts []Mount) error {
	orgPrefix, hasScope, err := worktreeScope(conversationID, memoryNamespace, worktree)
	if err != nil {
		return err
	}

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

		// Resolve the caller's claimed source to what it actually is before
		// comparing — see realPath's doc for why a lexical-only compare here
		// would let an orchestrator that plants a symlink inside its own
		// org's tree point the bind mount at another org's data (or
		// anywhere else) while still passing a prefix/equality check.
		realSource, err := realPath(m.Source)
		if err != nil {
			return fmt.Errorf("sandbox: mount %q source %q: %w", m.Destination, m.Source, err)
		}

		switch m.Destination {
		case TrustedTFBinaryDestination:
			trusted, tfErr := TrustedTFBinaryPath()
			if tfErr != nil {
				return fmt.Errorf("sandbox: resolve trusted TF binary path: %w", tfErr)
			}
			realTrusted, rErr := realPath(trusted)
			if rErr != nil {
				return fmt.Errorf("sandbox: resolve trusted TF binary path: %w", rErr)
			}
			if realSource != realTrusted {
				return fmt.Errorf("sandbox: mount %q source %q is not the broker-resolved TF binary path %q", m.Destination, m.Source, trusted)
			}
		case TrustedTFACBinaryDestination:
			// The additive `tfac` applet mount: same trusted source as the
			// canonical TF-binary mount (the broker's own re-exec target).
			trusted, tfErr := TrustedTFBinaryPath()
			if tfErr != nil {
				return fmt.Errorf("sandbox: resolve trusted TF binary path: %w", tfErr)
			}
			realTrusted, rErr := realPath(trusted)
			if rErr != nil {
				return fmt.Errorf("sandbox: resolve trusted TF binary path: %w", rErr)
			}
			if realSource != realTrusted {
				return fmt.Errorf("sandbox: mount %q source %q is not the broker-resolved TF binary path %q", m.Destination, m.Source, trusted)
			}
		case githooks.SandboxDir:
			trusted := TrustedGitHooksDir()
			realTrusted, rErr := realPath(trusted)
			if rErr != nil {
				return fmt.Errorf("sandbox: resolve trusted git-hooks dir: %w", rErr)
			}
			if realSource != realTrusted {
				return fmt.Errorf("sandbox: mount %q source %q is not the broker-resolved git-hooks dir %q", m.Destination, m.Source, trusted)
			}
		case TrustedAgentHostSocketDestination:
			trusted := TrustedAgentHostSocketPath(conversationID)
			realTrusted, rErr := realPath(trusted)
			if rErr != nil {
				return fmt.Errorf("sandbox: resolve this run's agenthost socket: %w", rErr)
			}
			if realSource != realTrusted {
				return fmt.Errorf("sandbox: mount %q source %q is not this run's own agenthost socket (want %q)", m.Destination, m.Source, trusted)
			}
		case TrustedGHBinaryDestination:
			realTrusted, rErr := realPath(TrustedGHBinaryPath())
			if rErr != nil {
				return fmt.Errorf("sandbox: resolve trusted gh binary path: %w", rErr)
			}
			if realSource != realTrusted {
				return fmt.Errorf("sandbox: mount %q source %q is not the broker-resolved gh binary path %q", m.Destination, m.Source, TrustedGHBinaryPath())
			}
		case TrustedGHInjectorCertDestination:
			trusted := TrustedGHInjectorCertPath(conversationID)
			realTrusted, rErr := realPath(trusted)
			if rErr != nil {
				return fmt.Errorf("sandbox: resolve this run's gh-injector cert: %w", rErr)
			}
			if realSource != realTrusted {
				return fmt.Errorf("sandbox: mount %q source %q is not this run's own gh-injector cert (want %q)", m.Destination, m.Source, trusted)
			}
		case TrustedSkillsDestination:
			// This run's own step-skill staging dir (optional — present only on a
			// launch that carries a blueprint step skill). Validate-not-override,
			// like the agenthost socket: the orchestrator staged it before the RPC,
			// so the broker requires the claimed source to resolve to its own
			// derivation from THIS launch's run id — which rejects a sibling run's
			// staging dir (shape-valid, still someone else's step) and any foreign
			// path. Read-only is required, not merely permitted: the mount exists so
			// the jail can READ a skill TF authored, and an rw bind would let the
			// agent rewrite orchestrator-owned state outside its run tree.
			if err := requireReadOnlyMount(m); err != nil {
				return err
			}
			if conversationID == "" {
				return fmt.Errorf("sandbox: mount %q requires a run id to derive this run's own staging dir", m.Destination)
			}
			// Resolve the staging BASE and re-derive, rather than resolving the
			// per-run path whole. Both forms reject a foreign source, but this one
			// additionally pins the final component to a real directory: resolving
			// <base>/<conversationID> whole would follow a symlink planted THERE — by the
			// same (compromised) orchestrator that owns the staging namespace —
			// and then agree with the equally-followed source, waving through a
			// read-only bind of anywhere on the host. With the base resolved
			// first, a symlinked per-run entry resolves outside the derived path
			// and is rejected.
			realBase, rErr := realPath(SkillStagingBase())
			if rErr != nil {
				return fmt.Errorf("sandbox: resolve step-skill staging base: %w", rErr)
			}
			trusted := filepath.Join(realBase, conversationID)
			if realSource != trusted {
				return fmt.Errorf("sandbox: mount %q source %q is not this run's own step-skill staging dir (want %q)", m.Destination, m.Source, trusted)
			}
		case TrustedToolHostBinaryDestination:
			// Read-only is REQUIRED: an rw bind of a privileged entrypoint
			// binary would let the jail rewrite the program the broker's
			// pinned-argv check trusts by path.
			if err := requireReadOnlyMount(m); err != nil {
				return err
			}
			realTrusted, rErr := realPath(TrustedToolHostBinaryPath())
			if rErr != nil {
				return fmt.Errorf("sandbox: resolve trusted tool host binary path: %w", rErr)
			}
			if realSource != realTrusted {
				return fmt.Errorf("sandbox: mount %q source %q is not the broker-resolved tool host binary path %q", m.Destination, m.Source, TrustedToolHostBinaryPath())
			}
		case TrustedToolHostSocketDirDestination:
			trusted := TrustedToolHostSocketDir(conversationID)
			realTrusted, rErr := realPath(trusted)
			if rErr != nil {
				return fmt.Errorf("sandbox: resolve this run's tool-host socket dir: %w", rErr)
			}
			if realSource != realTrusted {
				return fmt.Errorf("sandbox: mount %q source %q is not this run's own tool-host socket dir (want %q)", m.Destination, m.Source, trusted)
			}
		case TrustedMemoryDestination:
			// This launch's own materialized entity-memory tree (optional — absent
			// on a resume whose staging dir was swept). Same validate-not-override
			// contract, and the same two properties, as the step-skill case above:
			// read-only is REQUIRED, since an rw bind would let the agent rewrite
			// orchestrator-owned state outside its run tree — and here it would also
			// let one step forge the handoff a later step reads as fact.
			if err := requireReadOnlyMount(m); err != nil {
				return err
			}
			if conversationID == "" {
				return fmt.Errorf("sandbox: mount %q requires a run id to derive this run's own staging dir", m.Destination)
			}
			// Resolve the staging BASE and re-derive, never the per-run path whole
			// — see the step-skill case for why: resolving the whole path would
			// follow a symlink planted at the per-run entry and then agree with the
			// equally-followed source, waving through a read-only bind of anywhere
			// on the host.
			realMemBase, rErr := realPath(MemoryStagingBase())
			if rErr != nil {
				return fmt.Errorf("sandbox: resolve entity-memory staging base: %w", rErr)
			}
			trustedMem := filepath.Join(realMemBase, conversationID)
			if realSource != trustedMem {
				return fmt.Errorf("sandbox: mount %q source %q is not this run's own entity-memory staging dir (want %q)", m.Destination, m.Source, trustedMem)
			}
		default:
			if !hasScope {
				return fmt.Errorf("sandbox: mount %q source %q is not a recognized broker-global/per-run mount, and this run's worktree carries no org scope to authorize it", m.Destination, m.Source)
			}
			rel, relErr := filepath.Rel(orgPrefix, realSource)
			if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("sandbox: mount %q source %q is outside this run's own org tree %q", m.Destination, m.Source, orgPrefix)
			}
		}
	}
	return nil
}

// requireReadOnlyMount enforces that a mount is read-only: "ro" present and
// "rw" absent. The generic option check above only bounds the option SET; a
// destination whose whole purpose is read-only delivery of TF-authored content
// needs the stronger form, so a caller can never quietly upgrade it to writable.
func requireReadOnlyMount(m Mount) error {
	ro := false
	for _, o := range m.Options {
		if o == "rw" {
			return fmt.Errorf("sandbox: mount %q must be read-only, got %q", m.Destination, o)
		}
		if o == "ro" {
			ro = true
		}
	}
	if !ro {
		return fmt.Errorf("sandbox: mount %q must carry the \"ro\" option", m.Destination)
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
	// ConversationID only seeds the bundle dir's grep prefix + buildSpec's
	// required-field check; the unique key is ContainerID. Fall back to it
	// so an empty (but otherwise valid) ConversationID doesn't fail the build.
	conversationID := p.ConversationID
	if conversationID == "" {
		conversationID = p.ContainerID
	}
	cfg := Config{
		ConversationID: conversationID,
		Worktree:       p.Worktree,
		Argv:           p.Args,
		Env:            envVarsToStrings(p.Env),
		ExtraMounts:    p.Mounts,
		MemoryLimitMB:  p.MemoryLimitMB,
		Rlimits:        p.Rlimits,
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
