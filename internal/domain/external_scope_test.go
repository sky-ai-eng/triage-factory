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
		{ArtifactProviderGitHub, EntityScope("github", s)},
		{ArtifactProviderGit, EntityScope("github", s)},
		{ArtifactProviderJira, EntityScope("jira", s)},
		{ArtifactProviderLinear, "ws-1"},
		{ArtifactProviderSlack, SlackScope},
		{ArtifactProviderNetwork, NetworkScope},
		{"gitlab", ""},
	} {
		if got := ExternalObjectScope(tc.provider, s); got != tc.want {
			t.Errorf("ExternalObjectScope(%q) = %q, want %q", tc.provider, got, tc.want)
		}
	}
	if got := ExternalObjectScope(ArtifactProviderGit, s); got != "https://ghe.example.com" {
		t.Errorf("a branch's scope = %q, want the org's GitHub host", got)
	}
	unconfigured := OrgSettings{}
	for _, p := range []string{ArtifactProviderJira, ArtifactProviderLinear} {
		if got := ExternalObjectScope(p, unconfigured); got != "" {
			t.Errorf("ExternalObjectScope(%q) with the source unconfigured = %q, want \"\"", p, got)
		}
	}
}

// TestArtifactActionCarriesTheArtifactScope pins that an audit row built from
// an artifact is recorded in the artifact's scope, and that the link it falls
// back to for a linkless review is on that same host.
func TestArtifactActionCarriesTheArtifactScope(t *testing.T) {
	art := NewReviewArtifact("octo/api", 7, "abc", "conv-1")
	art.Scope = "https://ghe.example.com"
	act := ArtifactAction(&art, "user-1", ActionReviewSubmitted, ArtifactStateReviewPending, ArtifactStateReviewSubmitted, CredentialGitHubApp)
	if act.Scope != art.Scope {
		t.Errorf("action scope = %q, want the artifact's %q", act.Scope, art.Scope)
	}
	if want := "https://ghe.example.com/octo/api/pull/7"; act.URL != want {
		t.Errorf("action url = %q, want %q", act.URL, want)
	}
}
