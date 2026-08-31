// Package runmode owns the deployment-posture flag the binary reads
// once at startup. Multiple downstream concerns consume it — D2 stores
// dispatch SQLite vs Postgres, D4b paths branch on it, D5 secret
// store picks keychain vs Vault, D7 auth middleware mounts only in
// multi, D8 agent-runner sandbox lifecycle differs by mode, D13's
// container image bakes TF_MODE=multi at build time. Centralizing
// the flag here (rather than inside any one consumer like internal/
// paths) keeps the import lines reading naturally at every call
// site: a store-wiring file checking `runmode.Current() ==
// runmode.ModeMulti` reads correctly, where `paths.Current()` would
// be a name pun.
//
// Mode is read once at process startup from the TF_MODE environment
// variable via InitFromEnv(), called as the first thing in main.go
// before any subsystem touches a path or opens a DB. Default (empty
// TF_MODE) is ModeLocal so existing local installs see no behavior
// change.
//
// LocalDefaultOrgID also lives here even though it's not strictly mode
// state — it's the synthetic org-context value local-mode passes
// everywhere D2/D4b/D9 expect a real orgID. Belongs with the mode
// primitives because it only makes sense in concert with them.
package runmode

import (
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
)

// Mode names the runtime mode the binary is operating in. Set once
// at startup and never reassigned outside tests.
type Mode string

const (
	// ModeLocal is the single-binary, single-user, SQLite-backed mode
	// the current product ships as. Default when TF_MODE is unset.
	ModeLocal Mode = "local"

	// ModeMulti is the multi-tenant, Postgres-backed mode the
	// architecture spec (docs/for-agents/multi-tenant-architecture.html) targets
	// for v1. The binary boots into ModeMulti when TF_MODE=multi;
	// downstream tickets (D2 store dispatch, D3 schema, D4b
	// resolvers) consume the flag.
	ModeMulti Mode = "multi"
)

// Local-mode sentinel identity values. Four constants — one each for
// org, team, user, and agent — used as the canonical local-mode
// identity values at every API entry point. These were previously
// synthetic runtime constants with no DB row backing them; now
// the SQLite migration inserts one row per sentinel into
// orgs/teams/users so FKs from every resource table have a real
// target. The byte values here MUST match the migration's INSERTs
// verbatim — TestBootstrapLocalTenancy_ConstantsMatchRows asserts the
// equivalence so any drift fails CI rather than producing silently-
// broken FKs at runtime.
//
// The nil-shape UUIDs (00000000-...000N) are deliberately chosen for
// log visibility — a row id starting with thirty zeros is instantly
// recognizable as "the local-mode sentinel" rather than as a random
// tenant. gen_random_uuid() never produces these.
const (
	LocalDefaultOrgID   = "00000000-0000-0000-0000-000000000001"
	LocalDefaultTeamID  = "00000000-0000-0000-0000-000000000010"
	LocalDefaultUserID  = "00000000-0000-0000-0000-000000000100"
	LocalDefaultAgentID = "00000000-0000-0000-0000-000000001000"
)

// currentMode + initialized + modeMu form the package's mutable state.
// Reads through Current() take an RLock — cheap, contention-free for
// readers — so that SetForTest (which writes from test goroutines, in
// parallel suites) is provably race-free against Current()'s reads.
// Production reads are infrequent enough that the RLock overhead is
// noise; we trade a few nanoseconds for the simpler reasoning.
//
// initialized tracks whether Init has been called in production. It
// starts false; Init flips it true. SetForTest snapshots both fields
// and restores them via t.Cleanup, so tests can flip state freely
// without leaking into other tests or into a subsequent Init call.
var (
	currentMode Mode = ModeLocal
	initialized bool
	modeMu      sync.RWMutex
)

// Current returns the active mode. Always safe to call from any
// goroutine.
func Current() Mode {
	modeMu.RLock()
	defer modeMu.RUnlock()
	return currentMode
}

// Init sets the process-wide mode. Production code calls this exactly
// once, at the top of main(), via InitFromEnv. The contract:
//
//   - First call with a valid mode → sets currentMode, returns nil.
//   - Subsequent call with the SAME mode → idempotent no-op, returns
//     nil (so a stray double-init from cmd dispatch wouldn't fatal).
//   - Subsequent call with a DIFFERENT mode → returns an error
//     without mutating state. Catches accidental "let me re-init
//     mid-run" bugs that would otherwise silently flip behavior under
//     subsystems that already cached the original value.
//   - Unknown Mode value → returns an error.
//
// Tests should use SetForTest instead so the cleanup restores the
// previous (initialized, currentMode) pair. SetForTest also flips
// initialized=true so a test's downstream Init calls follow the
// idempotent / conflict branches above (predictable).
func Init(m Mode) error {
	if m != ModeLocal && m != ModeMulti {
		return fmt.Errorf("unknown mode %q (want %q or %q)", m, ModeLocal, ModeMulti)
	}
	modeMu.Lock()
	defer modeMu.Unlock()
	if initialized {
		if currentMode == m {
			return nil
		}
		return fmt.Errorf("already initialized as %q; cannot re-init as %q", currentMode, m)
	}
	currentMode = m
	initialized = true
	return nil
}

// InitFromEnv reads TF_MODE from the environment and initializes the
// mode accordingly. Empty / unset → ModeLocal (so existing local
// installs see no change). Any other value falls through to
// ModeFromEnv's parsing, which errors on unknown values.
//
// Call as the first thing in main.go and every cmd/*/Handle()
// entrypoint, before any subsystem touches a path or opens a DB.
func InitFromEnv() error {
	m, err := ModeFromEnv(os.Getenv("TF_MODE"))
	if err != nil {
		return err
	}
	return Init(m)
}

// ModeFromEnv parses a TF_MODE env-var string into a Mode. Empty
// string maps to ModeLocal (the safe default); known values map to
// their constants; anything else errors. Case-insensitive (any
// mixed-case spelling of "local" / "multi" works) but not
// whitespace-tolerant — operators that pass " local " typo'd a
// space and we surface it rather than silently accept.
func ModeFromEnv(s string) (Mode, error) {
	switch strings.ToLower(s) {
	case "":
		return ModeLocal, nil
	case "local":
		return ModeLocal, nil
	case "multi":
		return ModeMulti, nil
	default:
		return "", fmt.Errorf("unknown TF_MODE=%q (want %q or %q, or empty for local)", s, ModeLocal, ModeMulti)
	}
}

// Org-creation policy. A single boolean knob — TF_PREVENT_ORG_CREATION
// — that governs whether a self-service user who lands authenticated
// with zero org memberships may create their own org.
//
// This replaces the earlier three-way join-policy enum
// (TF_DEFAULT_JOIN_POLICY). The product has exactly one onboarding
// entry for every zero-membership user — the "create your org / wait
// for an invite" landing — and signup never provisions a tenant
// silently. Org creation is always a deliberate user action (the
// onboarding "Start your Factory" CTA → the create-org flow). This
// flag only toggles whether that page's create affordance is enabled:
// the default (creation allowed) is right for a SaaS deployment and
// unconfigured self-hosts, while a locked-down self-host sets
// TF_PREVENT_ORG_CREATION=true to gate access on an admin invite,
// leaving only the "wait for an invite" path.
//
// Local mode ignores the knob entirely: a local install is N=1 with a
// pre-provisioned sentinel org and never renders the onboarding entry.
var (
	// orgCreationPrevented is the parsed TF_PREVENT_ORG_CREATION value.
	// false (the default) means self-service org creation is allowed.
	orgCreationPrevented   bool
	orgCreationInitialized bool
	orgCreationMu          sync.RWMutex
)

// OrgCreationEnabled reports whether self-service users may create
// their own org. Safe from any goroutine. Defaults to true even before
// InitOrgCreationFromEnv runs, so callers during boot (and local-mode
// tests that never init it) get the permissive default.
func OrgCreationEnabled() bool {
	orgCreationMu.RLock()
	defer orgCreationMu.RUnlock()
	return !orgCreationPrevented
}

// InitOrgCreationPrevented sets the process-wide org-creation toggle.
// Same idempotency contract as Init: first call sets state and returns
// nil; a subsequent call with the same value is a no-op; a subsequent
// call with a different value returns an error without mutating state.
func InitOrgCreationPrevented(prevented bool) error {
	orgCreationMu.Lock()
	defer orgCreationMu.Unlock()
	if orgCreationInitialized {
		if orgCreationPrevented == prevented {
			return nil
		}
		return fmt.Errorf("org-creation policy already initialized as prevented=%t; cannot re-init as prevented=%t", orgCreationPrevented, prevented)
	}
	orgCreationPrevented = prevented
	orgCreationInitialized = true
	return nil
}

// InitOrgCreationFromEnv reads TF_PREVENT_ORG_CREATION from the
// environment and initializes the toggle. Empty / unset → creation
// allowed (the safe default for a SaaS deployment + unconfigured self-hosts).
//
// Only meaningful in multi mode — local mode never consults it. main.go
// gates the call accordingly.
func InitOrgCreationFromEnv() error {
	prevented, err := ParsePreventOrgCreation(os.Getenv("TF_PREVENT_ORG_CREATION"))
	if err != nil {
		return err
	}
	return InitOrgCreationPrevented(prevented)
}

// ParsePreventOrgCreation parses a TF_PREVENT_ORG_CREATION env-var
// string into a bool. Empty maps to false (creation allowed). Accepts
// the usual boolean spellings (case-insensitive, whitespace-trimmed);
// anything else errors so a typo in .env surfaces loudly at boot rather
// than silently degrading to the wrong default.
func ParsePreventOrgCreation(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "0", "false", "f", "no", "n", "off":
		return false, nil
	case "1", "true", "t", "yes", "y", "on":
		return true, nil
	default:
		return false, fmt.Errorf("unknown TF_PREVENT_ORG_CREATION=%q (want a boolean, e.g. true or false)", s)
	}
}

// Trusted-proxy allowlist + client-IP capture policy (TFAC-488).
//
// clientIP (internal/server) feeds three consumers — the session
// forensics row (sessions.ip_addr), the SOC2 authentication audit log
// (auth_events.ip_address), and the pre-auth per-IP rate limiter (a
// security control). X-Forwarded-For is *appended* by each proxy, never
// overwritten, so its leftmost entry is whatever the original caller
// typed — fully spoofable. Two env knobs make the extraction sound across
// both deployment shapes (a managed SaaS edge we control; an arbitrary
// self-host topology we don't):
//
//   - TF_TRUSTED_PROXY_CIDR — comma-separated CIDRs of the trusted
//     upstream proxies (a bare IP is accepted, treated as a /32 or /128).
//     When the direct peer (RemoteAddr) is in this set, clientIP walks XFF
//     right→left, skips trusted hops, and takes the first untrusted entry
//     as the client. When the peer is NOT in it (direct exposure), XFF is
//     ignored — it's forgeable — and RemoteAddr wins. Unset → RemoteAddr
//     only, XFF always ignored: the secure default, never spoofable (worst
//     case records the LB IP, never an attacker-chosen one). A CIDR
//     allowlist rather than a hop count because chain length varies
//     (health checks, internal calls, websocket upgrades). The literal
//     "none" (case-insensitive) is a synonym for TF_CAPTURE_CLIENT_IP=false.
//
//   - TF_CAPTURE_CLIENT_IP — boolean, default true. false opts out of IP
//     capture entirely: clientIP returns "" so sessions.ip_addr /
//     auth_events.ip_address store NULL (the nullable schema already
//     supports it), for data-minimization-conscious self-hosts. The per-IP
//     rate limiter then collapses to a single global bucket — an accepted
//     tradeoff of the opt-out.
//
// Read once at startup (multi-mode wireAuth) and never reassigned outside
// tests. Local mode never inits these — it's N=1 and clientIP's three
// consumers are all multi-mode — so the safe defaults (capture on, no
// trusted proxies → RemoteAddr only) apply and are moot.
var (
	trustedProxies         []*net.IPNet
	captureClientIP        = true
	trustedProxyConfigured bool
	clientIPMu             sync.RWMutex
)

// TrustedProxies returns the parsed trusted-proxy CIDR allowlist. The
// returned slice is the shared, init-time-frozen value — callers read it
// (clientIP iterates it per request) and must not mutate it. Safe from any
// goroutine.
func TrustedProxies() []*net.IPNet {
	clientIPMu.RLock()
	defer clientIPMu.RUnlock()
	return trustedProxies
}

// CaptureClientIP reports whether the deployment captures client IPs at
// all. false (TF_CAPTURE_CLIENT_IP=false, or TF_TRUSTED_PROXY_CIDR=none)
// makes clientIP return "" so the IP columns store NULL. Defaults to true
// even before init runs, so local mode and boot-time callers get the
// permissive default. Safe from any goroutine.
func CaptureClientIP() bool {
	clientIPMu.RLock()
	defer clientIPMu.RUnlock()
	return captureClientIP
}

// TrustedProxyConfigured reports whether TF_TRUSTED_PROXY_CIDR named at
// least one CIDR. Used by the multi-mode startup warning: capturing IPs
// with no allowlist means X-Forwarded-For is ignored and every request is
// attributed to its direct peer. Safe from any goroutine.
func TrustedProxyConfigured() bool {
	clientIPMu.RLock()
	defer clientIPMu.RUnlock()
	return trustedProxyConfigured
}

// InitClientIPPolicyFromEnv parses TF_TRUSTED_PROXY_CIDR + TF_CAPTURE_CLIENT_IP
// and installs the process-wide client-IP policy. A malformed CIDR or
// non-boolean toggle returns an error so a typo in .env fails boot loudly
// rather than silently degrading to the wrong (and security-relevant)
// default. Called once from the multi-mode auth-wiring path.
func InitClientIPPolicyFromEnv() error {
	nets, none, err := ParseTrustedProxyCIDR(os.Getenv("TF_TRUSTED_PROXY_CIDR"))
	if err != nil {
		return err
	}
	capture, err := ParseCaptureClientIP(os.Getenv("TF_CAPTURE_CLIENT_IP"))
	if err != nil {
		return err
	}
	clientIPMu.Lock()
	defer clientIPMu.Unlock()
	// Either kill switch ("none" sentinel or an explicit false) disables
	// capture; the CIDR list is moot once capture is off but stored anyway.
	captureClientIP = capture && !none
	trustedProxies = nets
	trustedProxyConfigured = len(nets) > 0
	return nil
}

// ParseTrustedProxyCIDR parses a TF_TRUSTED_PROXY_CIDR value into the
// trusted-proxy allowlist. Empty → (nil, false, nil): no allowlist. The
// literal "none" (case-insensitive, trimmed) → (nil, true, nil): the
// capture kill switch. Otherwise each comma-separated entry is parsed as a
// CIDR, or as a bare IP (promoted to a /32 or /128 host route); an
// unparseable entry errors. A pure function so it's unit-testable without
// touching the environment.
func ParseTrustedProxyCIDR(s string) (nets []*net.IPNet, none bool, err error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return nil, false, nil
	}
	if strings.EqualFold(trimmed, "none") {
		return nil, true, nil
	}
	for _, raw := range strings.Split(trimmed, ",") {
		part := strings.TrimSpace(raw)
		if part == "" {
			continue
		}
		if _, ipnet, perr := net.ParseCIDR(part); perr == nil {
			nets = append(nets, ipnet)
			continue
		}
		// Bare IP → host route. Re-parse through ParseCIDR with an explicit
		// full-length mask so the resulting *net.IPNet is byte-identical to
		// a CIDR parse (4-byte v4 / 16-byte v6), which net.IPNet.Contains
		// relies on for correct matching.
		if ip := net.ParseIP(part); ip != nil {
			suffix := "/32"
			if ip.To4() == nil {
				suffix = "/128"
			}
			if _, ipnet, perr := net.ParseCIDR(part + suffix); perr == nil {
				nets = append(nets, ipnet)
				continue
			}
		}
		// "none" only disables capture when it's the WHOLE value (handled
		// above); buried in a list it's an error, but say why rather than
		// calling it a malformed CIDR.
		if strings.EqualFold(part, "none") {
			return nil, false, fmt.Errorf(`invalid TF_TRUSTED_PROXY_CIDR entry %q: "none" is a capture kill switch and must appear alone — use TF_CAPTURE_CLIENT_IP=false to disable capture alongside a CIDR set`, part)
		}
		return nil, false, fmt.Errorf("invalid TF_TRUSTED_PROXY_CIDR entry %q (want a CIDR like 10.0.0.0/8 or an IP)", part)
	}
	return nets, false, nil
}

// ParseCaptureClientIP parses a TF_CAPTURE_CLIENT_IP env-var string into a
// bool. Empty maps to true (capture on — the default), the inverse of
// ParsePreventOrgCreation's default. Accepts the usual boolean spellings
// (case-insensitive, whitespace-trimmed); anything else errors so a typo
// surfaces loudly at boot.
func ParseCaptureClientIP(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "1", "true", "t", "yes", "y", "on":
		return true, nil
	case "0", "false", "f", "no", "n", "off":
		return false, nil
	default:
		return false, fmt.Errorf("unknown TF_CAPTURE_CLIENT_IP=%q (want a boolean, e.g. true or false)", s)
	}
}
