package db

import "testing"

// TestNormalizeJiraHost locks the trailing-slash + whitespace trim that keys
// every user_jira_identities row. The whitespace half is the difference from
// NormalizeGitHubHost (below) and the reason the /api/me host-preference SQL
// wraps the org host in trim() — see the doc comment on NormalizeJiraHost.
func TestNormalizeJiraHost(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://jira.example.com", "https://jira.example.com"},
		{"https://jira.example.com/", "https://jira.example.com"},
		{"https://jira.example.com///", "https://jira.example.com"},
		{"  https://jira.example.com  ", "https://jira.example.com"},
		{" https://jira.example.com/ ", "https://jira.example.com"},
		{"https://site.atlassian.net/", "https://site.atlassian.net"},
		{"", ""},
		{"   ", ""},
		{"/", ""},
	}
	for _, c := range cases {
		if got := NormalizeJiraHost(c.in); got != c.want {
			t.Errorf("NormalizeJiraHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestNormalizeGitHubHost pins the GitHub counterpart's behavior and, by
// contrast, documents the deliberate divergence: it trims a trailing slash but
// NOT surrounding whitespace. That's correct and self-consistent — the
// /api/me GitHub host-preference subquery uses a bare rtrim(..., '/') with no
// trim(), so the Go normalizer and the SQL agree by both leaving whitespace
// alone. (Jira needs trim() on both sides precisely because this one doesn't.)
func TestNormalizeGitHubHost(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://github.com", "https://github.com"},
		{"https://github.com/", "https://github.com"},
		{"https://github.com///", "https://github.com"},
		{"https://ghe.corp.example.com/", "https://ghe.corp.example.com"},
		{" https://github.com", " https://github.com"}, // whitespace deliberately preserved
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizeGitHubHost(c.in); got != c.want {
			t.Errorf("NormalizeGitHubHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
