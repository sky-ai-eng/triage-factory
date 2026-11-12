package github

import (
	"context"
	"errors"
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
	err         error
}

func (r identityOnlyResolver) OrgIdentityFor(context.Context, string) (string, string, bool, error) {
	if r.err != nil {
		return "", "", false, r.err
	}
	return r.name, r.email, r.name != "", nil
}

// identityAccountResolver adds the account-id extension to identityOnlyResolver.
type identityAccountResolver struct {
	identityOnlyResolver
	accountID string
}

func (r identityAccountResolver) OrgIdentityAccountFor(context.Context, string) (OrgIdentity, bool, error) {
	if r.err != nil {
		return OrgIdentity{}, false, r.err
	}
	return OrgIdentity{Name: r.name, Email: r.email, AccountID: r.accountID}, r.name != "", nil
}

// TestOrgIdentityAccount pins the helper's two arms: the account id comes
// through the extension when the resolver has it, and a resolver without it
// still yields the name and email, with no account id. A failed read comes
// through either arm as the error.
func TestOrgIdentityAccount(t *testing.T) {
	ctx := context.Background()
	base := identityOnlyResolver{name: "acme[bot]", email: "316798692+acme[bot]@users.noreply.github.com"}

	got, ok, err := OrgIdentityAccount(ctx, identityAccountResolver{base, "316798692"}, "org-1")
	if want := (OrgIdentity{Name: base.name, Email: base.email, AccountID: "316798692"}); err != nil || !ok || got != want {
		t.Errorf("with the extension = (%+v, %v, %v), want (%+v, true, nil)", got, ok, err, want)
	}

	got, ok, err = OrgIdentityAccount(ctx, base, "org-1")
	if want := (OrgIdentity{Name: base.name, Email: base.email}); err != nil || !ok || got != want {
		t.Errorf("without the extension = (%+v, %v, %v), want (%+v, true, nil)", got, ok, err, want)
	}

	if _, ok, err := OrgIdentityAccount(ctx, identityOnlyResolver{}, "org-1"); ok || err != nil {
		t.Errorf("an org with no identity = (%v, %v), want (false, nil)", ok, err)
	}

	readErr := errors.New("store unavailable")
	failing := identityOnlyResolver{name: base.name, email: base.email, err: readErr}
	for name, r := range map[string]Resolver{
		"with the extension":    identityAccountResolver{failing, "316798692"},
		"without the extension": failing,
	} {
		if got, ok, err := OrgIdentityAccount(ctx, r, "org-1"); !errors.Is(err, readErr) || ok || got != (OrgIdentity{}) {
			t.Errorf("%s, a failed read = (%+v, %v, %v), want the error and no identity", name, got, ok, err)
		}
	}
}
