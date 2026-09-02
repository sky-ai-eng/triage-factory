package db

import (
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
)

// NormalizeGitHubHost trims a trailing slash so the (user_id,
// github_base_url) key in user_github_identities matches regardless of
// whether a caller passes "https://github.com" or "https://github.com/".
// Reads and writes both normalize, so they agree by construction even when
// org_settings stored the raw form. Kept minimal (no lowercasing) — GHES
// path-based hosts are case-sensitive below the authority.
//
// Shared by the SQLite and Postgres usersStore impls so the rule has one
// home; the Jira/Linear sibling tables add their
// own scope-normalization rather than reusing this GitHub-specific one.
func NormalizeGitHubHost(host string) string { return strings.TrimRight(host, "/") }

// EffectiveGitHubHost resolves an org's configured github_base_url to the host
// identities are actually keyed under: an empty setting means the deployment's
// default GitHub (ghbase.DefaultBaseURL — github.com unless the operator named
// another), and the result is trailing-slash-trimmed to match
// NormalizeGitHubHost (what the stores key on). Read-side callers building a
// reverse identity lookup must use this rather than the raw setting — an empty
// setting would otherwise look up host="" and miss rows captured under the
// default.
//
// The GoTrue GitHub OAuth login identity is the one host-keyed row that does
// NOT resolve through here: that provider is github.com whatever the
// deployment default is, so the login claim binds under ghbase.GitHubCom
// literally.
func EffectiveGitHubHost(orgBase string) string {
	if h := NormalizeGitHubHost(orgBase); h != "" {
		return h
	}
	return ghbase.DefaultBaseURL()
}
