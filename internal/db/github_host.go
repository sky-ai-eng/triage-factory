package db

import "strings"

// NormalizeGitHubHost trims a trailing slash so the (user_id,
// github_base_url) key in user_github_identities matches regardless of
// whether a caller passes "https://github.com" or "https://github.com/".
// Reads and writes both normalize, so they agree by construction even when
// org_settings stored the raw form. Kept minimal (no lowercasing) — GHES
// path-based hosts are case-sensitive below the authority.
//
// Shared by the SQLite and Postgres usersStore impls so the rule has one
// home (SKY-396); the Jira/Linear sibling tables (SKY-397/398) add their
// own scope-normalization rather than reusing this GitHub-specific one.
func NormalizeGitHubHost(host string) string { return strings.TrimRight(host, "/") }

// DefaultGitHubHost is the public github.com web base. An org that leaves
// github_base_url unset resolves identities under this host — notably the
// GoTrue GitHub OAuth login-claim binds the identity row to it literally.
const DefaultGitHubHost = "https://github.com"

// EffectiveGitHubHost resolves an org's configured github_base_url to the host
// identities are actually keyed under: an empty setting (the common github.com
// case) means DefaultGitHubHost, and the result is trailing-slash-trimmed to
// match NormalizeGitHubHost (what the stores key on). Read-side callers
// building a reverse identity lookup must use this rather than the raw setting
// — an empty setting would otherwise look up host="" and miss rows captured
// under "https://github.com".
func EffectiveGitHubHost(orgBase string) string {
	if h := NormalizeGitHubHost(orgBase); h != "" {
		return h
	}
	return DefaultGitHubHost
}
