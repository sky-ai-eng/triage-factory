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
