package db

import "github.com/sky-ai-eng/triage-factory/internal/github/ghbase"

// NormalizeJiraHost puts a Jira site into the ghbase.CanonicalBaseURL form so
// the (user_id, jira_base_url) key in user_jira_identities matches whatever
// spelling a caller passes. Reads and writes both normalize, so they agree by
// construction.
//
// jira.CanonicalHost (through domain.JiraHost) returns the same form on its
// success path, so a credential keyed under
// "jira_token/<jira.CanonicalHost(orgBase)>" and an identity row keyed under
// NormalizeJiraHost(orgBase) land on the same site string. The store cannot
// call jira.CanonicalHost (internal/jira imports internal/db), and does not
// need its extra check: whether the value is a real http(s) origin decides
// only whether Jira is configured at all, never the key.
func NormalizeJiraHost(host string) string {
	return ghbase.CanonicalBaseURL(host)
}
