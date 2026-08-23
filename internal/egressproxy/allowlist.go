package egressproxy

import (
	"net/url"
	"strings"
)

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

// GitHubHosts names the hostnames that make up one run's configured GitHub
// deployment, split by which credential channel already serves them: API for
// the REST/upload side (the gh injector; the exec verbs), Git for the web/git
// side (the run's git proxy; the workspace verbs). The split only picks which
// remedy a denial's reason carries — membership never widens or narrows the
// allow decision, which belongs to the allowlist alone.
//
// The zero value means "the public github.com family": New fills it in, so a
// caller that threads nothing still gets the common case explained.
type GitHubHosts struct {
	API []string
	Git []string
}

// GitHubHostsForUpstreams derives GitHubHosts from the two upstream base URLs
// a run's GitHub channels are already built from — the REST API base the gh
// injector forwards to, and the git host base the git proxy forwards to. The
// hosts a denial explains are then the hosts this run's GitHub actually lives
// on, github.com and GHES alike, with no second list to drift from the
// channel configuration.
//
// Either input may be empty (a run without that channel); the empty side is
// filled from the other the way the deployment's own shape implies: a GHES
// serves API and git on one hostname, the public pair is
// api.github.com/github.com, and GHE Cloud data residency mirrors the public
// shape with an api. prefix on the tenant host. Both empty is the public
// default. The public hosts also add the sibling hostnames gh escapes to
// (uploads.github.com for release uploads, codeload.github.com for
// tarballs); a GHES serves those path-mounted on its one hostname, which is
// already in the set.
func GitHubHostsForUpstreams(apiUpstream, gitUpstream string) GitHubHosts {
	apiHost := upstreamHostname(apiUpstream)
	gitHost := upstreamHostname(gitUpstream)
	if apiHost == "" {
		if gitHost == "" || gitHost == "github.com" {
			apiHost = "api.github.com"
		} else {
			apiHost = gitHost
		}
	}
	if gitHost == "" {
		switch {
		case apiHost == "api.github.com":
			gitHost = "github.com"
		case strings.HasPrefix(apiHost, "api.") && strings.HasSuffix(apiHost, ".ghe.com"):
			gitHost = strings.TrimPrefix(apiHost, "api.")
		default:
			gitHost = apiHost
		}
	}
	gh := GitHubHosts{API: []string{apiHost}, Git: []string{gitHost}}
	if apiHost == "api.github.com" {
		gh.API = append(gh.API, "uploads.github.com")
	}
	if gitHost == "github.com" {
		gh.Git = append(gh.Git, "codeload.github.com")
	}
	return gh
}

// upstreamHostname extracts the normalized hostname of an upstream base URL,
// tolerating a bare host with no scheme. Empty or unparseable in, empty out —
// the caller reads an empty answer as an absent input and fills it from the
// sibling side, the only honest fallback available. Defensive only: these
// strings come from the same configuration that already built the run's
// working credential channels.
func upstreamHostname(upstream string) string {
	upstream = strings.TrimSpace(upstream)
	if upstream == "" {
		return ""
	}
	if u, err := url.Parse(upstream); err == nil && u.Hostname() != "" {
		return normalizeHost(u.Hostname())
	}
	if u, err := url.Parse("https://" + upstream); err == nil && u.Hostname() != "" {
		return normalizeHost(u.Hostname())
	}
	return ""
}

// deniedHostGuidance returns the remedy sentence appended to the
// client-visible denial reason when the refused host is part of this run's
// configured GitHub (Config.GitHub) — the one refusal with a better answer
// than "not on the list", because the run already has an audited channel for
// exactly what was reached for. Most commonly the request is `gh` following
// an absolute URL out of a response body (`gh run view`'s jobs fetch, the
// release-asset verbs), which bypasses the GH_HOST base-URL redirect and
// lands here. Which remedy a host gets follows from which channel serves it;
// a GHES hostname serves both sides and gets both. Empty for every other
// host: a registry nobody allowlisted has no sanctioned alternative to point
// at, and inventing one would send the agent to do useless work.
//
// The verbs are spelled under the `tfac` applet because that is the one
// invocation every reader of this refusal can run: the proxy serves only the
// sandboxed (multi-mode) cell, whose agent is the native jail with the applet
// on PATH — local mode runs no egress proxy. host must already be
// normalizeHost'd.
func (s *Server) deniedHostGuidance(host string) string {
	_, api := s.ghAPIHosts[host]
	_, git := s.ghGitHosts[host]
	switch {
	case api && git:
		return "this host serves this run's GitHub, and its API and git access go through the run's " +
			"credential channels, which direct connections (including gh following an absolute URL from " +
			"an API response) bypass. Use the Triage Factory exec verbs instead (`tfac gh --help` lists " +
			"them), and git with the worktree's existing remotes"
	case api:
		return "GitHub API access goes through this run's credential channel, " +
			"which direct connections (including gh following an absolute URL from an API response) bypass. " +
			"Use the Triage Factory exec verbs instead — `tfac gh --help` lists them"
	case git:
		return "git reaches GitHub through this run's preconfigured credential proxy. " +
			"Use git with the worktree's existing remotes, or `tfac workspace add <owner>/<repo>` for a " +
			"checkout you don't have"
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
