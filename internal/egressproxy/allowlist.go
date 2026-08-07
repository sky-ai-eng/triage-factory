package egressproxy

import "strings"

// DefaultRegistryHosts is the built-in v0 egress allowlist (TFAC-567):
// the public package registries for the toolchains actually installed
// in the sandbox rootfs (internal/sandbox rootfs.go apkPackages —
// nodejs/npm, go, python3). Least-privilege: every entry must be
// justified by an in-sandbox consumer, so ecosystems the rootfs
// doesn't ship (cargo, rubygems, ...) stay off the list until a
// rootfs variant adds their toolchain.
//
// Deliberately absent:
//   - github.com / codeload.github.com — git egress goes through the
//     authenticated per-run git proxy with its Authorize gate; an
//     allowlist entry here would be an unauthenticated bypass around it.
//   - dl-cdn.alpinelinux.org — the agent isn't root in-sandbox, so
//     apk add can't work anyway.
//
// TFAC-408 replaces this constant with per-profile data resolved by
// the spawner; the proxy's shape doesn't change.
func DefaultRegistryHosts() []string {
	return []string{
		// npm / pnpm
		"registry.npmjs.org",
		// Go modules + checksum database
		"proxy.golang.org",
		"sum.golang.org",
		// PyPI
		"pypi.org",
		"files.pythonhosted.org",
	}
}

// normalizeHost canonicalizes a hostname for allowlist matching:
// lowercase, trailing dot stripped (DNS root-anchored form
// "registry.npmjs.org." must match its bare spelling).
func normalizeHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

// newHostSet builds the case/dot-normalized lookup set the Server
// matches CONNECT targets against. Exact hostname matching only — no
// wildcards in v0 (pattern support arrives with the per-profile
// allowlists, where it can be designed against real customer input).
func newHostSet(hosts []string) map[string]struct{} {
	set := make(map[string]struct{}, len(hosts))
	for _, h := range hosts {
		if n := normalizeHost(h); n != "" {
			set[n] = struct{}{}
		}
	}
	return set
}
