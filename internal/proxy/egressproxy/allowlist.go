package egressproxy

import (
	"net/url"
	"strings"
)

// Registry is one host the tunnel admits, and the requests it may carry
// there. Every request is read-only: GET and HEAD to any path, and POST only
// to the paths in ReadPOSTPaths, where the registry answers a read that
// needs a request body.
type Registry struct {
	// Host is the exact hostname, matched after case and trailing-dot
	// normalization. No wildcards.
	Host string

	// ReadPOSTPaths are the paths on Host where a POST is a read, matched
	// exactly against the request's path with no query. Each entry is
	// probed like a host is (CLAUDE.md): it names a client in the rootfs
	// that sends it and what the registry answers there.
	ReadPOSTPaths []string
}

// DefaultRegistries is the built-in egress allowlist: the public package
// registries for the toolchains installed in the sandbox rootfs
// (internal/sandbox rootfs.go apkPackages and rustVersion). Every entry needs
// an in-sandbox consumer, so ecosystems the rootfs doesn't ship (rubygems,
// Maven, ...) stay off the list until a rootfs variant adds their toolchain.
// Go modules are not here: cmd/go fetches through the per-run fetch relay
// (internal/proxy/egressrelay), because the module proxy redirects into a
// shared storage namespace this list must never carry.
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
func DefaultRegistries() []Registry {
	return []Registry{
		// npm and pnpm. `npm install` posts the lockfile's package list to the
		// bulk advisory endpoint by default and falls back to the quick audit
		// endpoint; `pnpm audit` posts to the bulk one. Both answer with
		// advisories and store nothing.
		{
			Host: "registry.npmjs.org",
			ReadPOSTPaths: []string{
				"/-/npm/v1/security/advisories/bulk",
				"/-/npm/v1/security/audits/quick",
			},
		},
		// PyPI, for pip in a venv: `python3 -m venv` installs pip from the
		// wheel CPython bundles, and the rootfs ships no system pip.
		{Host: "pypi.org"},
		{Host: "files.pythonhosted.org"},
		// crates.io, for cargo: the sparse index cargo reads by default, and
		// the host the index's config.json names for crate downloads. The
		// API host, crates.io itself, carries publish, yank, and owner changes,
		// and stays off.
		{Host: "index.crates.io"},
		{Host: "static.crates.io"},
	}
}

// registryHosts is the hostname half of registries, the set the CA is
// name-constrained to.
func registryHosts(registries []Registry) []string {
	hosts := make([]string, 0, len(registries))
	for _, r := range registries {
		if n := normalizeHost(r.Host); n != "" {
			hosts = append(hosts, n)
		}
	}
	return hosts
}

// newRegistrySet indexes registries by normalized host, merging the read
// paths of an entry listed twice.
func newRegistrySet(registries []Registry) map[string]map[string]struct{} {
	set := make(map[string]map[string]struct{}, len(registries))
	for _, r := range registries {
		h := normalizeHost(r.Host)
		if h == "" {
			continue
		}
		paths := set[h]
		if paths == nil {
			paths = make(map[string]struct{}, len(r.ReadPOSTPaths))
			set[h] = paths
		}
		for _, p := range r.ReadPOSTPaths {
			paths[p] = struct{}{}
		}
	}
	return set
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
		switch {
		case gitHost == "" || gitHost == "github.com":
			apiHost = "api.github.com"
		case strings.HasSuffix(gitHost, ".ghe.com") && !strings.HasPrefix(gitHost, "api."):
			apiHost = "api." + gitHost
		default:
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

// newHostSet builds a case/dot-normalized hostname lookup set. Exact
// matching only, like the registry set.
func newHostSet(hosts []string) map[string]struct{} {
	set := make(map[string]struct{}, len(hosts))
	for _, h := range hosts {
		if n := normalizeHost(h); n != "" {
			set[n] = struct{}{}
		}
	}
	return set
}
