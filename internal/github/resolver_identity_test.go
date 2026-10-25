package github

import (
	"context"
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

// identityOnlyResolver answers OrgIdentityFor and nothing else, the shape of a
// Resolver with no account-id extension.
type identityOnlyResolver struct {
	Resolver
	name, email string
}

func (r identityOnlyResolver) OrgIdentityFor(context.Context, string) (string, string, bool) {
	return r.name, r.email, r.name != ""
}

// identityAccountResolver adds the account-id extension to identityOnlyResolver.
type identityAccountResolver struct {
	identityOnlyResolver
	accountID string
}

func (r identityAccountResolver) OrgIdentityAccountFor(context.Context, string) (OrgIdentity, bool) {
	return OrgIdentity{Name: r.name, Email: r.email, AccountID: r.accountID}, r.name != ""
}

// TestOrgIdentityAccount pins the helper's two arms: the account id comes
// through the extension when the resolver has it, and a resolver without it
// still yields the name and email, with no account id.
func TestOrgIdentityAccount(t *testing.T) {
	ctx := context.Background()
	base := identityOnlyResolver{name: "acme[bot]", email: "316798692+acme[bot]@users.noreply.github.com"}

	got, ok := OrgIdentityAccount(ctx, identityAccountResolver{base, "316798692"}, "org-1")
	if want := (OrgIdentity{Name: base.name, Email: base.email, AccountID: "316798692"}); !ok || got != want {
		t.Errorf("with the extension = (%+v, %v), want (%+v, true)", got, ok, want)
	}

	got, ok = OrgIdentityAccount(ctx, base, "org-1")
	if want := (OrgIdentity{Name: base.name, Email: base.email}); !ok || got != want {
		t.Errorf("without the extension = (%+v, %v), want (%+v, true)", got, ok, want)
	}

	if _, ok := OrgIdentityAccount(ctx, identityOnlyResolver{}, "org-1"); ok {
		t.Error("an org with no identity resolved one")
	}
}
