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
	base  string // org_settings.github_base_url
	err   error
}

func (o readyOrgs) GetSettings(context.Context, string) (domain.OrgSettings, error) {
	if o.err != nil {
		return domain.OrgSettings{}, o.err
	}
	return domain.OrgSettings{GitHubCredentialClass: o.class, GitHubBaseURL: o.base}, nil
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

// TestGitHubReady_PATAnswersOnlyWhereItWouldBeBorrowed is the rule that keeps
// this gate honest: a stored PAT is not readiness by itself, it is readiness
// only for a class whose resolution would actually reach for it.
//
// The managed rows are the ones that matter. A workspace on the deployment's
// shared App resolves from that App or fails — github.activeApp has no path
// from the managed class to the PAT tier — so a PAT left in its secret store is
// a credential it will never act on. Reporting it as connected would tell a
// founder their setup is finished on the strength of a token nothing will use,
// and would tell the event-source probe that GitHub can produce events for an
// org that cannot poll.
//
// Reading credential presence before the class is the same inference the class
// column exists to remove, and this test is where it stays removed.
func TestGitHubReady_PATAnswersOnlyWhereItWouldBeBorrowed(t *testing.T) {
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
		// The PAT is the credential.
		{"pat class", domain.GitHubCredentialClassPAT, nil, nil, true},
		// The staged window of a PAT→App switch: the class already says App
		// while the PAT is still what resolves. The PAT must keep answering here
		// or a mid-switch org reads as disconnected.
		{"byo app staged behind the pat", domain.GitHubCredentialClassBYOApp, staged, nil, true},
		{"byo app class with no row", domain.GitHubCredentialClassBYOApp, nil, nil, true},
		// The App answers; the PAT is beside the point either way.
		{"byo app live", domain.GitHubCredentialClassBYOApp, live, nil, true},
		// The leak: a credential the resolver would never borrow, and nothing
		// bound that could stand in for it.
		{"managed app with a stray pat and nothing bound", domain.GitHubCredentialClassManagedApp, nil, nil, false},
		// Ready on the bind, not on the PAT sitting beside it.
		{"managed app bound, stray pat irrelevant", domain.GitHubCredentialClassManagedApp, nil, bound, true},
		// Nothing resolves under a class this build cannot name, the PAT
		// included, so neither does this.
		{"unknown class", domain.GitHubCredentialClass("not-a-credential-class"), live, bound, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orgs := readyOrgs{class: tc.class}
			apps := readyApps{app: tc.app, insts: tc.insts}
			creds := auth.Credentials{GitHubPAT: "ghp_present"}

			got, err := GitHubReady(ctx, orgs, apps, "org-1", creds)
			if err != nil {
				t.Fatalf("GitHubReady: %v", err)
			}
			if got != tc.want {
				t.Errorf("GitHubReady = %v with a PAT in the store; want %v", got, tc.want)
			}
			gotSys, err := GitHubReadySystem(ctx, orgs, apps, "org-1", creds)
			if err != nil {
				t.Fatalf("GitHubReadySystem: %v", err)
			}
			if gotSys != got {
				t.Errorf("GitHubReadySystem = %v; GitHubReady = %v; the two doors must answer alike", gotSys, got)
			}
		})
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

// TestGitHubPATHostMatches is the truth table for the host check every PAT read
// makes: the PAT is sent only to the host recorded beside it, both sides
// compared as GitHubHost values, and a PAT with no recorded host is used on
// whatever host the org resolves to.
func TestGitHubPATHostMatches(t *testing.T) {
	for _, tc := range []struct {
		name         string
		bound, base  string
		wantSendable bool
	}{
		{"same host", "https://ghe.example.com", "https://ghe.example.com", true},
		{"trailing slash on the bound url", "https://ghe.example.com/", "https://ghe.example.com", true},
		{"trailing slashes on the base", "https://ghe.example.com", "https://ghe.example.com//", true},
		{"another host", "https://ghe.example.com", "https://github.com", false},
		{"another GHES path mount", "https://ghe.example.com/a", "https://ghe.example.com/b", false},
		// An empty base is the deployment default, github.com in a test process.
		{"empty base is the default host", "https://github.com", "", true},
		{"empty base, PAT bound on GHES", "https://ghe.example.com", "", false},
		// No host recorded: nothing to disagree with.
		{"no bound host", "", "https://ghe.example.com", true},
		{"no bound host, empty base", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := GitHubPATHostMatches(tc.bound, tc.base); got != tc.wantSendable {
				t.Errorf("GitHubPATHostMatches(%q, %q) = %v; want %v", tc.bound, tc.base, got, tc.wantSendable)
			}
		})
	}
}

// TestGitHubPATUsable is the truth table for the readiness half of the same
// rule. An empty org setting resolves to the URL stored beside the PAT, so
// only a setting that names another host makes a bound PAT unusable.
func TestGitHubPATUsable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		creds   auth.Credentials
		orgBase string
		want    bool
	}{
		{"no pat", auth.Credentials{GitHubURL: "https://github.com"}, "https://github.com", false},
		{"no pat, no host", auth.Credentials{}, "", false},
		{"pat on its own host", auth.Credentials{GitHubPAT: "ghp_x", GitHubURL: "https://ghe.example.com"}, "https://ghe.example.com", true},
		{"pat on its own host, slashes differ", auth.Credentials{GitHubPAT: "ghp_x", GitHubURL: "https://ghe.example.com/"}, "https://ghe.example.com", true},
		{"pat bound on another host", auth.Credentials{GitHubPAT: "ghp_x", GitHubURL: "https://ghe.example.com"}, "https://github.com", false},
		{"pat with no bound host", auth.Credentials{GitHubPAT: "ghp_x"}, "https://ghe.example.com", true},
		// No setting: the org resolves to the PAT's own URL, so it matches by
		// construction even for a GHES PAT.
		{"empty org setting", auth.Credentials{GitHubPAT: "ghp_x", GitHubURL: "https://ghe.example.com"}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := GitHubPATUsable(tc.creds, tc.orgBase); got != tc.want {
				t.Errorf("GitHubPATUsable(%+v, %q) = %v; want %v", tc.creds, tc.orgBase, got, tc.want)
			}
		})
	}
}

// TestGitHubReady_PATBoundOnAnotherHostIsNotReady pins the gate on the PAT
// arms: a PAT validated on a host other than the org's current one is a
// credential the resolver refuses to send, so the org reads as not connected
// until it is rebound — on the pat class and in the staged window of a
// PAT-to-App switch alike, through both doors.
func TestGitHubReady_PATBoundOnAnotherHostIsNotReady(t *testing.T) {
	ctx := context.Background()
	staged := &domain.OrgGitHubApp{Active: false, ClientID: "Iv1.byo"}
	creds := auth.Credentials{GitHubPAT: "ghp_present", GitHubURL: "https://ghe.old.example.com"}

	for _, tc := range []struct {
		name  string
		class domain.GitHubCredentialClass
		app   *domain.OrgGitHubApp
		base  string
		want  bool
	}{
		{"pat class, host moved", domain.GitHubCredentialClassPAT, nil, "https://github.com", false},
		{"pat class, same host", domain.GitHubCredentialClassPAT, nil, "https://ghe.old.example.com/", true},
		{"byo app staged behind the pat, host moved", domain.GitHubCredentialClassBYOApp, staged, "https://github.com", false},
		{"byo app staged behind the pat, same host", domain.GitHubCredentialClassBYOApp, staged, "https://ghe.old.example.com", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orgs := readyOrgs{class: tc.class, base: tc.base}
			apps := readyApps{app: tc.app}
			got, err := GitHubReady(ctx, orgs, apps, "org-1", creds)
			if err != nil {
				t.Fatalf("GitHubReady: %v", err)
			}
			if got != tc.want {
				t.Errorf("GitHubReady = %v; want %v", got, tc.want)
			}
			gotSys, err := GitHubReadySystem(ctx, orgs, apps, "org-1", creds)
			if err != nil {
				t.Fatalf("GitHubReadySystem: %v", err)
			}
			if gotSys != got {
				t.Errorf("GitHubReadySystem = %v; GitHubReady = %v; the two doors must answer alike", gotSys, got)
			}
		})
	}
}
