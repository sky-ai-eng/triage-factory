package integrations

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

type readyOrgs struct {
	db.OrgsStore
	class domain.GitHubCredentialClass
	err   error
}

func (o readyOrgs) GetSettings(context.Context, string) (domain.OrgSettings, error) {
	if o.err != nil {
		return domain.OrgSettings{}, o.err
	}
	return domain.OrgSettings{GitHubCredentialClass: o.class}, nil
}

func (o readyOrgs) GetSettingsSystem(ctx context.Context, orgID string) (domain.OrgSettings, error) {
	return o.GetSettings(ctx, orgID)
}

type readyApps struct {
	db.GitHubAppsStore
	app   *domain.OrgGitHubApp
	insts []domain.OrgGitHubAppInstallation
}

func (a readyApps) GetForOrg(context.Context, string) (*domain.OrgGitHubApp, error) {
	return a.app, nil
}

func (a readyApps) GetForOrgSystem(ctx context.Context, orgID string) (*domain.OrgGitHubApp, error) {
	return a.GetForOrg(ctx, orgID)
}

func (a readyApps) ListInstallationsForOrg(context.Context, string) ([]domain.OrgGitHubAppInstallation, error) {
	return a.insts, nil
}

func (a readyApps) ListInstallationsForOrgSystem(ctx context.Context, orgID string) ([]domain.OrgGitHubAppInstallation, error) {
	return a.ListInstallationsForOrg(ctx, orgID)
}

// TestGitHubReady_EveryCredentialClass walks all three classes through the
// derivation that stands behind both the setup gate and the github
// event-source's availability.
//
// The managed arm is the one worth reading twice. The shared App itself is a
// deployment fact — it lives in the operator's environment, which this function
// cannot see — so what it asks instead is the question the workspace can act
// on: has this workspace bound the App to any account? Zero installations is a
// workspace that has connected nothing, which is exactly what the setup step
// exists to prompt; one or more is a workspace TF can reach GitHub for.
func TestGitHubReady_EveryCredentialClass(t *testing.T) {
	ctx := context.Background()
	live := &domain.OrgGitHubApp{Active: true, ClientID: "Iv1.byo"}
	staged := &domain.OrgGitHubApp{Active: false, ClientID: "Iv1.byo"}
	bound := []domain.OrgGitHubAppInstallation{{InstallationID: "456", AccountLogin: "acme"}}

	for _, tc := range []struct {
		name  string
		class domain.GitHubCredentialClass
		app   *domain.OrgGitHubApp
		insts []domain.OrgGitHubAppInstallation
		want  bool
	}{
		{"pat class with no pat", domain.GitHubCredentialClassPAT, nil, nil, false},
		{"byo app, live", domain.GitHubCredentialClassBYOApp, live, nil, true},
		{"byo app, staged", domain.GitHubCredentialClassBYOApp, staged, nil, false},
		{"byo app class with no row", domain.GitHubCredentialClassBYOApp, nil, nil, false},
		// No registration row, and none can exist: what makes the shared App
		// usable FOR THIS WORKSPACE is the bind.
		{"managed app, bound", domain.GitHubCredentialClassManagedApp, nil, bound, true},
		{"managed app, nothing bound", domain.GitHubCredentialClassManagedApp, nil, nil, false},
		// An unrecognised class resolves no App and leaves the PAT signal
		// standing — which, with no PAT, is "not connected".
		{"unknown class", domain.GitHubCredentialClass("not-a-credential-class"), live, bound, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orgs := readyOrgs{class: tc.class}
			apps := readyApps{app: tc.app, insts: tc.insts}

			got, err := GitHubReady(ctx, orgs, apps, "org-1", auth.Credentials{})
			if err != nil {
				t.Fatalf("GitHubReady: %v", err)
			}
			if got != tc.want {
				t.Errorf("GitHubReady = %v; want %v", got, tc.want)
			}

			// The System twin is the same derivation through the claims-free
			// door, and the two drifting apart would mean a background pass and
			// a request disagreeing about whether GitHub is connected.
			gotSys, err := GitHubReadySystem(ctx, orgs, apps, "org-1", auth.Credentials{})
			if err != nil {
				t.Fatalf("GitHubReadySystem: %v", err)
			}
			if gotSys != got {
				t.Errorf("GitHubReadySystem = %v; GitHubReady = %v; the two doors must answer alike", gotSys, got)
			}
		})
	}
}

// TestGitHubReady_PATShortCircuitsEveryClass pins that a stored PAT still
// answers first, whatever the class says. The env overlay is folded into creds
// upstream, so this is also the headless-install path.
func TestGitHubReady_PATShortCircuitsEveryClass(t *testing.T) {
	ctx := context.Background()
	for _, class := range []domain.GitHubCredentialClass{
		domain.GitHubCredentialClassPAT,
		domain.GitHubCredentialClassBYOApp,
		domain.GitHubCredentialClassManagedApp,
	} {
		got, err := GitHubReady(ctx, readyOrgs{class: class}, readyApps{}, "org-1", auth.Credentials{GitHubPAT: "ghp_x"})
		if err != nil || !got {
			t.Errorf("class %q: GitHubReady = (%v, %v); want (true, nil)", class, got, err)
		}
	}
}

// TestGitHubReady_ReadFailureIsAnError pins that a backend fault stays a fault.
// Reporting one as "not connected" is indistinguishable to the caller from the
// real answer, and would send a founder back through setup over a store blip.
func TestGitHubReady_ReadFailureIsAnError(t *testing.T) {
	boom := errors.New("settings store unavailable")
	if _, err := GitHubReady(context.Background(), readyOrgs{err: boom}, readyApps{}, "org-1", auth.Credentials{}); !errors.Is(err, boom) {
		t.Errorf("err = %v; want the settings read error to propagate", err)
	}
}
