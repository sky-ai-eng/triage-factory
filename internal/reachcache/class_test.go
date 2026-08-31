package reachcache

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The class an org's entries are keyed under is the one thing writer and reader
// must agree on: key a read differently from the write and the mirror reads as
// empty for an org that has one. What makes that non-obvious is the App-XOR-PAT
// narrowing — an org in the BYO-App system whose App is not active reaches
// repositories through its PAT, and its entries are PAT entries.

type fakeOrgs struct {
	db.OrgsStore
	settings domain.OrgSettings
	err      error
}

func (f fakeOrgs) GetSettingsSystem(context.Context, string) (domain.OrgSettings, error) {
	return f.settings, f.err
}

type fakeApps struct {
	db.GitHubAppsStore
	app *domain.OrgGitHubApp
	err error
}

func (f fakeApps) GetForOrgSystem(context.Context, string) (*domain.OrgGitHubApp, error) {
	return f.app, f.err
}

func TestClassResolver(t *testing.T) {
	ctx := context.Background()
	active := &domain.OrgGitHubApp{Active: true}
	staged := &domain.OrgGitHubApp{Active: false}

	cases := []struct {
		name  string
		class domain.GitHubCredentialClass
		app   *domain.OrgGitHubApp
		want  domain.GitHubCredentialClass
	}{
		{"pat org", domain.GitHubCredentialClassPAT, nil, domain.GitHubCredentialClassPAT},
		{"app org with an active app", domain.GitHubCredentialClassBYOApp, active, domain.GitHubCredentialClassBYOApp},
		// Mid-cutover: the registration exists but its tier-1 mint is off, so the
		// org borrows a PAT. Keying these 'byo_app' would put PAT-reached
		// repositories where the drift queries read a grant — and the table's own
		// CHECK would refuse them outright, since a byo_app entry must name the
		// installation it came through.
		{"app org whose app is not active yet", domain.GitHubCredentialClassBYOApp, staged, domain.GitHubCredentialClassPAT},
		{"app class with no registration row", domain.GitHubCredentialClassBYOApp, nil, domain.GitHubCredentialClassPAT},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewClassResolver(
				fakeOrgs{settings: domain.OrgSettings{GitHubCredentialClass: tc.class}},
				fakeApps{app: tc.app},
			)
			got, err := r.For(ctx, "org-1")
			if err != nil {
				t.Fatalf("For: %v", err)
			}
			if got != tc.want {
				t.Errorf("class = %q; want %q", got, tc.want)
			}
		})
	}

	t.Run("the managed class has nowhere to be keyed, and is refused", func(t *testing.T) {
		// A workspace on the deployment's shared App reaches repositories
		// through installations, but reachable_repositories.credential_class
		// admits 'pat' and 'byo_app' only — each naming the scope column its
		// rows carry. So there is no key for these entries, which is a different
		// thing from there being none yet, and either of the two values on offer
		// would file a managed workspace's reach under a credential system that
		// did not observe it.
		r := NewClassResolver(
			fakeOrgs{settings: domain.OrgSettings{GitHubCredentialClass: domain.GitHubCredentialClassManagedApp}},
			fakeApps{},
		)
		got, err := r.For(ctx, "org-1")
		if !errors.Is(err, ErrClassNotMirrored) {
			t.Errorf("err = %v; want ErrClassNotMirrored", err)
		}
		if errors.Is(err, ErrUnknownCredentialClass) {
			t.Error("the managed class came back as unknown; it is a class this build resolves, and reading the refusal as \"unrecognised\" sends a reader hunting for a spelling mistake")
		}
		if got != "" {
			t.Errorf("class = %q alongside the refusal; a refused resolution keys nothing", got)
		}
	})

	t.Run("unknown class is refused, not defaulted", func(t *testing.T) {
		// Taking the PAT arm for a credential system this build does not
		// recognise would enumerate against a credential the org does not have.
		r := NewClassResolver(
			fakeOrgs{settings: domain.OrgSettings{GitHubCredentialClass: "shared_app"}},
			fakeApps{},
		)
		if _, err := r.For(ctx, "org-1"); !errors.Is(err, ErrUnknownCredentialClass) {
			t.Errorf("err = %v; want ErrUnknownCredentialClass", err)
		}
	})

	t.Run("a failed registration read propagates", func(t *testing.T) {
		// "We could not read the registration" and "there is no active App" must
		// not be the same answer: the second silently re-keys every entry.
		r := NewClassResolver(
			fakeOrgs{settings: domain.OrgSettings{GitHubCredentialClass: domain.GitHubCredentialClassBYOApp}},
			fakeApps{err: errors.New("transient store failure")},
		)
		if _, err := r.For(ctx, "org-1"); err == nil {
			t.Error("a failed registration read resolved a class; it must propagate")
		}
	})

	t.Run("no apps store is a pat deployment", func(t *testing.T) {
		r := NewClassResolver(
			fakeOrgs{settings: domain.OrgSettings{GitHubCredentialClass: domain.GitHubCredentialClassBYOApp}},
			nil,
		)
		got, err := r.For(ctx, "org-1")
		if err != nil {
			t.Fatalf("For: %v", err)
		}
		if got != domain.GitHubCredentialClassPAT {
			t.Errorf("class = %q with no App-registration store wired; want %q", got, domain.GitHubCredentialClassPAT)
		}
	})
}
