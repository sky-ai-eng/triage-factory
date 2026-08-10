package github

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestIdentityCredential pins the resolver's half of the same mapping the
// bundle owns on an executor: the tier that actually resolved decides what the
// audit row says, and an unclassified one keeps the App rather than asserting a
// PAT nobody observed.
func TestIdentityCredential(t *testing.T) {
	for _, tc := range []struct {
		identity Identity
		want     string
	}{
		{IdentityApp, domain.CredentialGitHubApp},
		{IdentityPAT, domain.CredentialGitHubPAT},
		{IdentityUnknown, domain.CredentialGitHubApp},
	} {
		if got := tc.identity.Credential(); got != tc.want {
			t.Errorf("Identity(%d).Credential() = %q, want %q", tc.identity, got, tc.want)
		}
	}
}
