package db

import "testing"

// TestNormalizeJiraHost locks the canonical form that keys every
// user_jira_identities row: whitespace and trailing slashes trimmed, scheme and
// authority lowercased, the context path's case kept.
func TestNormalizeJiraHost(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://jira.example.com", "https://jira.example.com"},
		{"https://jira.example.com/", "https://jira.example.com"},
		{"https://jira.example.com///", "https://jira.example.com"},
		{"  https://jira.example.com  ", "https://jira.example.com"},
		{" https://jira.example.com/ ", "https://jira.example.com"},
		{"https://site.atlassian.net/", "https://site.atlassian.net"},
		{"HTTPS://Jira.Example.COM", "https://jira.example.com"},
		{"https://Jira.Example.com:8443/Jira/Path/", "https://jira.example.com:8443/Jira/Path"},
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

// TestNormalizeGitHubHost pins the GitHub counterpart, which is the same
// canonical form: an empty host stays empty (EffectiveGitHubHost is the
// variant that resolves it to the deployment default).
func TestNormalizeGitHubHost(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://github.com", "https://github.com"},
		{"https://github.com/", "https://github.com"},
		{"https://github.com///", "https://github.com"},
		{"https://ghe.corp.example.com/", "https://ghe.corp.example.com"},
		{" https://github.com", "https://github.com"},
		{"https://GitHub.com", "https://github.com"},
		{"https://GHE.corp.example.com:8443/GitHub/", "https://ghe.corp.example.com:8443/GitHub"},
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizeGitHubHost(c.in); got != c.want {
			t.Errorf("NormalizeGitHubHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
