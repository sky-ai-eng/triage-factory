package linear

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
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

// TestDeploymentOAuthAppFromEnv_NoneYet pins that no deployment app resolves
// in this build, whatever the environment says: an org installs only through
// an app of its own.
func TestDeploymentOAuthAppFromEnv_NoneYet(t *testing.T) {
	t.Setenv("TF_LINEAR_CLIENT_ID", "dep-client")
	t.Setenv("TF_LINEAR_CLIENT_SECRET", "dep-secret")
	if app := DeploymentOAuthAppFromEnv(); app.Configured() {
		t.Errorf("DeploymentOAuthAppFromEnv = %+v, want the zero app", app)
	}
}
