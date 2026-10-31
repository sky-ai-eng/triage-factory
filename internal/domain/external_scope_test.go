package domain

import "testing"

// TestExternalObjectScope pins each provider's scope to the scope an entity of
// that provider is keyed under, so a record about an entity and the entity
// name one namespace.
func TestExternalObjectScope(t *testing.T) {
	s := OrgSettings{
		GitHubBaseURL:     "https://ghe.example.com/",
		JiraBaseURL:       "https://acme.atlassian.net",
		LinearWorkspaceID: "ws-1",
	}
	for _, tc := range []struct {
		provider, want string
	}{
		{ProviderGitHub, EntityScope("github", s)},
		{ProviderGit, EntityScope("github", s)},
		{ProviderJira, EntityScope("jira", s)},
		{ProviderLinear, "ws-1"},
		{ProviderSlack, SlackScope},
		{ProviderNetwork, NetworkScope},
		{"gitlab", ""},
	} {
		if got := ExternalObjectScope(tc.provider, s); got != tc.want {
			t.Errorf("ExternalObjectScope(%q) = %q, want %q", tc.provider, got, tc.want)
		}
	}
	if got := ExternalObjectScope(ProviderGit, s); got != "https://ghe.example.com" {
		t.Errorf("a branch's scope = %q, want the org's GitHub host", got)
	}
	unconfigured := OrgSettings{}
	for _, p := range []string{ProviderJira, ProviderLinear} {
		if got := ExternalObjectScope(p, unconfigured); got != "" {
			t.Errorf("ExternalObjectScope(%q) with the source unconfigured = %q, want \"\"", p, got)
		}
	}
}

// TestDeliverableActionCarriesTheDeliverableScope pins that an audit row built from
// a deliverable is recorded in the deliverable's scope, and that the link it falls
// back to for a linkless review is on that same host.
func TestDeliverableActionCarriesTheDeliverableScope(t *testing.T) {
	art := NewReviewDeliverable("octo/api", 7, "PR_kwDOnode", "abc", "conv-1")
	art.Scope = "https://ghe.example.com"
	act := DeliverableAction(&art, PerformedByPerson, "user-1", ActionReviewSubmitted, DeliverableStateReviewPending, DeliverableStateReviewSubmitted, CredentialGitHubApp)
	if act.Scope != art.Scope {
		t.Errorf("action scope = %q, want the deliverable's %q", act.Scope, art.Scope)
	}
	if want := "https://ghe.example.com/octo/api/pull/7"; act.URL != want {
		t.Errorf("action url = %q, want %q", act.URL, want)
	}
}
