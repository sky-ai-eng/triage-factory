package github

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
)

// unknownCredentialClass is a class no build defines: a value written by a
// newer peer, or a hand-edited row. It is deliberately not a real class name —
// "managed_app" stood in here until this build learned to resolve it, and what
// these tests pin is the refusal, not the string.
const unknownCredentialClass = domain.GitHubCredentialClass("not-a-credential-class")

// TestResolver_UnknownCredentialClass_Refuses pins the direction this ticket
// cares about most: a credential class the build doesn't recognise is refused,
// not resolved as PAT.
//
// The org here has a perfectly usable PAT sitting in the secret store, which is
// exactly what makes the assertion meaningful — the old "no App row means PAT"
// inference would have found it and handed it over. A class the resolver cannot
// name is a class whose credential system it cannot name either, and quietly
// picking the one it happens to find is how a workspace ends up acting on a
// credential that was never its own.
//
// Every entry point is covered, because a refusal that holds on ClientFor and
// leaks through TokenForRepoScoped is no refusal at all.
func TestResolver_UnknownCredentialClass_Refuses(t *testing.T) {
	newResolver := func() Resolver {
		return NewResolver(
			&fakeSecrets{vals: map[string]string{integrations.KeyGitHubPAT: "ghp_present_but_not_ours"}},
			&fakeApps{app: nil},
			&fakeOrgs{base: "https://github.com", class: unknownCredentialClass},
			&fakeAgents{},
			nil,
		)
	}
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		call func(r Resolver) error
	}{
		{"ClientFor", func(r Resolver) error {
			_, err := r.ClientFor(ctx, "org-1", "acme")
			return err
		}},
		{"ClientForRepo", func(r Resolver) error {
			_, err := r.ClientForRepo(ctx, "org-1", "acme", "api")
			return err
		}},
		{"TokenFor", func(r Resolver) error {
			_, err := r.TokenFor(ctx, "org-1", "acme")
			return err
		}},
		{"TokenForRepoScoped", func(r Resolver) error {
			_, err := r.(ScopedResolver).TokenForRepoScoped(ctx, "org-1", "acme", "api", nil)
			return err
		}},
		{"TokenForReposScoped", func(r Resolver) error {
			_, err := r.(ScopedResolver).TokenForReposScoped(ctx, "org-1", "acme", []string{"api"}, nil)
			return err
		}},
		{"ClientForRepoScoped", func(r Resolver) error {
			_, _, err := r.(ScopedRepoResolver).ClientForRepoScoped(ctx, "org-1", "acme", "api", nil)
			return err
		}},
		{"HasAnyCredential", func(r Resolver) error {
			_, err := r.(ScopedResolver).HasAnyCredential(ctx, "org-1")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(newResolver())
			if !errors.Is(err, ErrUnknownCredentialClass) {
				t.Fatalf("err = %v; want ErrUnknownCredentialClass — an unrecognised class must refuse, never fall through to the PAT tier", err)
			}
			// Not ErrNoGitHubCredentials: the org may well have a working
			// credential. What's missing is this build's ability to say which
			// system it belongs to, and reporting "GitHub not configured" would
			// send an admin to re-authorize something that isn't broken.
			if errors.Is(err, ErrNoGitHubCredentials) {
				t.Error("unknown class wrapped ErrNoGitHubCredentials; it must stay distinguishable from a genuinely unconfigured org")
			}
		})
	}
}

// TestResolver_UnknownCredentialClass_StampsNoIdentity covers the one entry
// point that cannot return an error. OrgIdentityFor's whole posture is to stamp
// nothing rather than something fabricated, and an org whose credential system
// this build can't name is the clearest case of that — the PAT login would be
// an identity for a credential it may not be acting as.
func TestResolver_UnknownCredentialClass_StampsNoIdentity(t *testing.T) {
	r := NewResolver(
		&fakeSecrets{vals: map[string]string{integrations.KeyGitHubPAT: "ghp_present"}},
		&fakeApps{app: nil},
		&fakeOrgs{base: "https://github.com", class: unknownCredentialClass},
		&fakeAgents{agent: &domain.Agent{GitHubOrgLogin: "acme-bot"}},
		nil,
	)

	name, email, ok := r.OrgIdentityFor(context.Background(), "org-1")
	if ok || name != "" || email != "" {
		t.Errorf("OrgIdentityFor = (%q, %q, %v); want no identity under an unrecognised credential class", name, email, ok)
	}
}

// TestResolver_ClassDecidesTier_NotRowPresence is the positive statement of the
// same rule: the class is what selects the credential system, so a PAT-class
// org does not reach the App tier even with a live registration row in front of
// it.
//
// That combination cannot occur in production — registration writes the class
// in the same transaction as the row — which is the point. Pinning it here is
// what proves the resolver reads the stated class rather than re-deriving it
// from the row it happens to find, and the assertion would flip the moment
// someone "simplified" activeApp back to a row-presence check.
func TestResolver_ClassDecidesTier_NotRowPresence(t *testing.T) {
	gh := newGHTestServer(t)
	r := NewResolver(
		&fakeSecrets{vals: map[string]string{integrations.KeyGitHubPAT: "ghp_test"}},
		&fakeApps{app: activeApp(), insts: []domain.OrgGitHubAppInstallation{installOn("acme")}},
		&fakeOrgs{base: gh.srv.URL, class: domain.GitHubCredentialClassPAT},
		&fakeAgents{},
		nil,
	)

	client, err := r.ClientFor(context.Background(), "org-1", "acme")
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if _, err := client.Get(context.Background(), "/probe"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if gh.lastProbe != "Bearer ghp_test" {
		t.Errorf("client carried %q; want the PAT — the class, not the App row, selects the tier", gh.lastProbe)
	}
	if gh.mintCalls != 0 {
		t.Errorf("mint was called %d times for a pat-class org", gh.mintCalls)
	}
}

// TestResolver_StagedApp_IsBYOAppClassOnPATTier pins the orthogonality the
// class and the Active bit are there to express. A staged App — registered,
// key stored, not yet cut over — is a byo_app org whose LIVE credential is
// still the PAT. The class says which system; Active says which credential.
// Collapsing either into the other breaks the PAT→App switch window.
func TestResolver_StagedApp_IsBYOAppClassOnPATTier(t *testing.T) {
	gh := newGHTestServer(t)
	staged := activeApp()
	staged.Active = false
	r := NewResolver(
		&fakeSecrets{vals: map[string]string{integrations.KeyGitHubPAT: "ghp_live"}},
		&fakeApps{app: staged, insts: []domain.OrgGitHubAppInstallation{installOn("acme")}},
		&fakeOrgs{base: gh.srv.URL, class: domain.GitHubCredentialClassBYOApp},
		&fakeAgents{},
		nil,
	)

	client, err := r.ClientFor(context.Background(), "org-1", "acme")
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if _, err := client.Get(context.Background(), "/probe"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if gh.lastProbe != "Bearer ghp_live" {
		t.Errorf("client carried %q; want the live PAT — a staged App must not mint", gh.lastProbe)
	}
	if gh.mintCalls != 0 {
		t.Errorf("mint was called %d times for a staged App", gh.mintCalls)
	}
}

// TestResolver_ResolvesOrgSettingsOncePerCall pins the cost of storing the
// credential class next to the host.
//
// Both facts live on org_settings, and every entry point that resolves a
// credential needs both — so reading the row once per resolution rather than
// once per fact is what keeps the class from doubling the settings round-trips
// on the polling paths, which resolve per org per cycle.
//
// It is also a correctness property, not only a cost one: two reads could
// straddle a credential transition and pair one state's host with another
// state's class. One read is one snapshot.
func TestResolver_ResolvesOrgSettingsOncePerCall(t *testing.T) {
	gh := newGHTestServer(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		class domain.GitHubCredentialClass
		app   *domain.OrgGitHubApp
		insts []domain.OrgGitHubAppInstallation
		call  func(r Resolver) error
	}{
		{"ClientFor_pat", domain.GitHubCredentialClassPAT, nil, nil, func(r Resolver) error {
			_, err := r.ClientFor(ctx, "org-1", "acme")
			return err
		}},
		{"ClientFor_app", domain.GitHubCredentialClassBYOApp, activeApp(), []domain.OrgGitHubAppInstallation{installOn("acme")}, func(r Resolver) error {
			_, err := r.ClientFor(ctx, "org-1", "acme")
			return err
		}},
		{"ClientForRepo_pat", domain.GitHubCredentialClassPAT, nil, nil, func(r Resolver) error {
			_, err := r.ClientForRepo(ctx, "org-1", "acme", "api")
			return err
		}},
		{"TokenFor_pat", domain.GitHubCredentialClassPAT, nil, nil, func(r Resolver) error {
			_, err := r.TokenFor(ctx, "org-1", "acme")
			return err
		}},
		{"TokenForRepoScoped_pat", domain.GitHubCredentialClassPAT, nil, nil, func(r Resolver) error {
			_, err := r.(ScopedResolver).TokenForRepoScoped(ctx, "org-1", "acme", "api", nil)
			return err
		}},
		{"TokenForReposScoped_pat", domain.GitHubCredentialClassPAT, nil, nil, func(r Resolver) error {
			_, err := r.(ScopedResolver).TokenForReposScoped(ctx, "org-1", "acme", []string{"api"}, nil)
			return err
		}},
		{"ClientForRepoScoped_pat", domain.GitHubCredentialClassPAT, nil, nil, func(r Resolver) error {
			_, _, err := r.(ScopedRepoResolver).ClientForRepoScoped(ctx, "org-1", "acme", "api", nil)
			return err
		}},
		{"HasAnyCredential_pat", domain.GitHubCredentialClassPAT, nil, nil, func(r Resolver) error {
			_, err := r.(ScopedResolver).HasAnyCredential(ctx, "org-1")
			return err
		}},
		{"OrgIdentityFor_pat", domain.GitHubCredentialClassPAT, nil, nil, func(r Resolver) error {
			r.OrgIdentityFor(ctx, "org-1")
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orgs := &fakeOrgs{base: gh.srv.URL, class: tc.class}
			r := NewResolver(
				// Both credentials present, so the App arm can actually mint and
				// the count reflects a completed resolution rather than an early
				// bail-out.
				&fakeSecrets{vals: map[string]string{
					integrations.KeyGitHubPAT: "ghp_test",
					"pem":                     testPEM(t),
				}},
				&fakeApps{app: tc.app, insts: tc.insts},
				orgs,
				&fakeAgents{},
				nil,
			)
			if err := tc.call(r); err != nil {
				t.Fatalf("call: %v", err)
			}
			if orgs.settingsReads != 1 {
				t.Errorf("org_settings read %d times for one resolution; want exactly 1 — the host and the credential class come off the same row",
					orgs.settingsReads)
			}
		})
	}
}

// TestResolver_CredentialClassReadError_Propagates pins that an unreadable
// settings row is an error, not a guess. The org has a PAT, so falling back to
// the old inference would silently succeed — and would be wrong for any org
// that is not on a PAT. Same discipline githubBaseFor already applies to the
// same read for the same reason: a source that decides which credential to use
// must never be assumed.
func TestResolver_CredentialClassReadError_Propagates(t *testing.T) {
	boom := errors.New("settings store unavailable")
	r := NewResolver(
		&fakeSecrets{vals: map[string]string{integrations.KeyGitHubPAT: "ghp_test"}},
		&fakeApps{app: nil},
		&fakeOrgs{base: "https://github.com", settingsErr: boom},
		&fakeAgents{},
		nil,
	)

	if _, err := r.ClientFor(context.Background(), "org-1", "acme"); !errors.Is(err, boom) {
		t.Errorf("ClientFor err = %v; want the settings read error to propagate", err)
	}
	if _, err := r.(ScopedResolver).HasAnyCredential(context.Background(), "org-1"); !errors.Is(err, boom) {
		t.Errorf("HasAnyCredential err = %v; want the settings read error to propagate", err)
	}
}
