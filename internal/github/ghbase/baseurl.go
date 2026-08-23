// Package ghbase holds the GitHub base-URL derivations — the web base an org
// configures, and the REST API mount that follows from it. It is a leaf:
// stdlib only, so every package that needs the derivation can import it
// (internal/auth and internal/db both sit below internal/github, which imports
// them). One copy, no lockstep to keep.
package ghbase

import (
	"net/url"
	"strings"
)

// DefaultBaseURL is the public github.com web base. An org with an empty
// GitHubBaseURL setting (the common case) resolves to this.
const DefaultBaseURL = "https://github.com"

// ResolveBaseURL normalizes a per-org GitHub base URL into the user-facing
// web base github.NewClient expects. Empty (not configured) maps to
// github.com; a GHES base is returned trimmed of trailing slashes.
func ResolveBaseURL(orgBase string) string {
	if orgBase != "" {
		return strings.TrimRight(orgBase, "/")
	}
	return DefaultBaseURL
}

// APIBase derives the REST API base from a user-facing GitHub web base,
// covering the three host classes TF supports:
//
//   - github.com           → https://api.github.com (public, fixed).
//   - <tenant>.ghe.com      → https://api.<tenant>.ghe.com. GitHub Enterprise
//     Cloud *data residency* puts the API on an api.* subdomain, mirroring
//     github.com→api.github.com — NOT the GHES path mount. These hosts are
//     public and per-enterprise, which is why the base URL is free-entry.
//   - everything else (GHES) → {base}/api/v3, the path-mounted REST root on
//     the same (typically private) host.
//
// This is the only API-mount derivation in the repo: the client, the App JWT
// mint endpoint, the credential validator, the reachability probe and the
// sidecar's proxies all read it, so nothing can disagree about where an org's
// API lives.
func APIBase(base string) string {
	base = strings.TrimRight(base, "/")
	if base == "" || base == DefaultBaseURL {
		return "https://api.github.com"
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		// Unparseable — fall back to the GHES path mount rather than guess a
		// public host, which keeps an odd input on the host it named.
		return base + "/api/v3"
	}
	host := u.Hostname()
	switch {
	case host == "github.com":
		return "https://api.github.com"
	case strings.HasSuffix(host, ".ghe.com"):
		// Data residency: the API host is api.<tenant>.ghe.com. Built from
		// u.Host (not host) so an explicit port — rare but valid — is kept. If
		// the caller already handed us that api.* host (a stored API base, or a
		// paste of the wrong field), return it as-is rather than double-prefix
		// it into api.api.<tenant>.ghe.com.
		if strings.HasPrefix(host, "api.") {
			return u.Scheme + "://" + u.Host
		}
		return u.Scheme + "://api." + u.Host
	default:
		return base + "/api/v3"
	}
}
