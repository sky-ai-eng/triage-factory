package domain

import (
	"net/url"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
)

// EntityScope is the scope an org's entities of source are keyed under now,
// computed from its settings. It is the one place a scope is derived: every
// writer and every key lookup passes what this returns, so two callers cannot
// key the same object under different spellings of its namespace.
//
//   - github: the org's GitHub host (GitHubHost) — the host the poller and
//     routing already key on.
//   - jira: the org's Jira site (JiraHost) — the host per-user Jira
//     credentials are keyed under. "" while no valid Jira base URL is set.
//   - linear: the org's Linear workspace id. "" while no Linear credential is
//     bound.
//
// Slack is not here: an org can connect more than one Slack workspace, so a
// Slack entity's scope is the workspace of the connection it came through, and
// the Slack caller passes that directly. Any source not listed answers "".
//
// When this changes for an org — its Linear credential rebound to another
// workspace, its Jira base URL pointed at another site — rows keyed under the
// old scope stay where they are, and nothing keyed under the new one collides
// with them.
func EntityScope(source string, s OrgSettings) string {
	switch source {
	case "github":
		return GitHubHost(s.GitHubBaseURL)
	case "jira":
		host, _ := JiraHost(s.JiraBaseURL)
		return host
	case "linear":
		return s.LinearWorkspaceID
	}
	return ""
}

// GitHubHost resolves an org's configured github_base_url to the host GitHub
// objects and identities are keyed under: trailing slashes trimmed, and an
// empty setting is the deployment's default GitHub (ghbase.DefaultBaseURL —
// github.com unless the operator named another). Case is kept: GHES
// path-based hosts are case-sensitive below the authority.
func GitHubHost(orgBase string) string {
	if h := strings.TrimRight(orgBase, "/"); h != "" {
		return h
	}
	return ghbase.DefaultBaseURL()
}

// JiraHost canonicalizes an org's configured Jira base URL into the site a
// Jira object or credential is keyed under: surrounding whitespace and
// trailing slashes trimmed, and the result must be a real http(s) origin.
// ok=false, with "", for an empty ("Jira not configured") or malformed base
// URL. A context path is part of the site and is kept.
func JiraHost(orgBase string) (string, bool) {
	host := strings.TrimRight(strings.TrimSpace(orgBase), "/")
	if host == "" {
		return "", false
	}
	u, err := url.Parse(host)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	return host, true
}
