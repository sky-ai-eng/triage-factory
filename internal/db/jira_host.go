package db

import "strings"

// NormalizeJiraHost trims surrounding whitespace and a trailing slash so the
// (user_id, jira_base_url) key in user_jira_identities matches regardless of
// the exact form a caller passes. Reads and writes both normalize, so they
// agree by construction.
//
// This is deliberately the trailing-slash/whitespace half of
// jira.CanonicalHost — and ONLY that half. The store can't call
// jira.CanonicalHost directly (internal/jira imports internal/db, so the
// reverse would cycle), so the normalization the store keys on lives here.
// For any well-formed base URL the two agree by construction:
// jira.CanonicalHost returns strings.TrimRight(strings.TrimSpace(orgBase),
// "/") on its success path, identical to this. The extra scheme/host
// validation jira.CanonicalHost layers on top only decides whether Jira is
// configured at all (the capture/status surfaces gate on its ok bool); it
// doesn't change the stored key. So a credential keyed under
// "jira_token/<jira.CanonicalHost(orgBase)>" and an identity row keyed under
// NormalizeJiraHost(orgBase) land on the same host string, keeping access and
// identity in lockstep. Case is preserved — Server/DC path-based
// hosts are case-sensitive below the authority, matching NormalizeGitHubHost.
func NormalizeJiraHost(host string) string {
	return strings.TrimRight(strings.TrimSpace(host), "/")
}
