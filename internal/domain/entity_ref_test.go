package domain

import "testing"

// TestEntityRefForExternal pins the (provider, target) → entity natural-key
// mapping the exec-touch resolver uses: Jira targets are issue keys and need
// the issue's id, Linear targets are issue identifiers and need the issue's
// UUID, Slack targets are a SlackSourceID, and every GitHub coordinate, every
// other provider and every empty key is skipped (ok=false). A GitHub key's
// shape says neither whether it names a pull request or an issue nor which
// repository its name belongs to now, so a GitHub entity is resolved from
// GitHub's ids and never here.
func TestEntityRefForExternal(t *testing.T) {
	cases := []struct {
		name       string
		provider   string
		target     string
		externalID string
		wantOK     bool
		// wantSourceID is the natural key expected out. Empty means "the
		// target verbatim", which is every provider except Jira — whose keys
		// are folded to canonical form because Jira resolves them
		// case-insensitively but answers with one spelling, and an entity
		// keyed on any other spelling can never match its own issue again.
		wantSourceID string
		wantSource   string
		wantKind     string
	}{
		{
			name:     "github owner/repo#N target skipped",
			provider: ProviderGitHub, target: "octo/repo#18",
		},
		{
			name:     "github target with a node id skipped",
			provider: ProviderGitHub, target: "octo/repo#18", externalID: "PR_kwDO18",
		},
		{
			// A bare owner/repo is repo-level — a branch push's shape too.
			name:     "github repo-level target skipped",
			provider: ProviderGitHub, target: "octo/repo",
		},
		{
			name:     "github empty target skipped",
			provider: ProviderGitHub, target: "",
		},
		{
			name:     "jira issue key → issue entity",
			provider: ProviderJira, target: "SKY-123", externalID: "10042",
			wantOK: true, wantSource: ProviderJira, wantKind: "issue",
		},
		{
			// A key alone can name another issue once the one it named moved,
			// so nothing is resolved — or minted — without the id.
			name:     "jira key without an issue id skipped",
			provider: ProviderJira, target: "SKY-123",
		},
		{
			name:     "jira empty target skipped",
			provider: ProviderJira, target: "", externalID: "10042",
		},
		{
			// The defect this folding closes: Jira accepts a lower-case key on
			// every surface and answers with the upper-case one, so an entity
			// minted verbatim here is one the poller can never match a refresh
			// back to — silently, since nothing about the call fails.
			name:     "jira lower-case key folded to canonical",
			provider: ProviderJira, target: "sky-123", externalID: "10042",
			wantOK: true, wantSourceID: "SKY-123",
			wantSource: ProviderJira, wantKind: "issue",
		},
		{
			name:     "jira mixed-case key with padding folded",
			provider: ProviderJira, target: "  Sky-123 ", externalID: "10042",
			wantOK: true, wantSourceID: "SKY-123",
			wantSource: ProviderJira, wantKind: "issue",
		},
		{
			// Skipped on the folded value, not the raw one — otherwise a
			// whitespace-only target mints an entity keyed on "".
			name:     "jira whitespace-only target skipped",
			provider: ProviderJira, target: "   ", externalID: "10042",
		},
		{
			name:     "slack source id → message entity",
			provider: ProviderSlack, target: SlackSourceID("C0125", "1700000000.000100"),
			wantOK: true, wantSource: ProviderSlack, wantKind: "message",
		},
		{
			name:     "slack empty target skipped",
			provider: ProviderSlack, target: "",
		},
		{
			name:     "linear issue identifier → issue entity",
			provider: ProviderLinear, target: "ENG-42", externalID: "2a7e6b0c-uuid",
			wantOK: true, wantSource: ProviderLinear, wantKind: "issue",
		},
		{
			// An identifier alone can name another issue once the one it named
			// moved team, so nothing is resolved — or minted — without the UUID.
			name:     "linear identifier without an issue uuid skipped",
			provider: ProviderLinear, target: "ENG-42",
		},
		{
			name:     "linear empty target skipped",
			provider: ProviderLinear, target: "  ", externalID: "2a7e6b0c-uuid",
		},
		{
			// PR-shaped target under an unmapped provider — proves the skip is
			// on the provider (default arm), not the target shape.
			name:     "unmapped provider skipped",
			provider: ProviderGit, target: "octo/repo#9",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, kind, ok := EntityRefForExternal(tc.provider, tc.target, tc.externalID)
			source, sourceID := ref.Source, ref.SourceID
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				if ref != (EntityRef{}) || kind != "" {
					t.Errorf("skip should return a zero ref, got (%+v,%q)", ref, kind)
				}
				return
			}
			wantExternalID := tc.externalID
			if tc.provider == ProviderSlack {
				wantExternalID = ""
			}
			if ref.ExternalID != wantExternalID {
				t.Errorf("externalID = %q, want %q", ref.ExternalID, wantExternalID)
			}
			if source != tc.wantSource {
				t.Errorf("source = %q, want %q", source, tc.wantSource)
			}
			wantSourceID := tc.wantSourceID
			if wantSourceID == "" {
				wantSourceID = tc.target
			}
			if sourceID != wantSourceID {
				t.Errorf("sourceID = %q, want %q", sourceID, wantSourceID)
			}
			if kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", kind, tc.wantKind)
			}
		})
	}
}
