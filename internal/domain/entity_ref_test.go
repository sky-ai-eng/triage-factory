package domain

import "testing"

// TestEntityRefForExternal pins the (provider, target) → entity natural-key
// mapping shared by the exec-touch resolver and the run-end produced-artifact
// attach: GitHub targets must parse as owner/repo#N (repo-level coordinates
// map to nothing), Jira targets are issue keys, Slack targets are a
// SlackSourceID, and every other provider or empty key is skipped (ok=false).
func TestEntityRefForExternal(t *testing.T) {
	cases := []struct {
		name       string
		provider   string
		target     string
		wantOK     bool
		wantSource string
		wantKind   string
	}{
		{
			name:     "github PR target → pr entity",
			provider: ArtifactProviderGitHub, target: "octo/repo#18",
			wantOK: true, wantSource: ArtifactProviderGitHub, wantKind: "pr",
		},
		{
			// A bare owner/repo is repo-level — a branch push's shape too.
			name:     "github repo-level target skipped",
			provider: ArtifactProviderGitHub, target: "octo/repo",
		},
		{
			name:     "github empty target skipped",
			provider: ArtifactProviderGitHub, target: "",
		},
		{
			name:     "jira issue key → issue entity",
			provider: ArtifactProviderJira, target: "SKY-123",
			wantOK: true, wantSource: ArtifactProviderJira, wantKind: "issue",
		},
		{
			name:     "jira empty target skipped",
			provider: ArtifactProviderJira, target: "",
		},
		{
			name:     "slack source id → message entity",
			provider: ArtifactProviderSlack, target: SlackSourceID("C0125", "1700000000.000100"),
			wantOK: true, wantSource: ArtifactProviderSlack, wantKind: "message",
		},
		{
			name:     "slack empty target skipped",
			provider: ArtifactProviderSlack, target: "",
		},
		{
			// PR-shaped target under an unmapped provider — proves the skip is
			// on the provider (default arm), not the target shape.
			name:     "unmapped provider skipped",
			provider: ArtifactProviderLinear, target: "octo/repo#9",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source, sourceID, kind, ok := EntityRefForExternal(tc.provider, tc.target)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				if source != "" || sourceID != "" || kind != "" {
					t.Errorf("skip should return empty strings, got (%q,%q,%q)", source, sourceID, kind)
				}
				return
			}
			if source != tc.wantSource {
				t.Errorf("source = %q, want %q", source, tc.wantSource)
			}
			if sourceID != tc.target {
				t.Errorf("sourceID = %q, want %q (the target verbatim)", sourceID, tc.target)
			}
			if kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", kind, tc.wantKind)
			}
		})
	}
}
