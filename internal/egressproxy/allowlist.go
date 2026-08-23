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
// One exception to the consumer rule: an entry whose traffic has moved
// to the fetch-relay lane (internal/egressrelay) may stay here, marked
// VESTIGIAL in its comment, as a deliberate one-release degraded path
// before removal — the Go entries below are the current case. Don't
// delete a vestigial entry under the consumer rule; its comment names
// when it goes.
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
		// Go modules + checksum database. Both are now VESTIGIAL for a
		// sandboxed run: GOPROXY points cmd/go at the per-run fetch relay
		// (internal/egressrelay, go-modules catalog entry), which serves
		// the module protocol and the /sumdb/ arm from the host, so the
		// jail no longer dials either host. They stay for one release as
		// the degraded path if that env plumbing is ever absent — small
		// modules still resolve inline, where a jail with neither entry
		// nor relay would fail at the first fetch with nothing to point
		// at. Remove once the relay has production miles.
		//
		// Note the allowlist alone was never sufficient here: the public
		// proxy 302s large zips (and every toolchain) to
		// storage.googleapis.com, a shared storage namespace this list
		// must never carry — which is why the relay lane exists.
		"proxy.golang.org",
		"sum.golang.org",
		// PyPI
		"pypi.org",
		"files.pythonhosted.org",
	}
}

// deniedHostGuidance returns the remedy sentence appended to the
// client-visible denial reason for hosts whose refusal has a better answer
// than "not on the list". These are the hosts the "Deliberately absent" note
// above names, plus the API host itself: a CONNECT aimed at any of them is
// almost always an agent (or its tooling) reaching for GitHub directly when
// the run already has an audited channel for exactly that — most commonly
// `gh` following an absolute api.github.com URL out of a response body
// (`gh run view`'s jobs fetch, the release-asset verbs), which bypasses the
// GH_HOST base-URL redirect and lands here. Empty for every other host: a
// registry nobody allowlisted has no sanctioned alternative to point at, and
// inventing one would send the agent to do useless work.
//
// The verbs are named without an invocation prefix, like ghwrite's refusal
// explanations, because the spelling differs per runtime (`tfac gh …` in the
// native jail, `<binary> exec gh …` under the SDK) and the agent's own
// prompt teaches the right one. host must already be normalizeHost'd.
func deniedHostGuidance(host string) string {
	switch host {
	case "api.github.com", "uploads.github.com":
		return "GitHub API access goes through this run's credential channel, " +
			"which direct connections (including gh following an absolute URL from an API response) bypass. " +
			"Use the Triage Factory exec verbs instead — CI logs: `gh actions download-logs <run_id>`"
	case "github.com", "codeload.github.com":
		return "git reaches GitHub through this run's preconfigured credential proxy. " +
			"Use git with the worktree's existing remotes, or the workspace exec verbs to add a checkout"
	}
	return ""
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
