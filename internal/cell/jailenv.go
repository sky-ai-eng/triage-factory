package cell

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	"github.com/sky-ai-eng/triage-factory/internal/proxy/egressrelay"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The in-jail paths the jail's environment names. Every backend presents
// them at these paths, however it delivers what is behind them; the rest of
// the jail filesystem contract is in specs/sandbox-backends/
// §4.1. internal/sandbox's broker-side constants for the same paths are
// these, so the broker validates mounts against the paths the environment
// points at.
const (
	// WorkDir is where the run tree is presented, the tool host's working
	// directory and the agent's HOME.
	WorkDir = "/work"
	// TFBinaryPath is where the TF binary is presented. The git hooks find it
	// through githooks.BinEnvVar, and prompts tell the agent to invoke it
	// here.
	TFBinaryPath = "/usr/local/bin/triagefactory"
	// CABundlePath is where the run's CA bundle is presented, read-only, and
	// what every trust variable in caBundleEnv names. The sidecar writes the
	// bundle: the host's system roots, the egress interceptor's CA for this
	// engagement, and the gh injector's leaf when that channel is on.
	CABundlePath = "/run/tf-ca-bundle.crt"
	// AllowedSignersPath is where the run's allowed-signers file is
	// presented, read-only: the gpg.ssh.allowedSignersFile a run that signs
	// its commits checks them against. The sidecar writes it, listing the
	// run's signer's public keys, and empty for a run that does not sign.
	AllowedSignersPath = "/run/tf-allowed-signers"
)

// goToolchainEnvKey / goToolchainEnvValue are the Go toolchain policy every
// jail runs with. Named so the assembler can drop a caller-supplied copy by
// the same name before appending its own.
const (
	goToolchainEnvKey   = "GOTOOLCHAIN"
	goToolchainEnvValue = "auto"
)

// JailEnv is the environment the tool host runs with in spec's jail, and so
// what every command the agent runs inherits: the same on every backend.
// Property B holds by construction: nothing here or in spec's inputs is a
// real credential. spec.Env carries run-scoped metadata and spec.ProxyEnv
// the sidecar's proxy coordinates and per-run placeholders.
//
// The order is load-bearing where keys could repeat, because duplicate keys
// resolve first-wins on Linux (getenv, and Go's runtime, take the first
// match): the base comes first and spec.Env is filtered against it, and the
// proxy env comes after spec.Env, so the keys the proxy env owns are dropped
// from spec.Env too.
func JailEnv(spec LaunchSpec) []string {
	env := baseEnv(translateEnv(spec.Env, spec.RunTree))
	env = append(env, githooks.BinEnvVar+"="+TFBinaryPath)
	env = append(env, spec.ProxyEnv...)
	env = append(env, caBundleEnv()...)
	return append(env, ghChannelEnv(spec.GHChannel)...)
}

// baseEnv is the jail's floor followed by extraEnv, less the keys the floor
// and the proxy env own.
//
// The floor must never set GIT_CONFIG_COUNT or any GIT_CONFIG_* entry. The
// jail's git config arrives in the proxy env as one indexed block, which owns
// GIT_CONFIG_COUNT outright and numbers from 0; a second count would make
// which one git reads platform-dependent and silently drop either the hooks
// entry or the proxy routing. A feature that needs jail git config folds its
// pairs into that block.
func baseEnv(extraEnv []string) []string {
	base := []string{
		// /opt/tf/bin leads so the TF-pinned gh (presented there when the
		// real-gh channel is on) wins over any gh a custom profile image ships
		// at /usr/local/bin or /usr/bin. An empty directory on PATH is a no-op.
		"PATH=/opt/tf/bin:/usr/local/bin:/usr/bin:/bin",
		"HOME=" + WorkDir,
		"TERM=xterm",
		// The rootfs installs Go from alpine's apk, whose go.env bakes in
		// GOTOOLCHAIN=local. Then a repo whose go.mod floor is newer than the
		// packaged Go cannot build at all, and the agent, who is not root and
		// has no vendor download host on the allowlist, has no way forward.
		// The upstream default makes the floor self-healing: cmd/go fetches
		// the toolchain as an ordinary module (golang.org/toolchain) through
		// the run's fetch relay, checksum-verified like any other. No new
		// host and no new capability.
		goToolchainEnvKey + "=" + goToolchainEnvValue,
		runmode.SandboxMarkerEnvVar + "=" + runmode.SandboxMarkerEnvValue,
	}
	// Dropped from extraEnv, each because a caller's copy would shadow or
	// duplicate the authoritative one:
	//   - the sandbox marker, which only its writers may set, so that an
	//     exact-match read keeps meaning "a sandbox put me here";
	//   - GOTOOLCHAIN, which the host very plausibly carries as "local"
	//     (alpine's go.env), and whose base value must win;
	//   - the fetch-relay catalog's keys (GOPROXY today), which the proxy env
	//     sets after extraEnv; under first-wins an inherited GOPROXY would
	//     point the jail's cmd/go at a host the allowlist does not carry. The
	//     keys come from the catalog, so a new entry's key is covered the
	//     moment it exists.
	drop := append([]string{runmode.SandboxMarkerEnvVar, goToolchainEnvKey}, egressrelay.CatalogEnvKeys()...)
	return append(base, withoutKeys(extraEnv, drop)...)
}

// caBundleEnv points every toolchain in the rootfs at the run's CA bundle,
// each through the variable it reads: SSL_CERT_FILE for Go, curl, OpenSSL and
// pip's system-store default, NODE_EXTRA_CA_CERTS for Node and so npm and
// pnpm, PIP_CERT so pip reads it whichever store its version defaults to, and
// CARGO_HTTP_CAINFO for cargo. Cargo's HTTP client honors SSL_CERT_FILE too,
// but only by way of how it finds its roots; CARGO_HTTP_CAINFO is the setting
// cargo itself documents, so a toolchain bump cannot quietly stop it
// trusting the bundle. The bundle is the system roots plus this run's own
// certificates, so none of these narrows what a client trusted before. A
// toolchain added to the rootfs that reads none of them fails the
// interception integration test until its variable is here.
func caBundleEnv() []string {
	return []string{
		"SSL_CERT_FILE=" + CABundlePath,
		"NODE_EXTRA_CA_CERTS=" + CABundlePath,
		"PIP_CERT=" + CABundlePath,
		"CARGO_HTTP_CAINFO=" + CABundlePath,
	}
}

// ghChannelEnv points the real gh binary at the per-run injector: GH_HOST (the
// injector's host:port; gh forces https and verifies against the CA bundle,
// which carries the injector's leaf), GH_ENTERPRISE_TOKEN (the per-run
// placeholder gh sends, which the injector replaces with the real token), and
// the prompt and update-notifier suppressors so gh runs non-interactively and
// never checks for releases. Empty when the channel is off.
func ghChannelEnv(gc *GHChannel) []string {
	if gc == nil || gc.Host == "" {
		return nil
	}
	return []string{
		"GH_HOST=" + gc.Host,
		"GH_ENTERPRISE_TOKEN=" + gc.Token,
		"GH_PROMPT_DISABLED=1",
		"GH_NO_UPDATE_NOTIFIER=1",
	}
}

// translateEnv rewrites env values that are absolute host paths under
// runTree to the same paths under WorkDir, where the jail sees the run tree.
// The delegate sets TRIAGE_FACTORY_CONVERSATION_ROOT to the run tree's host
// path, for one; in the jail that path does not exist.
//
// Only absolute values are candidates; IDs, flags and "owner/repo" strings
// pass through. An absolute path outside runTree is dropped: the jail cannot
// reach it, and a dangling host path is more confusing than a missing
// variable. An entry with no '=' passes through verbatim.
func translateEnv(env []string, runTree string) []string {
	if len(env) == 0 {
		return env
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key, val, ok := strings.Cut(kv, "=")
		if !ok || val == "" || !filepath.IsAbs(val) {
			out = append(out, kv)
			continue
		}
		if runTree == "" {
			continue
		}
		rel, err := filepath.Rel(runTree, val)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		out = append(out, key+"="+filepath.Join(WorkDir, rel))
	}
	return out
}

// withoutKeys is env less every KEY=... entry whose KEY is in keys, matched
// case-sensitively as POSIX env names are. An entry with no '=' is kept.
func withoutKeys(env, keys []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key, _, ok := strings.Cut(kv, "=")
		if ok && slices.Contains(keys, key) {
			continue
		}
		out = append(out, kv)
	}
	return out
}
