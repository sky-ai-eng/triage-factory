package linear

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// fakeLinearApps is a minimal db.LinearAppsStore for the OAuth-app resolver tests.
// Only GetForOrgSystem is exercised; the rest panic via the embedded interface
// if the resolver unexpectedly reaches for them.
type fakeLinearApps struct {
	db.LinearAppsStore
	app *domain.OrgLinearApp
	err error
}

func (f *fakeLinearApps) GetForOrgSystem(_ context.Context, _ string) (*domain.OrgLinearApp, error) {
	return f.app, f.err
}

// TestResolveOAuthApp_PerOrgOverride: a per-org row + its secret resolve to the
// override credential — tier 1 wins over the deployment app.
func TestResolveOAuthApp_PerOrgOverride(t *testing.T) {
	apps := &fakeLinearApps{app: &domain.OrgLinearApp{ClientID: "org-client", ClientSecretRef: linearSecretRef}}
	secrets := &fakeSecrets{sys: map[string]string{linearSecretRef: "org-secret"}}
	r := NewOAuthAppResolver(apps, secrets, OAuthApp{ClientID: "dep-client", ClientSecret: "dep-secret"})

	got, source, err := r.Resolve(context.Background(), testOrgID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ClientID != "org-client" || got.ClientSecret != "org-secret" {
		t.Fatalf("Resolve = %+v, want org override", got)
	}
	if source != SourceOrgOverride {
		t.Fatalf("source = %v, want SourceOrgOverride", source)
	}
}

// TestResolveOAuthApp_DeploymentFallback: no per-org row → the deployment
// app resolves.
func TestResolveOAuthApp_DeploymentFallback(t *testing.T) {
	r := NewOAuthAppResolver(&fakeLinearApps{}, &fakeSecrets{}, OAuthApp{ClientID: "dep-client", ClientSecret: "dep-secret"})

	got, source, err := r.Resolve(context.Background(), testOrgID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ClientID != "dep-client" || got.ClientSecret != "dep-secret" {
		t.Fatalf("Resolve = %+v, want deployment app", got)
	}
	if source != SourceDeployment {
		t.Fatalf("source = %v, want SourceDeployment", source)
	}
}

// TestResolveOAuthApp_NotConfigured: no per-org row and no deployment app
// → the typed not-configured error.
func TestResolveOAuthApp_NotConfigured(t *testing.T) {
	r := NewOAuthAppResolver(&fakeLinearApps{}, &fakeSecrets{}, OAuthApp{})

	_, source, err := r.Resolve(context.Background(), testOrgID)
	if !errors.Is(err, ErrNoLinearOAuthApp) {
		t.Fatalf("Resolve err = %v, want ErrNoLinearOAuthApp", err)
	}
	if source != SourceNone {
		t.Fatalf("source = %v, want SourceNone", source)
	}
}

// TestResolveOAuthApp_OverrideMissingSecretFallsThrough: a row exists but its
// secret read clean-and-empty → the override is unusable, so the resolver falls
// through to the deployment app rather than returning a half-credential.
func TestResolveOAuthApp_OverrideMissingSecretFallsThrough(t *testing.T) {
	apps := &fakeLinearApps{app: &domain.OrgLinearApp{ClientID: "org-client", ClientSecretRef: linearSecretRef}}
	r := NewOAuthAppResolver(apps, &fakeSecrets{}, OAuthApp{ClientID: "dep-client", ClientSecret: "dep-secret"})

	got, source, err := r.Resolve(context.Background(), testOrgID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ClientID != "dep-client" {
		t.Fatalf("Resolve = %+v, want deployment fallback", got)
	}
	// The dead override must NOT be reported as the live source — this is the
	// state that would otherwise make the settings card claim "configured".
	if source != SourceDeployment {
		t.Fatalf("source = %v, want SourceDeployment (dead override falls through)", source)
	}
}

// TestResolveOAuthApp_OverrideMissingSecretNoDeployment: a dead override with
// no deployment app to fall through to resolves nothing.
func TestResolveOAuthApp_OverrideMissingSecretNoDeployment(t *testing.T) {
	apps := &fakeLinearApps{app: &domain.OrgLinearApp{ClientID: "org-client", ClientSecretRef: linearSecretRef}}
	r := NewOAuthAppResolver(apps, &fakeSecrets{}, OAuthApp{})

	_, source, err := r.Resolve(context.Background(), testOrgID)
	if !errors.Is(err, ErrNoLinearOAuthApp) {
		t.Fatalf("Resolve err = %v, want ErrNoLinearOAuthApp", err)
	}
	if source != SourceNone {
		t.Fatalf("source = %v, want SourceNone", source)
	}
}

// TestResolveOAuthApp_StoreErrorPropagates: a backend read error on the
// per-org lookup propagates rather than being misreported as not-configured —
// a transient outage must not silently fall through to the deployment app.
func TestResolveOAuthApp_StoreErrorPropagates(t *testing.T) {
	boom := errors.New("db down")
	r := NewOAuthAppResolver(&fakeLinearApps{err: boom}, &fakeSecrets{}, OAuthApp{ClientID: "dep", ClientSecret: "dep"})

	_, _, err := r.Resolve(context.Background(), testOrgID)
	if !errors.Is(err, boom) {
		t.Fatalf("Resolve err = %v, want wrapped db error", err)
	}
	if errors.Is(err, ErrNoLinearOAuthApp) {
		t.Fatalf("Resolve err = %v, must not be ErrNoLinearOAuthApp", err)
	}
}

// TestResolveOAuthApp_SecretErrorPropagates: a backend error reading the
// override's client_secret propagates too.
func TestResolveOAuthApp_SecretErrorPropagates(t *testing.T) {
	boom := errors.New("vault down")
	apps := &fakeLinearApps{app: &domain.OrgLinearApp{ClientID: "org-client", ClientSecretRef: linearSecretRef}}
	r := NewOAuthAppResolver(apps, &fakeSecrets{sysErr: boom}, OAuthApp{})

	_, _, err := r.Resolve(context.Background(), testOrgID)
	if !errors.Is(err, boom) {
		t.Fatalf("Resolve err = %v, want wrapped vault error", err)
	}
}

const linearSecretRef = "linear_oauth_client_secret"

// setDeploymentEnv sets the deployment app's pair through t.Setenv, so the
// environment is restored when the test ends. An empty value leaves that var
// unset in effect, which is how the half-configured cases are expressed.
func setDeploymentEnv(t *testing.T, clientID, clientSecret string) {
	t.Helper()
	t.Setenv("TF_LINEAR_CLIENT_ID", clientID)
	t.Setenv("TF_LINEAR_CLIENT_SECRET", clientSecret)
}

// TestDeploymentOAuthAppFromEnv_LocalModeReadsNothing: a distributed local
// binary can ship no shared client secret, so local mode returns the zero app
// with both vars set.
func TestDeploymentOAuthAppFromEnv_LocalModeReadsNothing(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	setDeploymentEnv(t, "dep-client", "dep-secret")
	if app := DeploymentOAuthAppFromEnv(); app != (OAuthApp{}) {
		t.Errorf("DeploymentOAuthAppFromEnv = %+v, want the zero app", app)
	}
}

// TestDeploymentOAuthAppFromEnv_MultiReadsAndTrims: multi mode reads the pair,
// and a stray newline or indent in .env does not reach the credential.
func TestDeploymentOAuthAppFromEnv_MultiReadsAndTrims(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	setDeploymentEnv(t, "  dep-client\n", "\tdep-secret\n")
	want := OAuthApp{ClientID: "dep-client", ClientSecret: "dep-secret"}
	if app := DeploymentOAuthAppFromEnv(); app != want {
		t.Errorf("DeploymentOAuthAppFromEnv = %+v, want %+v", app, want)
	}
}

// TestDeploymentOAuthAppFromEnv_MultiUnconfigured: neither var set is a
// deployment whose orgs bring their own apps, and nothing is logged.
func TestDeploymentOAuthAppFromEnv_MultiUnconfigured(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	setDeploymentEnv(t, "", " ")
	var logs bytes.Buffer
	t.Cleanup(logging.SetOutput(&logs))

	if app := DeploymentOAuthAppFromEnv(); app != (OAuthApp{}) {
		t.Errorf("DeploymentOAuthAppFromEnv = %+v, want the zero app", app)
	}
	if logs.Len() != 0 {
		t.Errorf("logged %q, want nothing", logs.String())
	}
}

// TestDeploymentOAuthAppFromEnv_HalfConfiguredIsLoggedAndNone: one var without
// the other is the state in which an operator believes the deployment app is
// set up and it is not. The boot log says which var is missing, never the
// value of the one that is set, and the deployment runs with no deployment app.
func TestDeploymentOAuthAppFromEnv_HalfConfiguredIsLoggedAndNone(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	for _, tc := range []struct {
		clientID, clientSecret, missing, present string
	}{
		{clientID: "dep-client", missing: "TF_LINEAR_CLIENT_SECRET", present: "dep-client"},
		{clientSecret: "dep-secret", missing: "TF_LINEAR_CLIENT_ID", present: "dep-secret"},
	} {
		t.Run(tc.missing, func(t *testing.T) {
			setDeploymentEnv(t, tc.clientID, tc.clientSecret)
			var logs bytes.Buffer
			t.Cleanup(logging.SetOutput(&logs))

			if app := DeploymentOAuthAppFromEnv(); app != (OAuthApp{}) {
				t.Errorf("DeploymentOAuthAppFromEnv = %+v, want the zero app", app)
			}
			// The text and JSON handlers spell the attribute differently.
			line := logs.String()
			named := strings.Contains(line, "not_set="+tc.missing) || strings.Contains(line, `"not_set":"`+tc.missing+`"`)
			if !strings.Contains(line, "ERROR") || !named {
				t.Errorf("logged %q, want an error naming %s", line, tc.missing)
			}
			if strings.Contains(line, tc.present) {
				t.Errorf("logged %q, which carries the value that is set", line)
			}
		})
	}
}

// TestResolveClient: a token another app minted is answered with that app
// while the org can still authenticate as it — its own app or the deployment
// app, whichever Resolve would pick now — and with ErrNoLinearOAuthApp once
// its secret is gone.
func TestResolveClient(t *testing.T) {
	deployment := OAuthApp{ClientID: "dep-client", ClientSecret: "dep-secret"}
	withOrgApp := NewOAuthAppResolver(
		&fakeLinearApps{app: &domain.OrgLinearApp{ClientID: "org-client", ClientSecretRef: linearSecretRef}},
		&fakeSecrets{sys: map[string]string{linearSecretRef: "org-secret"}},
		deployment)
	for _, tc := range []struct {
		clientID string
		want     OAuthApp
	}{
		{"org-client", OAuthApp{ClientID: "org-client", ClientSecret: "org-secret"}},
		// The deployment app still authenticates the tokens it minted after
		// the org registered its own.
		{"dep-client", deployment},
	} {
		got, err := withOrgApp.ResolveClient(context.Background(), testOrgID, tc.clientID)
		if err != nil || got != tc.want {
			t.Errorf("ResolveClient(%s) = (%+v, %v), want %+v", tc.clientID, got, err, tc.want)
		}
	}
	for _, clientID := range []string{"replaced-client", ""} {
		if _, err := withOrgApp.ResolveClient(context.Background(), testOrgID, clientID); !errors.Is(err, ErrNoLinearOAuthApp) {
			t.Errorf("ResolveClient(%q) err = %v, want ErrNoLinearOAuthApp", clientID, err)
		}
	}

	// A row whose secret is gone authenticates nothing.
	noSecret := NewOAuthAppResolver(
		&fakeLinearApps{app: &domain.OrgLinearApp{ClientID: "org-client", ClientSecretRef: linearSecretRef}},
		&fakeSecrets{}, OAuthApp{})
	if _, err := noSecret.ResolveClient(context.Background(), testOrgID, "org-client"); !errors.Is(err, ErrNoLinearOAuthApp) {
		t.Errorf("ResolveClient with the secret gone err = %v, want ErrNoLinearOAuthApp", err)
	}

	sentinel := errors.New("store down")
	broken := NewOAuthAppResolver(&fakeLinearApps{err: sentinel}, &fakeSecrets{}, deployment)
	if _, err := broken.ResolveClient(context.Background(), testOrgID, "dep-client"); !errors.Is(err, sentinel) {
		t.Errorf("ResolveClient over a failed read err = %v, want the read error", err)
	}
}
