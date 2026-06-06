package github

import (
	"net/url"
	"strings"
)

// DefaultBaseURL is the public github.com web base. An org with an empty
// GitHubBaseURL setting (the common case) resolves to this.
const DefaultBaseURL = "https://github.com"

// ResolveBaseURL normalizes a per-org GitHub base URL into the user-facing
// web base NewClient expects. Empty (not configured) maps to github.com;
// a GHES base is returned trimmed of trailing slashes. This is the single
// canonical copy shared by the App-register flow (internal/server) and the
// credential resolver — keep new callers pointed here rather than
// re-deriving.
//
// internal/db keeps its own private copy (github_app_backfill.go) because
// importing internal/github there would form a cycle (the resolver in this
// package imports internal/db).
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
// Mirrors the derivation NewClient does internally so the App JWT mint
// endpoint and the client agree on where the API lives. internal/db keeps a
// private copy (github_app_backfill.go) that must track this — importing
// internal/github there would form a cycle.
func APIBase(base string) string {
	base = strings.TrimRight(base, "/")
	if base == "" || base == DefaultBaseURL {
		return "https://api.github.com"
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		// Unparseable — fall back to the GHES path mount rather than guess a
		// public host, preserving the historical behavior for odd inputs.
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
